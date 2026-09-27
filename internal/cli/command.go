// Package cli is the command tree and its dispatch.
//
// Hand-rolled on flag.FlagSet rather than built on a framework, which is the
// same rule agent/ follows and for the same reason: one static binary whose
// dependency tree a customer can audit in an afternoon, holding a credential
// that reaches their production site.
//
// What that costs is help text and completion, written here once. What it
// buys is that `go.mod` has no `require` block.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Command is one verb, or a noun holding verbs.
//
// A command has either Run or Children, never both. A noun with a Run would
// be a command whose behaviour depends on whether an argument happened to
// match a subcommand, which is how `vallic env production` becomes either a
// deploy or an error depending on spelling.
type Command struct {
	// Name is the single word this is invoked as.
	Name string

	// Aliases are other spellings. Used sparingly: six commands have one,
	// because a CLI where everything has a shortcut is a CLI where nothing
	// is guessable.
	Aliases []string

	// Summary is the one line in the parent's help. Lower case, no full
	// stop, starts with a verb.
	Summary string

	// Usage is the whole command and its arguments, without the binary name:
	// "deploy [<env>]", "team switch <name>". Ancestors included, because a
	// usage line somebody cannot paste is not worth printing.
	Usage string

	// Long is printed under the usage line by `--help`. Optional, and worth
	// having wherever the summary cannot carry a caveat somebody needs.
	Long string

	// Flags registers this command's own flags. Called once per invocation,
	// before parsing.
	Flags func(fs *flag.FlagSet)

	// Run does the work. args are what is left after the flags.
	Run func(ctx context.Context, env *Env, args []string) error

	// Children are subcommands, for a noun.
	Children []*Command
}

// lookup finds a child by name or alias.
func (c *Command) lookup(name string) *Command {
	for _, child := range c.Children {
		if child.Name == name {
			return child
		}

		for _, alias := range child.Aliases {
			if alias == name {
				return child
			}
		}
	}

	return nil
}

// UsageError is a command invoked wrongly: an unknown flag, a missing
// argument, an ambiguous target.
//
// Its own type because it is the difference between exit code 2 and exit
// code 1, and a pipeline wants to tell "you typed it wrong" from "the
// platform said no".
type UsageError struct {
	// Command is the command that was misused, so its help can be printed
	// rather than the root's.
	Command *Command
	Err     error
}

func (e *UsageError) Error() string { return e.Err.Error() }
func (e *UsageError) Unwrap() error { return e.Err }

// Usagef builds a UsageError.
func Usagef(cmd *Command, format string, args ...any) error {
	return &UsageError{Command: cmd, Err: fmt.Errorf(format, args...)}
}

// Execute walks the command tree and runs what the arguments name.
func Execute(ctx context.Context, root *Command, env *Env, args []string) error {
	cmd := root
	path := []string{root.Name}

	// Walk while the next argument names a child. Stops at the first thing
	// that does not, which is either a flag or a positional argument for the
	// command reached — and stops on "--" so a passthrough command can be
	// handed a subcommand's own name without it being intercepted here.
	for len(cmd.Children) > 0 && len(args) > 0 {
		if args[0] == "--" || strings.HasPrefix(args[0], "-") {
			break
		}

		child := cmd.lookup(args[0])
		if child == nil {
			// Named rather than silently treated as an argument: a typo in a
			// subcommand is the single most common mistake, and a noun with
			// no Run has nothing to do with it anyway.
			return Usagef(cmd, "%s has no %q command", strings.Join(path, " "), args[0])
		}

		path = append(path, child.Name)
		cmd = child
		args = args[1:]
	}

	// Everything after a bare "--" is the command's to pass on untouched,
	// and is taken out before the flags are parsed. flag would swallow the
	// separator itself, leaving `vallic ssh -- drush status` and `vallic ssh
	// staging drush status` indistinguishable — and the first would then
	// treat `drush` as the name of an environment.
	args, passthrough, hasPassthrough := splitPassthrough(args)

	fs := flag.NewFlagSet(strings.Join(path, " "), flag.ContinueOnError)

	// Silenced so the error is reported once, by the top level, in the same
	// shape as every other error. flag's own output goes to stderr with its
	// own formatting and then the caller prints it again.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	help := fs.Bool("help", false, "show what this command does")
	fs.BoolVar(help, "h", false, "show what this command does")

	env.registerGlobals(fs)

	if cmd.Flags != nil {
		cmd.Flags(fs)
	}

	// Flags moved ahead of positional arguments, because flag.Parse stops at
	// the first argument that is not one — so `vallic ssh production
	// --dry-run` would read --dry-run as a second positional and connect for
	// real. Every CLI people already use accepts flags after arguments, and
	// a --dry-run that is silently ignored is the worst possible way to
	// learn otherwise.
	if err := fs.Parse(permute(fs, args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			PrintHelp(env.Err, cmd, path)
			return nil
		}

		return &UsageError{Command: cmd, Err: err}
	}

	if *help {
		PrintHelp(env.Err, cmd, path)
		return nil
	}

	if cmd.Run == nil {
		// A noun invoked on its own. Its help is the answer, and it is not
		// an error — somebody typing `vallic env` is asking what env does.
		PrintHelp(env.Err, cmd, path)
		return nil
	}

	if err := env.applyGlobals(); err != nil {
		return err
	}

	env.passthrough = passthrough
	env.hasPassthrough = hasPassthrough

	// Remembered so a UsageError raised inside the closure below can print
	// this command's help rather than the root's. A Run closure cannot name
	// the command it belongs to without a second reference to it, and every
	// call site having to thread one through is a call site that will pass
	// nil — which is how `vallic team switch nope` came to print the whole
	// command tree instead of what switch takes.
	env.command = cmd
	env.path = path

	err := cmd.Run(ctx, env, fs.Args())

	// After the command, not before: whether a credential was needed is not
	// known until it has run, and loading one to find out would break the
	// commands that deliberately need none.
	warnAboutExpiry(env, time.Now())

	return err
}

