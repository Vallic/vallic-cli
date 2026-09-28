package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The generated bash script has to parse as bash.
//
// This is the test that earns its keep. A completion script is read by a shell
// and never by a person, so a generator that emits something unparseable fails
// silently: TAB simply stops working, and nothing anywhere says why. The first
// version of this emitted `team list)` as a case label, which is a syntax error
// at `list` — every multi-word command in the tree was broken, and `go test`,
// `go vet` and reading the output all looked fine.
func TestTheGeneratedBashScriptParses(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash to check against")
	}

	script := filepath.Join(t.TempDir(), "vallic.bash")

	if err := os.WriteFile(script, []byte(bashCompletion(describeTree(Root(), &Env{}))), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, err := exec.Command(bash, "-n", script).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n rejected the script: %v\n%s", err, out)
	}
}

// And it has to complete, not merely parse.
//
// Driven the way bash drives it — COMP_WORDS, COMP_CWORD, read COMPREPLY —
// because everything about this only exists inside a shell, and a test that
// asserted on the generated text would be asserting on the shape of the
// generator rather than on whether TAB works.
func TestTheBashScriptCompletesCommandsSubcommandsAndFlags(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash to drive")
	}

	dir := t.TempDir()
	script := filepath.Join(dir, "vallic.bash")

	if err := os.WriteFile(script, []byte(bashCompletion(describeTree(Root(), &Env{}))), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, tc := range []struct {
		name  string
		words []string
		want  []string
	}{
		{"a bare prefix", []string{"vallic", "bu"}, []string{"build"}},
		{"a noun's subcommands", []string{"vallic", "env", ""}, []string{"list", "create", "info"}},
		{"a command's own flag", []string{"vallic", "build", "--"}, []string{"--deploy"}},
		{"a flag added elsewhere", []string{"vallic", "release", "list", "--br"}, []string{"--branch"}},

		// The one that is easy to get wrong: a global flag's *value* is not a
		// command, so the path here is "env" and not "acme env".
		{"a flag value is not part of the path", []string{"vallic", "--project", "acme", "env", ""}, []string{"list"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := complete(t, bash, script, tc.words)

			for _, want := range tc.want {
				if !strings.Contains(" "+got+" ", " "+want+" ") {
					t.Errorf("completing %q offered %q, want it to include %q", tc.words, got, want)
				}
			}
		})
	}
}

// complete runs the script's function against one set of words.
func complete(t *testing.T, bash, script string, words []string) string {
	t.Helper()

	var quoted []string
	for _, word := range words {
		quoted = append(quoted, "'"+word+"'")
	}

	program := strings.Join([]string{
		"source " + script,
		"COMP_WORDS=(" + strings.Join(quoted, " ") + ")",
		"COMP_CWORD=" + itoa(len(words)-1),
		"COMPREPLY=()",
		"_vallic",
		`printf '%s\n' "${COMPREPLY[*]}"`,
	}, "\n")

	out, err := exec.Command(bash, "-c", program).CombinedOutput()
	if err != nil {
		t.Fatalf("driving completion: %v\n%s", err, out)
	}

	return strings.TrimSpace(string(out))
}

// itoa without importing strconv for one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	var digits []byte

	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}

	return string(digits)
}

// Every command the tree has is offered somewhere.
//
// The point of generating from the tree rather than writing a list: a command
// added without a thought for completion is completed anyway. Asserted so that
// a future refactor which loses a branch of the walk fails here rather than in
// somebody's terminal.
func TestEveryCommandInTheTreeIsCompletable(t *testing.T) {
	root := Root()
	script := bashCompletion(describeTree(root, &Env{}))

	var missing []string

	var walk func(cmd *Command, path string)

	walk = func(cmd *Command, path string) {
		for _, child := range cmd.Children {
			if !strings.Contains(script, `"`+strings.TrimSpace(path+" "+child.Name)+`"`) {
				missing = append(missing, strings.TrimSpace(path+" "+child.Name))
			}

			walk(child, strings.TrimSpace(path+" "+child.Name))
		}
	}

	walk(root, "")

	if len(missing) > 0 {
		t.Errorf("not completable: %s", strings.Join(missing, ", "))
	}
}
