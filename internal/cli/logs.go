package cli

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/vallic/vallic-cli/internal/api"
	vssh "github.com/vallic/vallic-cli/internal/ssh"
)

// logSources are the kinds of line an environment's logs hold, as --source
// takes them.
var logSources = []string{"app", "container", "cron", "proxy", "proxy-error"}

// logsCommand reads an environment's logs off its machines, or follows them.
//
// The lines never pass through the control plane. Each machine keeps a week
// of what the environment's containers, cron jobs, application and proxy
// wrote; this logs in to the machine with the person's own SSH key, the way
// `vallic ssh` does, and the agent there hands over what is this
// environment's and nothing else. So it works for whoever has a shell, costs
// the control plane nothing, and a tail is as live as the machine is.
func logsCommand() *Command {
	var (
		identity string
		machine  string
		since    string
		until    string
		output   string
		tail     int
		follow   bool
		asJSON   bool
		where    bool
		dryRun   bool
		sources  kindList
	)

	return &Command{
		Name:    "logs",
		Summary: "read or follow an environment's logs",
		Usage:   "logs [<env>] [--follow] [--since <when>] [--tail <n>] [--source <kind>]... [--machine <name>] [--output <file>]",
		Long: `What the environment wrote on its machines: its containers' output, its cron
jobs, the files its application logs to, and the requests the proxy served for
its hostnames. Each machine keeps the last 7 days.

With nothing else it prints the last 100 lines. --follow keeps going, line by
line as they are written, until you stop it:

    vallic logs production --follow
    vallic logs production --since 1h --source proxy
    vallic logs staging --since 2026-11-13T10:00 --until 2026-11-13T10:30
    vallic logs production --since 24h --output production.log.gz

--since and --until take how long ago (15m, 2h, 3d) or a moment, in UTC.
--source takes app, container, cron, proxy or proxy-error, and can be given
more than once. An environment on several machines is read on all of them,
each line named for its machine; --machine reads one.

The lines come straight from the machine over your SSH key, as vallic ssh
does, and are not kept by Vallic Cloud. --where says where the project sends
them besides. What the platform did to the environment — deploys,
provisioning, backups — is a different log: vallic activity.`,
		Flags: func(fs *flag.FlagSet) {
			identityFlag(fs, &identity)
			fs.BoolVar(&follow, "follow", false, "keep printing lines as they are written")
			fs.BoolVar(&follow, "f", false, "keep printing lines as they are written")
			fs.StringVar(&since, "since", "", "from how long ago (15m, 2h, 3d) or a moment (2026-11-13T10:00)")
			fs.StringVar(&until, "until", "", "up to how long ago or a moment")
			fs.IntVar(&tail, "tail", -1, "only the last so many lines (default 100, or 10 with --follow)")
			fs.IntVar(&tail, "n", -1, "only the last so many lines")
			fs.Var(&sources, "source", "a kind of log: "+strings.Join(logSources, ", ")+" (repeatable)")
			fs.StringVar(&machine, "machine", "", "read one of the environment's machines instead of all of them")
			fs.StringVar(&output, "output", "", "write the lines to a file instead of printing them (.gz compresses)")
			fs.StringVar(&output, "o", "", "write the lines to a file instead of printing them")
			fs.BoolVar(&asJSON, "json", false, "one JSON object a line: time, source, name, message")
			fs.BoolVar(&where, "where", false, "say where the project sends its logs, instead of reading them")
			fs.BoolVar(&dryRun, "dry-run", false, "print the ssh command instead of running it")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			if err := ensureNoExtra(nil, args, 1); err != nil {
				return err
			}

			if where {
				return logsWhere(ctx, env, first(args))
			}

			request := logsRequest{
				Since:   since,
				Until:   until,
				Tail:    tail,
				Follow:  follow,
				JSON:    asJSON || env.Printer.Structured(),
				Sources: sources,
			}

			if err := request.check(output); err != nil {
				return err
			}

			target, err := env.sshTarget(ctx, first(args), identity)
			if err != nil {
				return err
			}

			// The verb the control plane named, as the database's are: empty
			// from a control plane that predates it, and where a machine's
			// agent would pass the word to a container as a command.
			if target.Logs == "" {
				return fmt.Errorf("this environment's machines cannot be read for logs yet: they update on their own, usually within the hour\n  where the project sends its logs meanwhile: vallic logs --where")
			}

			streams, err := logStreams(target, machine, request.verb(target.Logs))
			if err != nil {
				return err
			}

			if dryRun {
				for _, stream := range streams {
					env.Printer.Line("%s", vssh.Describe(stream.cmd))
				}

				return nil
			}

			if _, err := exec.LookPath("ssh"); err != nil {
				return fmt.Errorf("ssh is not installed, and this command needs it")
			}

			out, done, err := logsOutput(env, output)
			if err != nil {
				return err
			}

			lines, err := runLogStreams(ctx, streams, out, env.Err, request)

			if closeErr := done(); err == nil {
				err = closeErr
			}

			if err == nil && output != "" {
				env.Printer.Say("Wrote %d lines to %s.", lines, output)
			}

			return err
		},
	}
}

