// Package output writes results in the format the caller asked for.
//
// Three rules hold everything here together:
//
// The format does not change because a pipe is attached. A command whose
// output shape depends on whether stdout is a terminal is a command that
// breaks in CI for a reason nobody can see in the diff — so `table` is the
// default everywhere and a script passes --format json and says what it
// meant.
//
// --format json is a contract. Field names are the API's field names,
// unabbreviated, and nothing is dropped because it was empty: a key that
// appears only when it has a value is a key every script has to test for.
//
// Progress, colour and anything conversational goes to stderr. `vallic db
// export > dump.sql` has to produce a dump, not a dump with a spinner in it.
package output

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/vallic/vallic-cli/internal/terminal"
)

// Format is how results are written.
type Format string

const (
	// FormatTable is columns for a person.
	FormatTable Format = "table"

	// FormatJSON is the contract; see the package comment.
	FormatJSON Format = "json"

	// FormatYAML is the same data, for eyes that prefer it. Emitted by hand
	// because the shapes here are flat, and a dependency for that would be a
	// dependency for that.
	FormatYAML Format = "yaml"

	// FormatCSV is for a spreadsheet, and carries only what a table carries.
	FormatCSV Format = "csv"
)

// Printer writes results and messages.
type Printer struct {
	out    io.Writer
	err    io.Writer
	format Format

	// Quiet suppresses everything conversational. Results still print: a
	// --quiet that printed nothing would be a command with no output and no
	// way to tell it apart from one that did nothing.
	Quiet bool

	// colour is whether stderr may carry escape codes.
	colour bool
}

// NewPrinter builds a printer for a named format.
//
// An empty name is `table`. An unknown one is an error rather than a
// fallback: a script that typoed --format jsonl wants to hear about it, not
// to get a table it will then fail to parse.
func NewPrinter(out, errOut io.Writer, format string) (*Printer, error) {
	if format == "" {
		format = string(FormatTable)
	}

	f := Format(strings.ToLower(format))

	switch f {
	case FormatTable, FormatJSON, FormatYAML, FormatCSV:
	default:
		return nil, fmt.Errorf("unknown format %q: use table, json, yaml or csv", format)
	}

	return &Printer{
		out:    out,
		err:    errOut,
		format: f,
		colour: colourAllowed(errOut),
	}, nil
}

// Format is what this printer emits.
func (p *Printer) Format() Format { return p.format }

// Structured reports whether the output is for a machine.
//
// Commands use this to decide whether to print a human sentence at all, not
// to decide what the data looks like.
func (p *Printer) Structured() bool {
	return p.format != FormatTable
}

// Table is a set of rows, with the columns to show in a table.
type Table struct {
	// Columns are the headers, in order.
	Columns []string

	// Rows are the cells, in the same order as Columns.
	Rows [][]string

	// Empty is what to say on stderr when there is nothing. A table with no
	// rows and no explanation is indistinguishable from a broken command.
	Empty string
}

// Print writes a table, or the data behind it in a structured format.
//
// The `data` argument is what json, yaml and csv render — the API's own
// structs, with the API's own field names. The table's cells are a
// presentation of the same thing and are never what a machine reads, which
// is why a column may be abbreviated and a JSON key may not.
func (p *Printer) Print(t Table, data any) error {
	switch p.format {
	case FormatJSON:
		return p.json(data)
	case FormatYAML:
		return p.yaml(data)
	case FormatCSV:
		return p.csv(t)
	default:
		return p.table(t)
	}
}

// Value writes a single structured value, for a command with no table shape.
func (p *Printer) Value(data any) error {
	switch p.format {
	case FormatYAML:
		return p.yaml(data)
	default:
		return p.json(data)
	}
}

// Line writes one line of result to stdout, unformatted.
//
// For commands whose whole answer is a string — `vallic url`, `vallic env
// info --format table` on one field. It goes to stdout because it is the
// result, and the result is what a `$(…)` captures.
func (p *Printer) Line(format string, args ...any) {
	fmt.Fprintf(p.out, format+"\n", args...)
}

// Say writes a conversational line to stderr.
func (p *Printer) Say(format string, args ...any) {
	if p.Quiet {
		return
	}

	fmt.Fprintf(p.err, format+"\n", args...)
}

// Warn writes a warning to stderr. Not suppressed by --quiet: something the
// person should know is not something they asked to hide.
func (p *Printer) Warn(format string, args ...any) {
	fmt.Fprintf(p.err, p.paint("33", "! ")+format+"\n", args...)
}

// Good writes a success line to stderr.
func (p *Printer) Good(format string, args ...any) {
	if p.Quiet {
		return
	}

	fmt.Fprintf(p.err, p.paint("32", "✓ ")+format+"\n", args...)
}

