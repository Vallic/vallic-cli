package cli

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vallic/vallic-cli/internal/output"
	vssh "github.com/vallic/vallic-cli/internal/ssh"
	"github.com/vallic/vallic-cli/internal/terminal"
)

// dbDumpCommand writes a database dump to a file it names itself.
//
// Upsun's `db:dump`, which is the command people coming from it type first:
// the file is named after the project and the environment, so a dump needs
// nothing but the environment, and two dumps of different environments never
// land on the same name. `db export` is still the stream, for a pipeline.
func dbDumpCommand() *Command {
	var (
		identity  string
		dryRun    bool
		file      string
		directory string
		gzipped   bool
		stamped   bool
		stdout    bool
		part      dumpPart
	)

	return &Command{
		Name:    "dump",
		Summary: "write a dump of the database to a file named for it",
		Usage:   "db dump [<env>] [--file <path>] [--directory <dir>] [--gzip] [--timestamp] [--stdout] [--table <t>]... [--exclude-table <t>]... [--schema-only]",
		Long: `Writes the database to a file in the working directory, named after the
project and the environment, and says where:

    vallic db dump production
    ✓ Wrote acme--production--dump.sql (412 MB)

--gzip (-z) compresses it and names it .gz, and a --file ending in .gz is
compressed without being asked. --timestamp (-t) puts the time of the dump in
the name, in UTC, so a nightly dump never replaces the last one. --directory
(-d) puts the named file somewhere else, and --file (-f) names it outright.

The file appears only once the dump is complete: it is written beside its
name and moved there at the end, so a dump that fails half way leaves nothing
that looks like a good one. A file already there is replaced only when you
confirm it.

--stdout (-o) writes to standard output instead, which is what db export does.

Part of the database, as Upsun's db:dump takes it:

    vallic db dump production --table users --table orders
    vallic db dump production --exclude-table cache_render,sessions
    vallic db dump production --schema-only

Each flag takes a table, or several separated by commas, and can be repeated;
a table in another PostgreSQL schema is schema.table. The name the file gets
says which part it holds. Names are exact: there are no wildcards, because
mysqldump has none.

What runs on the far side is the same db-export verb: the engine's own client
in the container, with credentials that never reach this machine.`,
		Flags: func(fs *flag.FlagSet) {
			identityFlag(fs, &identity)
			fs.BoolVar(&dryRun, "dry-run", false, "print the command instead of running it")
			fs.StringVar(&file, "file", "", "the file to write, instead of the name chosen for it")
			fs.StringVar(&file, "f", "", "the file to write")
			fs.StringVar(&directory, "directory", "", "the directory the named file goes in")
			fs.StringVar(&directory, "d", "", "the directory the named file goes in")
			fs.BoolVar(&gzipped, "gzip", false, "compress the dump")
			fs.BoolVar(&gzipped, "z", false, "compress the dump")
			fs.BoolVar(&stamped, "timestamp", false, "put the time of the dump in its name")
			fs.BoolVar(&stamped, "t", false, "put the time of the dump in its name")
			fs.BoolVar(&stdout, "stdout", false, "write to standard output instead of a file")
			fs.BoolVar(&stdout, "o", false, "write to standard output instead of a file")
			part.flags(fs)
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			if err := ensureNoExtra(nil, args, 1); err != nil {
				return err
			}

			if stdout && (file != "" || directory != "" || stamped) {
				return Usagef(nil, "--stdout writes no file, so it takes no --file, --directory or --timestamp")
			}

			if file != "" && directory != "" {
				return Usagef(nil, "--file names the whole path; --directory is for the name chosen when there is no --file")
			}

			if err := part.check(); err != nil {
				return err
			}

			target, resolved, err := env.sshTargetFor(ctx, first(args), identity)
			if err != nil {
				return err
			}

			verb, err := part.verb(target)
			if err != nil {
				return err
			}

			target.Compress = true
			process := target.Verb(verb, false)

			if stdout {
				return streamDump(env, process, gzipped, dryRun, target.Host)
			}

			path := file
			if path == "" {
				path = filepath.Join(directory, dumpFilename(resolved, part, gzipped, stamped, time.Now()))
			}

			if strings.HasSuffix(path, ".gz") {
				gzipped = true
			}

			if dryRun {
				if gzipped {
					env.Printer.Line("%s | gzip > %s", vssh.Describe(process), vssh.Quote(path))
				} else {
					env.Printer.Line("%s > %s", vssh.Describe(process), vssh.Quote(path))
				}

				return nil
			}

			return writeDump(env, process, path, gzipped, resolved.Named())
		},
	}
}

