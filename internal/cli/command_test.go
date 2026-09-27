package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"

	"github.com/vallic/vallic-cli/internal/api"
	"github.com/vallic/vallic-cli/internal/auth"
	"github.com/vallic/vallic-cli/internal/resolve"
)

// Argument permutation is the dispatch behaviour most likely to be wrong and
// least likely to be noticed: flag.Parse stops at the first argument that is
// not a flag, so `vallic ssh production --dry-run` read --dry-run as a second
// positional and connected for real. A flag silently ignored is the worst
// possible failure for --dry-run specifically.
func TestPermute(t *testing.T) {
	newSet := func() *flag.FlagSet {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.Bool("dry-run", false, "")
		fs.Bool("wait", false, "")
		fs.String("identity", "", "")
		fs.String("release", "", "")

		return fs
	}

	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "a bool flag after a positional moves ahead of it",
			in:   []string{"production", "--dry-run"},
			want: []string{"--dry-run", "production"},
		},
		{
			name: "a valued flag carries its value with it",
			in:   []string{"production", "--identity", "/key"},
			want: []string{"--identity", "/key", "production"},
		},
		{
			name: "an inline value is not given a second argument",
			in:   []string{"production", "--identity=/key", "staging"},
			want: []string{"--identity=/key", "production", "staging"},
		},
		{
			name: "single dash form is treated the same",
			in:   []string{"production", "-identity", "/key"},
			want: []string{"-identity", "/key", "production"},
		},
		{
			name: "two positionals keep their order",
			in:   []string{"production", "./files", "--dry-run"},
			want: []string{"--dry-run", "production", "./files"},
		},
		{
			// A lone "-" is the stdin convention, not a flag, and must not
			// swallow whatever follows it.
			name: "a bare dash is a positional",
			in:   []string{"-", "--wait"},
			want: []string{"--wait", "-"},
		},
		{
			// Left in place so flag.Parse reports it. Treating it as a
			// positional would make a typo look like an argument.
			name: "an unknown flag is left for flag.Parse to refuse",
			in:   []string{"production", "--nonsense"},
			want: []string{"--nonsense", "production"},
		},
		{
			// The value is missing, and permute must not run off the end.
			name: "a valued flag at the end does not over-read",
			in:   []string{"production", "--identity"},
			want: []string{"--identity", "production"},
		},
		{
			name: "already ordered is unchanged",
			in:   []string{"--wait", "production"},
			want: []string{"--wait", "production"},
		},
		{
			name: "nothing at all",
			in:   nil,
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := permute(newSet(), tc.in)

			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Errorf("permute(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The separator has to be taken out before the flags are parsed, because
// flag.Parse swallows it — which would make `vallic ssh -- drush status` and
// `vallic ssh staging drush status` indistinguishable, and the first would
// then search for an environment called "drush".
func TestSplitPassthrough(t *testing.T) {
	cases := []struct {
		name       string
		in         []string
		wantBefore []string
		wantAfter  []string
		wantFound  bool
	}{
		{
			name:       "no separator",
			in:         []string{"production", "--dry-run"},
			wantBefore: []string{"production", "--dry-run"},
			wantAfter:  nil,
			wantFound:  false,
		},
		{
			name:       "separator with a command",
			in:         []string{"production", "--", "drush", "status"},
			wantBefore: []string{"production"},
			wantAfter:  []string{"drush", "status"},
			wantFound:  true,
		},
		{
			name:       "separator with no environment named",
			in:         []string{"--", "drush", "status"},
			wantBefore: nil,
			wantAfter:  []string{"drush", "status"},
			wantFound:  true,
		},
		{
			// Found with nothing after it is somebody asking for a shell in
			// the most explicit way available. Reading it as "no separator"
			// would send the empty string off to be resolved as a name.
			name:       "separator with nothing after it",
			in:         []string{"production", "--"},
			wantBefore: []string{"production"},
			wantAfter:  []string{},
			wantFound:  true,
		},
		{
			// Only the first is ours. A second belongs to whatever is being
			// run on the far side.
			name:       "only the first separator splits",
			in:         []string{"--", "git", "log", "--", "path"},
			wantBefore: nil,
			wantAfter:  []string{"git", "log", "--", "path"},
			wantFound:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before, after, found := splitPassthrough(tc.in)

			if found != tc.wantFound {
				t.Errorf("found = %v, want %v", found, tc.wantFound)
			}

			if strings.Join(before, "\x00") != strings.Join(tc.wantBefore, "\x00") {
				t.Errorf("before = %q, want %q", before, tc.wantBefore)
			}

			if strings.Join(after, "\x00") != strings.Join(tc.wantAfter, "\x00") {
				t.Errorf("after = %q, want %q", after, tc.wantAfter)
			}
		})
	}
}

// Exit codes are a contract with CI. 4 in particular must not collapse into
// 1: a pipeline needs to tell "we would not deploy that" from "we deployed it
// and it broke" without parsing text.
func TestExitCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nothing went wrong", nil, ExitOK},
		{
			"a child process's own status passes through",
			&PassthroughError{Code: 42},
			42,
		},
		{
			"work that finished and failed",
			&TaskFailedError{Task: &api.Task{Type: "deploy", State: "failed"}},
			ExitTaskFailed,
		},
		{
			"a command invoked wrongly",
			&UsageError{Err: errors.New("unknown flag")},
			ExitUsage,
		},
		{
			"an unresolvable target is a usage problem, because the fix is an argument",
			&resolve.Ambiguous{What: "environment", Because: "no match"},
			ExitUsage,
		},
		{"no credential", auth.ErrNoCredential, ExitUnauthenticated},
		{
			"a credential the platform refused",
			&api.Error{Status: 401, Code: "unauthorized"},
			ExitUnauthenticated,
		},
		{
			"an OAuth failure is an authentication problem",
			&auth.OAuthError{Code: "invalid_grant"},
			ExitUnauthenticated,
		},
		{
			"the platform refused the action",
			&api.Error{Status: 409, Code: "environment_busy"},
			ExitRefused,
		},
		{
			"nothing answered",
			errors.New(`cannot reach https://console.vallic.com: dial tcp: refused`),
			ExitUnreachable,
		},
		{"anything else", errors.New("something went wrong"), ExitRefused},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCode(tc.err); got != tc.want {
				t.Errorf("ExitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// A wrapped error keeps its code. Commands add a sentence to a refusal —
// describeDeployFailure does exactly this — and the wrapping must not turn a
// 409 into an unknown failure.
func TestExitCodeSeesThroughWrapping(t *testing.T) {
	wrapped := errors.Join(
		errors.New("a deployment is already running"),
		&api.Error{Status: 409, Code: api.CodeEnvironmentBusy},
	)

	if got := ExitCode(wrapped); got != ExitRefused {
		t.Errorf("ExitCode(wrapped) = %d, want %d", got, ExitRefused)
	}

	if got := ExitCode(errors.Join(errors.New("context"), auth.ErrNoCredential)); got != ExitUnauthenticated {
		t.Errorf("a wrapped missing credential = %d, want %d", got, ExitUnauthenticated)
	}
}

// A child process has already explained itself on the stderr it inherited.
// Saying it again in the CLI's own words would be two errors for one failure.
func TestReportStaysQuietForAPassthrough(t *testing.T) {
	var out, errOut bytes.Buffer
	env := &Env{Out: &out, Err: &errOut}

	code := Report(env, Root(), &PassthroughError{Code: 3})

	if code != 3 {
		t.Errorf("code = %d, want 3", code)
	}

	if errOut.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", errOut.String())
	}
}

// An error goes to stderr whatever --format says: a JSON consumer reading
// stdout should find valid JSON or nothing, never an error object it has to
// tell apart from a result.
func TestReportWritesToStderrOnly(t *testing.T) {
	var out, errOut bytes.Buffer
	env := &Env{Out: &out, Err: &errOut}

	Report(env, Root(), &api.Error{Status: 409, Message: "already running"})

	if out.Len() != 0 {
		t.Errorf("stdout = %q, want nothing on it", out.String())
	}

	if !strings.Contains(errOut.String(), "already running") {
		t.Errorf("stderr = %q, want the message", errOut.String())
	}
}

// Every command in the tree must be reachable, uniquely named, and either a
// verb or a noun — never both. A noun with a Run would behave differently
// depending on whether an argument happened to match a subcommand.
func TestCommandTreeIsWellFormed(t *testing.T) {
	var walk func(cmd *Command, path string)

	walk = func(cmd *Command, path string) {
		if cmd.Run == nil && len(cmd.Children) == 0 {
			t.Errorf("%s does nothing and has no subcommands", path)
		}

		if cmd.Run != nil && len(cmd.Children) > 0 {
			t.Errorf("%s is both a verb and a noun", path)
		}

		if cmd.Summary == "" {
			t.Errorf("%s has no summary, so it is invisible in help", path)
		}

		seen := map[string]bool{}
		for _, child := range cmd.Children {
			for _, name := range append([]string{child.Name}, child.Aliases...) {
				if seen[name] {
					t.Errorf("%s has two children answering to %q", path, name)
				}
				seen[name] = true
			}

			walk(child, path+" "+child.Name)
		}
	}

	walk(Root(), "vallic")
}

// A typo in a subcommand is the most common mistake there is, and it must be
// named rather than quietly treated as an argument to the noun.
func TestUnknownSubcommandIsNamed(t *testing.T) {
	var out, errOut bytes.Buffer
	env := &Env{Out: &out, Err: &errOut}

	err := Execute(context.Background(), Root(), env, []string{"env", "lst"})

	var usage *UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error = %v, want a *UsageError", err)
	}

	if !strings.Contains(err.Error(), `"lst"`) {
		t.Errorf("error = %q, want the mistyped word in it", err)
	}
}

// A noun on its own is a question, not a mistake. Somebody typing `vallic env`
// is asking what env does.
func TestANounOnItsOwnPrintsHelp(t *testing.T) {
	var out, errOut bytes.Buffer
	env := &Env{Out: &out, Err: &errOut}

	if err := Execute(context.Background(), Root(), env, []string{"env"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(errOut.String(), "list") || !strings.Contains(errOut.String(), "info") {
		t.Errorf("help = %q, want the subcommands listed", errOut.String())
	}

	if out.Len() != 0 {
		t.Errorf("stdout = %q, want help on stderr", out.String())
	}
}

// The usage line has to be something somebody can paste. It was
// "vallic team team switch <name>", because the walked path was joined onto a
// Usage string that already carried its ancestors.
func TestHelpPrintsAPastableUsageLine(t *testing.T) {
	cases := []struct {
		path []string
		cmd  *Command
		want string
	}{
		{
			path: []string{"vallic", "deploy"},
			cmd:  &Command{Name: "deploy", Summary: "x", Usage: "deploy [<env>]"},
			want: "vallic deploy [<env>]",
		},
		{
			path: []string{"vallic", "team", "switch"},
			cmd:  &Command{Name: "switch", Summary: "x", Usage: "team switch <name>"},
			want: "vallic team switch <name>",
		},
		{
			// No Usage given: the walked path, plus <command> because it has
			// children to run.
			path: []string{"vallic", "team"},
			cmd: &Command{Name: "team", Summary: "x", Children: []*Command{
				{Name: "list", Summary: "y", Run: func(context.Context, *Env, []string) error { return nil }},
			}},
			want: "vallic team <command>",
		},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			var out bytes.Buffer
			PrintHelp(&out, tc.cmd, tc.path)

			if !strings.Contains(out.String(), "  "+tc.want+"\n") {
				t.Errorf("help did not carry %q:\n%s", tc.want, out.String())
			}
		})
	}
}

// A leaf invited somebody to run `vallic activity log <command> --help`, which
// does not exist. The line belongs only where there is a subcommand to run.
func TestHelpOffersSubcommandsOnlyWhenThereAreSome(t *testing.T) {
	var leaf bytes.Buffer
	PrintHelp(&leaf, &Command{
		Name:    "switch",
		Summary: "x",
		Usage:   "team switch <name>",
		Run:     func(context.Context, *Env, []string) error { return nil },
	}, []string{"vallic", "team", "switch"})

	if strings.Contains(leaf.String(), "--help for one command") {
		t.Errorf("a leaf offered subcommands:\n%s", leaf.String())
	}

	var noun bytes.Buffer
	PrintHelp(&noun, Root().lookup("team"), []string{"vallic", "team"})

	if !strings.Contains(noun.String(), "Run vallic team <command> --help") {
		t.Errorf("a noun did not offer its subcommands:\n%s", noun.String())
	}
}

// A usage error raised inside a Run closure points at the command that raised
// it, not at the root. Without this, `vallic team switch nope` printed the
// whole command tree instead of what switch takes.
func TestAUsageErrorFromInsideACommandPrintsThatCommandsHelp(t *testing.T) {
	var out, errOut bytes.Buffer
	env := &Env{Out: &out, Err: &errOut}
	root := Root()

	// Resolves the command and reaches its Run, which refuses the missing
	// argument — the same path a real invocation takes.
	err := Execute(context.Background(), root, env, []string{"team", "switch"})

	var usage *UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error = %v, want a *UsageError", err)
	}

	errOut.Reset()
	Report(env, root, err)

	if !strings.Contains(errOut.String(), "vallic team switch <name>") {
		t.Errorf("help was not switch's:\n%s", errOut.String())
	}

	if strings.Contains(errOut.String(), "open a shell in an environment") {
		t.Errorf("the root command list was printed instead:\n%s", errOut.String())
	}
}