// paint wraps text in an ANSI colour, or does not.
func (p *Printer) paint(code, text string) string {
	if !p.colour {
		return text
	}

	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (p *Printer) table(t Table) error {
	if len(t.Rows) == 0 {
		if t.Empty != "" {
			p.Say("%s", t.Empty)
		}

		return nil
	}

	w := tabwriter.NewWriter(p.out, 0, 8, 2, ' ', 0)

	if len(t.Columns) > 0 {
		fmt.Fprintln(w, strings.Join(upper(t.Columns), "\t"))
	}

	for _, row := range t.Rows {
		fmt.Fprintln(w, strings.Join(row, "\t"))
	}

	return w.Flush()
}

func (p *Printer) json(data any) error {
	enc := json.NewEncoder(p.out)
	enc.SetIndent("", "  ")
	// Off, because a URL with a query string is a normal value here and
	// `\u0026` in place of `&` is a value somebody has to un-escape by hand.
	enc.SetEscapeHTML(false)

	return enc.Encode(data)
}

func (p *Printer) csv(t Table) error {
	w := csv.NewWriter(p.out)

	if len(t.Columns) > 0 {
		if err := w.Write(t.Columns); err != nil {
			return err
		}
	}

	for _, row := range t.Rows {
		if err := w.Write(row); err != nil {
			return err
		}
	}

	w.Flush()

	return w.Error()
}

// yaml emits YAML for the flat and nested-map shapes this CLI produces.
//
// Written here rather than depended on. The data is JSON-shaped — the API's
// own responses — so it is round-tripped through encoding/json to get maps
// and slices, and printed. What this deliberately does not attempt is
// anything with an anchor, a tag or a multi-document stream, none of which a
// list of environments needs.
func (p *Printer) yaml(data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}

	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}

	var b strings.Builder
	writeYAML(&b, value, 0, false)

	_, err = io.WriteString(p.out, b.String())

	return err
}

func writeYAML(b *strings.Builder, value any, indent int, inList bool) {
	pad := strings.Repeat("  ", indent)

	switch v := value.(type) {
	case map[string]any:
		if len(v) == 0 {
			b.WriteString("{}\n")
			return
		}

		// Sorted, so two runs of the same command produce the same bytes.
		// Go randomises map order and a diff that changes every run is a
		// diff nobody can use.
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		for i, k := range keys {
			prefix := pad
			if inList && i == 0 {
				// The first key of a map in a list shares the dash's line.
				prefix = ""
			}

			switch child := v[k].(type) {
			case map[string]any, []any:
				fmt.Fprintf(b, "%s%s:\n", prefix, k)
				writeYAML(b, child, indent+1, false)
			default:
				fmt.Fprintf(b, "%s%s: %s\n", prefix, k, scalarYAML(child))
			}
		}
	case []any:
		if len(v) == 0 {
			b.WriteString("[]\n")
			return
		}

		for _, item := range v {
			switch child := item.(type) {
			case map[string]any:
				b.WriteString(pad + "- ")
				writeYAML(b, child, indent+1, true)
			case []any:
				b.WriteString(pad + "-\n")
				writeYAML(b, child, indent+1, false)
			default:
				fmt.Fprintf(b, "%s- %s\n", pad, scalarYAML(child))
			}
		}
	default:
		fmt.Fprintf(b, "%s%s\n", pad, scalarYAML(v))
	}
}

// scalarYAML renders one value, quoting what would otherwise change meaning.
func scalarYAML(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(v)
	case float64:
		// JSON has one number type. Printed as an integer where it is one,
		// because `"id": 12` should not come out as `id: 12.0`.
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}

		return strconv.FormatFloat(v, 'f', -1, 64)
	case string:
		return quoteYAML(v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// quoteYAML quotes a string where leaving it bare would change what it means.
//
// The cases that matter for this data: something that reads as a number, a
// bool or null; something empty; something with a leading or trailing space;
// and anything carrying a character YAML gives structure to. `main` stays
// bare, `2231` and `yes` and `*` do not.
func quoteYAML(s string) string {
	if s == "" {
		return `""`
	}

	if s != strings.TrimSpace(s) {
		return strconv.Quote(s)
	}

	if strings.ContainsAny(s, ":#{}[]&*!|>'\"%@`,\n\t") {
		return strconv.Quote(s)
	}

	switch strings.ToLower(s) {
	case "true", "false", "yes", "no", "on", "off", "null", "~":
		return strconv.Quote(s)
	}

	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return strconv.Quote(s)
	}

	return s
}

func upper(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToUpper(s)
	}

	return out
}

// colourAllowed reports whether escape codes may be written.
//
// NO_COLOR is honoured whatever its value, which is what no-color.org asks
// for. Otherwise: only to a terminal.
func colourAllowed(w io.Writer) bool {
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}

	if os.Getenv("TERM") == "dumb" {
		return false
	}

	file, ok := w.(*os.File)
	if !ok {
		return false
	}

	return terminal.IsTerminal(file)
}