// dumpFilename is the name a dump is written under when none is given.
//
// Upsun's shape, `<project>--<environment>--dump.sql`, with the double dash
// that keeps a hyphenated project apart from its environment, and the part of
// the database after them as Upsun names it: the tables, `excl-` and the
// tables left out, `schema`. The time, when asked for, is UTC and to the
// second, so names sort in the order the dumps were taken.
func dumpFilename(target *Target, part dumpPart, gzipped, stamped bool, now time.Time) string {
	var parts []string

	if target != nil && target.Project != nil && target.Project.MachineName != "" {
		parts = append(parts, target.Project.MachineName)
	}

	if target != nil && target.Environment != nil {
		parts = append(parts, firstNonEmpty(target.Environment.Name, target.Environment.Slug))
	}

	if len(part.tables) > 0 {
		parts = append(parts, strings.Join(part.tables, ","))
	}

	if len(part.excluded) > 0 {
		parts = append(parts, "excl-"+strings.Join(part.excluded, ","))
	}

	if part.schemaOnly {
		parts = append(parts, "schema")
	}

	if stamped {
		parts = append(parts, now.UTC().Format("20060102-150405")+"-UTC")
	}

	name := strings.Join(append(parts, "dump.sql"), "--")

	if gzipped {
		name += ".gz"
	}

	// A name comes from the platform, and a slash in it would be a directory
	// nobody asked for.
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 {
			return '-'
		}

		return r
	}, name)
}

// dumpPart is which part of the database a dump takes: every table unless
// told otherwise, and their rows unless --schema-only.
type dumpPart struct {
	tables     tableList
	excluded   tableList
	schemaOnly bool
}

func (p *dumpPart) flags(fs *flag.FlagSet) {
	fs.Var(&p.tables, "table", "dump only this table; repeat it, or separate several with commas")
	fs.Var(&p.excluded, "exclude-table", "leave this table out; repeat it, or separate several with commas")
	fs.BoolVar(&p.schemaOnly, "schema-only", false, "the tables' definitions without their rows")
}