// splitPassthrough separates the arguments at the first bare "--".
//
// found distinguishes "no separator" from "a separator with nothing after
// it": the second is somebody asking for a shell in the most explicit way
// available, and reading it as the first would search for an environment
// called nothing.
func splitPassthrough(args []string) (before, after []string, found bool) {
	for i, arg := range args {
		if arg == "--" {
			return args[:i], args[i+1:], true
		}
	}

	return args, nil, false
}

// permute reorders arguments so flags come first.
//
// A flag that takes a value carries the next argument with it, which is why
// this has to ask the FlagSet whether each one is a boolean rather than
// guessing. An unrecognised flag is passed through untouched so flag.Parse
// reports it, rather than being silently treated as a positional argument.
func permute(fs *flag.FlagSet, args []string) []string {
	var flags, positionals []string

	for i := 0; i < len(args); i++ {
		arg := args[i]

		// "-" alone is the stdin convention, not a flag.
		if len(arg) < 2 || arg[0] != '-' {
			positionals = append(positionals, arg)
			continue
		}

		flags = append(flags, arg)

		// An inline value needs nothing more taken with it.
		if strings.Contains(arg, "=") {
			continue
		}

		formal := fs.Lookup(strings.TrimLeft(arg, "-"))
		if formal == nil || isBoolFlag(formal) {
			continue
		}

		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}

	return append(flags, positionals...)
}

// isBoolFlag reports whether a flag stands alone.
//
// flag marks these by implementing IsBoolFlag on the value, which is the
// same hook flag.Parse itself uses to decide whether to consume the next
// argument.
func isBoolFlag(formal *flag.Flag) bool {
	boolean, ok := formal.Value.(interface{ IsBoolFlag() bool })

	return ok && boolean.IsBoolFlag()
}

// PrintHelp writes a command's help.
func PrintHelp(w io.Writer, cmd *Command, path []string) {
	name := strings.Join(path, " ")

	if cmd.Summary != "" {
		fmt.Fprintf(w, "%s\n\n", cmd.Summary)
	}

	usage := name
	if cmd.Usage != "" {
		// Usage carries the command and its arguments; only the binary name
		// is prepended. Joining the walked path as well printed "vallic team
		// team switch <name>".
		usage = path[0] + " " + cmd.Usage
	} else if len(cmd.Children) > 0 {
		usage += " <command>"
	}

	fmt.Fprintf(w, "Usage:\n  %s\n", strings.TrimSpace(usage))

	if cmd.Long != "" {
		fmt.Fprintf(w, "\n%s\n", strings.TrimSpace(cmd.Long))
	}

	if len(cmd.Children) > 0 {
		children := make([]*Command, len(cmd.Children))
		copy(children, cmd.Children)
		sort.Slice(children, func(i, j int) bool { return children[i].Name < children[j].Name })

		width := 0
		for _, child := range children {
			if len(child.Name) > width {
				width = len(child.Name)
			}
		}

		fmt.Fprintf(w, "\nCommands:\n")
		for _, child := range children {
			fmt.Fprintf(w, "  %-*s  %s\n", width, child.Name, child.Summary)
		}
	}

	// Only where there is a subcommand to run. A leaf invited somebody to
	// run `vallic activity log <command> --help`, which does not exist.
	if len(cmd.Children) > 0 {
		fmt.Fprintf(w, "\nRun %s <command> --help for one command.\n", name)
	}
}