// --help must work without a credential or a control plane, because the two
// most common moments to ask for it are before signing in and while offline.
func TestHelpNeedsNothing(t *testing.T) {
	var out, errOut bytes.Buffer
	env := &Env{Out: &out, Err: &errOut}

	if err := Execute(context.Background(), Root(), env, []string{"deploy", "--help"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(errOut.String(), "deploy") {
		t.Errorf("help = %q, want it to describe deploy", errOut.String())
	}
}

// A command's own flag must not collide with a global one.
//
// flag panics on a redefinition, and it does it when somebody runs the
// command rather than when the tree is built — so `vallic var list --project`
// shipped as a stack trace, not an error, because --project is already global
// and means *which* project. Registering each command's flags the way Execute
// does is the only way to see that without running every command.
func TestNoCommandRedefinesAGlobalFlag(t *testing.T) {
	var walk func(cmd *Command, path string)

	walk = func(cmd *Command, path string) {
		if cmd.Flags != nil {
			// Recovered rather than allowed to fail the run, so one
			// collision names its command instead of taking the suite down
			// with a stack trace.
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("%s: %v", path, r)
					}
				}()

				fs := flag.NewFlagSet(path, flag.ContinueOnError)
				fs.SetOutput(io.Discard)

				help := fs.Bool("help", false, "")
				fs.BoolVar(help, "h", false, "")

				env := &Env{}
				env.registerGlobals(fs)
				cmd.Flags(fs)
			}()
		}

		for _, child := range cmd.Children {
			walk(child, path+" "+child.Name)
		}
	}

	walk(Root(), "vallic")
}