// kindList is a flag given more than once, or as a comma-separated list.
type kindList []string

func (l *kindList) String() string { return strings.Join(*l, ",") }

func (l *kindList) Set(value string) error {
	for _, kind := range strings.Split(value, ",") {
		if kind = strings.TrimSpace(kind); kind != "" && !slices.Contains(*l, kind) {
			*l = append(*l, kind)
		}
	}

	return nil
}

// logsRequest is what was asked for, before it is a command.
type logsRequest struct {
	Since   string
	Until   string
	Tail    int
	Follow  bool
	JSON    bool
	Sources []string
}

// check says what is wrong with a request before a connection is opened for
// it. The machine checks again; this is the answer that names the flag.
func (r logsRequest) check(output string) error {
	for _, source := range r.Sources {
		if !slices.Contains(logSources, source) {
			return Usagef(nil, "--source takes %s; not %q", strings.Join(logSources, ", "), source)
		}
	}

	for flag, value := range map[string]string{"--since": r.Since, "--until": r.Until} {
		// One word, because it travels as one: a moment with a space in it
		// is written with a T.
		if strings.ContainsAny(value, " \t'\"\\;|&$`<>()") {
			return Usagef(nil, "%s takes how long ago (15m, 2h, 3d) or a moment written as one word (2026-11-13T10:00), not %q", flag, value)
		}
	}

	if r.Follow && r.Until != "" {
		return Usagef(nil, "--follow has no end, so it does not go with --until")
	}

	if r.Follow && output != "" {
		return Usagef(nil, "--follow has no end, so there is no file to finish; redirect it instead: vallic logs --follow > file")
	}

	return nil
}

// verb is the request as the forced command on the machine reads it.
//
// Words only, each of them checked above or a number: the machine's side
// takes them apart on spaces and hands nothing to a shell.
func (r logsRequest) verb(name string) string {
	words := []string{name}

	if r.Since != "" {
		words = append(words, "--since", r.Since)
	}
	if r.Until != "" {
		words = append(words, "--until", r.Until)
	}

	tail := r.Tail
	if tail < 0 && r.Since == "" && r.Until == "" {
		// Nothing said about how much: the end of it, as `tail` gives.
		tail = 100
		if r.Follow {
			tail = 10
		}
	}
	if tail > 0 {
		words = append(words, "--tail", strconv.Itoa(tail))
	}

	if len(r.Sources) > 0 {
		words = append(words, "--source", strings.Join(r.Sources, ","))
	}
	if r.Follow {
		words = append(words, "--follow")
	}
	if r.JSON {
		words = append(words, "--json")
	}

	return strings.Join(words, " ")
}

// logStream is one machine being read.
type logStream struct {
	// name is the machine's, or empty where the environment has one.
	name string
	cmd  *exec.Cmd
}

// logStreams builds a connection to each machine the logs are on.
//
// An environment's logs are its machines': the requests on the one traffic
// reaches, a worker's output on the worker's. So all of them are read unless
// one was named.
func logStreams(target *vssh.Target, machine, verb string) ([]logStream, error) {
	if machine != "" {
		if err := target.ChooseMachine(machine); err != nil {
			return nil, err
		}

		return []logStream{{name: "", cmd: target.Verb(verb, false)}}, nil
	}

	if len(target.Machines) < 2 {
		return []logStream{{name: "", cmd: target.Verb(verb, false)}}, nil
	}

	streams := make([]logStream, 0, len(target.Machines))
	for _, m := range target.Machines {
		one := *target
		one.Machine = m.Name
		streams = append(streams, logStream{name: m.Name, cmd: one.Verb(verb, false)})
	}

	return streams, nil
}