// The names db-export takes, as the agent checks them: letters, digits, `_`
// and `-`, with one `.` before a table in another schema. Checked here too so
// a typo is said before anything connects.
var dumpTableName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]*(\.[A-Za-z0-9_][A-Za-z0-9_-]*)?$`)

func (p *dumpPart) check() error {
	for _, table := range append(slices.Clone(p.tables), p.excluded...) {
		if !dumpTableName.MatchString(table) {
			return Usagef(nil, "%q is not a table name a dump takes: letters, digits, _ and -, and schema.table for another schema; there are no wildcards", table)
		}
	}

	for _, table := range p.tables {
		if slices.Contains(p.excluded, table) {
			return Usagef(nil, "%s is both the table to dump and one to leave out", table)
		}
	}

	return nil
}

// verb is the export to ask for: the control plane's verb, and the options
// after it where the environment takes them.
//
// What it takes is the control plane's to say. A machine whose agent reads no
// options would run `db-export --table=users` as a command and answer "not
// found", so an option it does not list is refused here, in a sentence.
func (p *dumpPart) verb(target *vssh.Target) (string, error) {
	if target.DBExport == "" {
		return "", fmt.Errorf("this environment runs no database")
	}

	words := []string{target.DBExport}

	ask := func(option string, values ...string) error {
		if !slices.Contains(target.DBExportOptions, option) {
			return fmt.Errorf(
				"this environment does not offer %s yet: its machines' agent, or the control plane, is from before it\n"+
					"  the whole database: leave out --table, --exclude-table and --schema-only",
				option,
			)
		}

		if len(values) == 0 {
			words = append(words, option)
		}

		for _, value := range values {
			words = append(words, option+"="+value)
		}

		return nil
	}

	if p.schemaOnly {
		if err := ask("--schema-only"); err != nil {
			return "", err
		}
	}

	if len(p.tables) > 0 {
		if err := ask("--table", p.tables...); err != nil {
			return "", err
		}
	}

	if len(p.excluded) > 0 {
		if err := ask("--exclude-table", p.excluded...); err != nil {
			return "", err
		}
	}

	return strings.Join(words, " "), nil
}

// tableList is a flag given more than once, or with commas, or both:
// `--table users --table orders` and `--table users,orders` are one list.
type tableList []string

func (l *tableList) String() string {
	return strings.Join(*l, ",")
}

func (l *tableList) Set(value string) error {
	for _, table := range strings.Split(value, ",") {
		if table = strings.TrimSpace(table); table != "" && !slices.Contains(*l, table) {
			*l = append(*l, table)
		}
	}

	return nil
}

// writeDump runs the dump into a file, which appears only once it is whole.
func writeDump(env *Env, process *exec.Cmd, path string, gzipped bool, named string) error {
	if _, err := os.Stat(path); err == nil {
		// Asked rather than refused, as Upsun asks: replacing yesterday's
		// dump with today's is the ordinary case. With nobody to ask, kept.
		if !env.confirm(fmt.Sprintf("%s is already here. Replace it?", path)) {
			return fmt.Errorf("%s is already here\n  keep both: --timestamp, or name another with --file", path)
		}
	}

	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}

	// Beside the final name, so the rename at the end is on one filesystem
	// and cannot fail half way.
	partial, err := os.CreateTemp(dir, "."+base+".*.partial")
	if err != nil {
		return fmt.Errorf("cannot write %s: %w", path, err)
	}

	finished := false

	defer func() {
		if !finished {
			partial.Close()
			os.Remove(partial.Name())
		}
	}()

	var (
		out        io.Writer = partial
		compressor *gzip.Writer
	)

	if gzipped {
		compressor = gzip.NewWriter(partial)
		out = compressor
	}

	buffered := bufio.NewWriterSize(out, 1<<20)
	process.Stdout = buffered

	// To stderr, as every progress line is.
	env.Printer.Say("Dumping the database of %s to %s…", named, path)

	if err := runOrPrint(env, process, false); err != nil {
		return err
	}

	if err := buffered.Flush(); err != nil {
		return fmt.Errorf("cannot write %s: %w", path, err)
	}

	if compressor != nil {
		if err := compressor.Close(); err != nil {
			return fmt.Errorf("cannot write %s: %w", path, err)
		}
	}

	if err := partial.Close(); err != nil {
		return fmt.Errorf("cannot write %s: %w", path, err)
	}

	if err := os.Rename(partial.Name(), path); err != nil {
		return fmt.Errorf("cannot write %s: %w", path, err)
	}

	finished = true

	size := ""
	if info, err := os.Stat(path); err == nil {
		size = " (" + humanBytes(info.Size()) + ")"
	}

	if env.Printer.Structured() {
		return env.Printer.Value(map[string]any{"dump": map[string]any{
			"path":       path,
			"compressed": gzipped,
		}})
	}

	env.Printer.Good("Wrote %s%s", path, size)

	return nil
}

// streamDump runs the dump onto stdout, compressed where asked.
//
// Refused onto a terminal: a dump is megabytes of SQL that scroll past and
// are gone, which is never what somebody typing it meant. The fix is one of
// two words, and the message says both.
func streamDump(env *Env, process *exec.Cmd, gzipped, dryRun bool, host string) error {
	if !dryRun && env.stdoutIsTerminal() {
		return Usagef(nil,
			"standard output is a terminal, and a dump would be printed into it\n"+
				"  redirect it: vallic db export > dump.sql\n"+
				"  or let it name a file: vallic db dump")
	}

	if dryRun {
		if gzipped {
			env.Printer.Line("%s | gzip", vssh.Describe(process))
		} else {
			env.Printer.Line("%s", vssh.Describe(process))
		}

		return nil
	}

	// To stderr, because stdout is the dump. A progress line in the middle
	// of a redirect would corrupt the file.
	env.Printer.Say("Streaming the database out of %s…", host)

	if !gzipped {
		return runOrPrint(env, process, false)
	}

	compressor := gzip.NewWriter(env.Out)
	process.Stdout = compressor

	if err := runOrPrint(env, process, false); err != nil {
		return err
	}

	return compressor.Close()
}

// openDump is what an import reads: a file, or standard input, decompressed
// where it is gzip.
//
// Told apart by the first two bytes rather than by a name, because standard
// input has none and a dump called .sql is sometimes compressed anyway. The
// engine's client would read gzip as a syntax error on line one.
func openDump(env *Env, file string) (reader io.Reader, described string, closer func(), err error) {
	source, described, closer := io.Reader(env.In), "standard input", func() {}

	if file != "" {
		opened, err := os.Open(file)
		if err != nil {
			return nil, "", nil, fmt.Errorf("cannot read %s: %w", file, err)
		}

		source, described, closer = opened, file, func() { opened.Close() }
	}

	buffered := bufio.NewReaderSize(source, 1<<20)

	magic, err := buffered.Peek(2)
	if err != nil && !errors.Is(err, io.EOF) {
		closer()

		return nil, "", nil, fmt.Errorf("cannot read %s: %w", described, err)
	}

	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		decompressed, err := gzip.NewReader(buffered)
		if err != nil {
			closer()

			return nil, "", nil, fmt.Errorf("%s looks compressed and is not gzip: %w", described, err)
		}

		return decompressed, described + ", decompressed", closer, nil
	}

	return buffered, described, closer, nil
}

// stdoutIsTerminal reports whether what this writes lands on a screen.
func (e *Env) stdoutIsTerminal() bool {
	file, ok := e.Out.(*os.File)

	return ok && terminal.IsTerminal(file)
}

// stdinIsTerminal reports whether stdin is somebody typing, whatever
// --no-input says: the question here is what the bytes are, not whether to
// ask anything.
func (e *Env) stdinIsTerminal() bool {
	file, ok := e.In.(*os.File)

	return ok && terminal.IsTerminal(file)
}

// humanBytes is a size as somebody reads one.
func humanBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// dbSizeCommand says how much the database holds, and where.
//
// Upsun's db:size, and one step further: the largest tables beside the total,
// because the question after "how big is it" is nearly always "what is it",
// and in a CMS the answer is usually a cache table.
func dbSizeCommand() *Command {
	var (
		identity string
		dryRun   bool
		all      bool
	)

	return &Command{
		Name:    "size",
		Summary: "show how much the database holds, and its largest tables",
		Usage:   "db size [<env>] [--all]",
		Long: `The database's size and its ten largest tables, data and indexes