// `vallic drush status` has no environment in it, and reading the first word
// as one would refuse a correct command with a message about environments.
// That is the opposite of `vallic ssh production ls`, where the first word is
// the environment, which is why the two do not share a splitter.
func TestDrushReadsNoEnvironmentWithoutASeparator(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		passthrough []string
		found       bool
		wantEnv     string
		wantRemote  []string
	}{
		{
			name: "every word is drush's when nothing separates them",
			args: []string{"status"},
			// No separator, so nothing was split out before parsing.
			wantEnv:    "",
			wantRemote: []string{"status"},
		},
		{
			name:       "including a word that happens to name an environment",
			args:       []string{"production"},
			wantEnv:    "",
			wantRemote: []string{"production"},
		},
		{
			name:        "a separator is what names one",
			args:        []string{"staging"},
			passthrough: []string{"updb", "-y"},
			found:       true,
			wantEnv:     "staging",
			wantRemote:  []string{"updb", "-y"},
		},
		{
			name:        "and the separator alone still runs drush bare",
			args:        nil,
			passthrough: []string{},
			found:       true,
			wantEnv:     "",
			wantRemote:  []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := &Env{passthrough: tc.passthrough, hasPassthrough: tc.found}

			gotEnv, gotRemote := splitDrush(env, tc.args)

			if gotEnv != tc.wantEnv {
				t.Errorf("environment = %q, want %q", gotEnv, tc.wantEnv)
			}

			if strings.Join(gotRemote, " ") != strings.Join(tc.wantRemote, " ") {
				t.Errorf("remote = %v, want %v", gotRemote, tc.wantRemote)
			}
		})
	}
}