// logsOutput is where the lines go: the terminal, or a file.
func logsOutput(env *Env, path string) (io.Writer, func() error, error) {
	if path == "" {
		return env.Out, func() error { return nil }, nil
	}

	file, err := os.Create(path)
	if err != nil {
		return nil, nil, fmt.Errorf("opening %s: %w", path, err)
	}

	if !strings.HasSuffix(path, ".gz") {
		return file, file.Close, nil
	}

	packed := gzip.NewWriter(file)

	return packed, func() error {
		if err := packed.Close(); err != nil {
			_ = file.Close()

			return err
		}

		return file.Close()
	}, nil
}

// runLogStreams reads every machine and writes its lines out, and says how
// many there were.
//
// One machine is copied through as it arrives. Several are put in order of
// time when the read has an end, since each machine's lines already are; a
// tail that is followed has no end to wait for, so its lines are written as
// each machine sends them.
func runLogStreams(ctx context.Context, streams []logStream, out, errOut io.Writer, request logsRequest) (int, error) {
	type running struct {
		logStream
		lines  chan string
		stderr *limitedBuffer
		failed error
	}

	all := make([]*running, 0, len(streams))

	for _, stream := range streams {
		one := &running{logStream: stream, lines: make(chan string, 256), stderr: &limitedBuffer{limit: 4 << 10}}

		stream.cmd.Stdin = nil
		stream.cmd.Stderr = one.stderr
		pipe, err := stream.cmd.StdoutPipe()
		if err != nil {
			return 0, err
		}
		if err := stream.cmd.Start(); err != nil {
			return 0, fmt.Errorf("starting ssh: %w", err)
		}

		go func() {
			defer close(one.lines)

			reader := bufio.NewReaderSize(pipe, 64<<10)
			for {
				line, err := reader.ReadString('\n')
				if line != "" {
					one.lines <- strings.TrimRight(line, "\r\n")
				}
				if err != nil {
					break
				}
			}

			one.failed = stream.cmd.Wait()
		}()

		all = append(all, one)
	}

	var (
		mu      sync.Mutex
		written int
		failure error
	)
	write := func(machine, line string) {
		mu.Lock()
		defer mu.Unlock()

		if failure != nil {
			return
		}
		if _, err := io.WriteString(out, named(machine, line, request.JSON)+"\n"); err != nil {
			failure = err

			return
		}
		written++
	}

	if request.Follow || len(all) == 1 {
		var wait sync.WaitGroup
		for _, one := range all {
			wait.Add(1)
			go func() {
				defer wait.Done()
				for line := range one.lines {
					write(one.name, line)
				}
			}()
		}
		wait.Wait()
	} else {
		// The earliest line any machine is holding, until none holds one.
		heads := make([]*string, len(all))
		next := func(i int) {
			if line, ok := <-all[i].lines; ok {
				heads[i] = &line
			} else {
				heads[i] = nil
			}
		}
		for i := range all {
			next(i)
		}
		for {
			first := -1
			for i, head := range heads {
				if head != nil && (first < 0 || stampOf(*head) < stampOf(*heads[first])) {
					first = i
				}
			}
			if first < 0 {
				break
			}
			write(all[first].name, *heads[first])
			next(first)
		}
	}

	// A file that could not be written, or a terminal that went away.
	if failure != nil && !errors.Is(failure, io.ErrClosedPipe) {
		return written, failure
	}

	var problems []string
	for _, one := range all {
		if problem := logsFailure(ctx, one.name, one.failed, one.stderr.String(), request.Follow); problem != "" {
			problems = append(problems, problem)
		}
	}

	if len(problems) == len(all) && len(problems) > 0 {
		return written, errors.New(strings.Join(problems, "\n"))
	}

	// Some machines answered: what they said is printed, and the ones that
	// did not are named beside it rather than failing the lot.
	for _, problem := range problems {
		fmt.Fprintln(errOut, "warning: "+problem)
	}

	return written, nil
}