together; --all lists every table.

On PostgreSQL the total is the database's own size on disk, catalogs and all,
so it is a little more than its tables add up to. MySQL and MariaDB have no
such figure, and there the total is the tables'.`,
		Flags: func(fs *flag.FlagSet) {
			identityFlag(fs, &identity)
			fs.BoolVar(&dryRun, "dry-run", false, "print the ssh command instead of running it")
			fs.BoolVar(&all, "all", false, "list every table, not the ten largest")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			if err := ensureNoExtra(nil, args, 1); err != nil {
				return err
			}

			target, resolved, err := env.sshTargetFor(ctx, first(args), identity)
			if err != nil {
				return err
			}

			if target.DBCLI == "" {
				return fmt.Errorf("this environment runs no database")
			}

			if target.DBSize == "" {
				return fmt.Errorf("this environment does not report its database's size yet: its machines' agent, or the control plane, is from before db size")
			}

			process := target.Verb(target.DBSize, false)

			if dryRun {
				env.Printer.Line("%s", vssh.Describe(process))

				return nil
			}

			var answer strings.Builder
			process.Stdout = &answer

			if err := runOrPrint(env, process, false); err != nil {
				return err
			}

			size, err := parseDatabaseSize(answer.String())
			if err != nil {
				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(map[string]any{"database": size})
			}

			env.Printer.Line("%s holds %s in %d tables.", resolved.Named(), humanBytes(size.Bytes), len(size.Tables))

			shown := size.Tables
			if !all && len(shown) > 10 {
				shown = shown[:10]
			}

			if len(shown) == 0 {
				return nil
			}

			table := output.Table{Columns: []string{"table", "size", "share"}}

			for _, t := range shown {
				share := "-"
				if size.Bytes > 0 {
					share = fmt.Sprintf("%.0f%%", 100*float64(t.Bytes)/float64(size.Bytes))
				}

				table.Rows = append(table.Rows, []string{t.Name, humanBytes(t.Bytes), share})
			}

			env.Printer.Line("")

			if err := env.Printer.Print(table, nil); err != nil {
				return err
			}

			if rest := len(size.Tables) - len(shown); rest > 0 {
				env.Printer.Say("  and %d more: --all lists them", rest)
			}

			return nil
		},
	}
}

// DatabaseSize is what db-size answered.
type DatabaseSize struct {
	Bytes  int64       `json:"bytes"`
	Tables []TableSize `json:"tables"`
}

// TableSize is one table's data and indexes.
type TableSize struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

// parseDatabaseSize reads db-size's answer: the total alone on the first
// line, then a table and its size on each line after, a tab between, largest
// first.
func parseDatabaseSize(answer string) (*DatabaseSize, error) {
	lines := strings.Split(strings.TrimSpace(answer), "\n")

	total, err := strconv.ParseInt(strings.TrimSpace(lines[0]), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("the database's size came back as %q, which this version cannot read", lines[0])
	}

	size := &DatabaseSize{Bytes: total, Tables: []TableSize{}}

	for _, line := range lines[1:] {
		name, bytes, found := strings.Cut(line, "\t")
		if !found {
			continue
		}

		n, err := strconv.ParseInt(strings.TrimSpace(bytes), 10, 64)
		if err != nil {
			continue
		}

		size.Tables = append(size.Tables, TableSize{Name: name, Bytes: n})
	}

	return size, nil
}