// logsFailure turns how a connection ended into a sentence, or nothing where
// it ended well.
func logsFailure(ctx context.Context, machine string, err error, stderr string, follow bool) string {
	if err == nil {
		return ""
	}

	// A tail somebody stopped ends with the connection being cut, which is
	// how it was meant to end.
	if follow && ctx.Err() != nil {
		return ""
	}

	var exit *exec.ExitError
	if follow && errors.As(err, &exit) && (exit.ExitCode() == 130 || exit.ExitCode() == -1) {
		return ""
	}

	where := ""
	if machine != "" {
		where = machine + ": "
	}

	stderr = strings.TrimSpace(stderr)

	// A machine whose agent is older than this passes the word to the
	// container, which has never heard of it.
	if strings.Contains(stderr, "vallic-logs: not found") || strings.Contains(stderr, "vallic-logs: command not found") {
		return where + "this machine cannot be read for logs yet; it updates on its own, usually within the hour"
	}

	if stderr != "" {
		return where + strings.TrimPrefix(stderr, "vallic: ")
	}

	return where + err.Error()
}

// named puts the machine in front of a line, where there are several.
func named(machine, line string, asJSON bool) string {
	if machine == "" {
		return line
	}

	if asJSON {
		if rest, ok := strings.CutPrefix(line, "{"); ok {
			return `{"machine":` + strconv.Quote(machine) + "," + rest
		}

		return line
	}

	return machine + " | " + line
}

// stampOf is the time a line starts with, as text that sorts as time does.
//
// Both shapes the machine writes begin with it: `2026-11-13T10:00:00.000Z
// [container php] …`, and `{"time":"2026-11-13T10:00:00Z",…}`. A line with
// none sorts first, which keeps it where its machine put it.
func stampOf(line string) string {
	if rest, ok := strings.CutPrefix(line, `{"time":"`); ok {
		stamp, _, _ := strings.Cut(rest, `"`)

		return stamp
	}

	if strings.HasPrefix(line, "{") || strings.HasPrefix(line, "-") {
		return ""
	}

	stamp, _, _ := strings.Cut(line, " ")

	return stamp
}

// limitedBuffer keeps the start of what a connection said on its error
// stream, which is where a refusal is, and drops the rest.
type limitedBuffer struct {
	mu    sync.Mutex
	limit int
	text  strings.Builder
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if room := b.limit - b.text.Len(); room > 0 {
		if len(p) > room {
			b.text.Write(p[:room])
		} else {
			b.text.Write(p)
		}
	}

	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.text.String()
}

// logsWhere says where an environment's logs are sent.
//
// Besides the machine's own copy, which `vallic logs` reads: a project can
// send its logs to a collector of its own, and this names it.
func logsWhere(ctx context.Context, env *Env, positional string) error {
	target, err := env.ResolveEnvironment(ctx, positional)
	if err != nil {
		return err
	}

	client, err := env.Client()
	if err != nil {
		return err
	}

	answer, err := client.Logs(ctx, target.Environment.ID)

	var refusal *api.Error
	if errors.As(err, &refusal) && refusal.Status == http.StatusNotFound {
		// An older control plane. Said plainly: the console's
		// configuration page has the same answer.
		return fmt.Errorf("this control plane does not say where logs go yet\n  the project's configuration page in the console has it")
	}
	if err != nil {
		return err
	}

	if env.Printer.Structured() {
		return env.Printer.Value(answer)
	}

	for _, line := range logsReport(answer) {
		env.Printer.Line("%s", line)
	}

	return nil
}

// logsReport is what `vallic logs --where` prints, one line each.
func logsReport(answer *api.Logs) []string {
	lines := []string{fmt.Sprintf("Where %s's logs are:", answer.Environment)}

	if answer.MachineDays > 0 {
		lines = append(lines, fmt.Sprintf("  On the machine: the last %d days — read them with `vallic logs`, follow them with --follow.", answer.MachineDays))
	}

	switch {
	case answer.Destination != nil:
		lines = append(lines, fmt.Sprintf("  Sent to:        %s (%s)", answer.Destination.Name, answer.Destination.KindLabel))
	case answer.Unusable != nil:
		// The case worth the most words: somebody believes their logs go
		// somewhere, and they do not.
		lines = append(lines,
			fmt.Sprintf("  Sent to:        nowhere — %s (%s) is chosen, but it is switched off or", answer.Unusable.Name, answer.Unusable.KindLabel),
			"                  missing what it needs, so nothing reaches it.",
		)
	default:
		lines = append(lines, "  Sent to:        nowhere — this project has no log destination.")
	}

	if answer.SettingsURL != nil {
		lines = append(lines, fmt.Sprintf("  Change it:      %s", *answer.SettingsURL))
	}

	return append(lines, "", "What the platform did to it (deploys, provisioning, backups): vallic activity")
}
