//go:build smoke

package smoke

import (
	"fmt"
	"strings"
	"testing"
)

// Every read command answers, and answers JSON that decodes.
//
// The broad net. A json tag that stops matching what the control plane sends
// fails the whole response rather than the one field, so decoding each answer
// through the binary is what turns a silent wrong value into a loud failure.
// This is the test that would have caught `validate` losing its ability to
// read its own reply, which no unit test could: both halves of that one were
// written from the same assumption about an empty map.
func TestEveryReadCommandAnswers(t *testing.T) {
	c := newCLI(t)

	// Reads only. Nothing here deploys, restores, takes a backup, claims a
	// hostname or writes a variable, because a smoke test that can change a
	// customer's site is one nobody dares point at anything worth testing.
	for _, args := range [][]string{
		{"whoami"},
		{"team", "list"},
		{"project", "list"},
		{"env", "list"},
		{"server", "list"},
		{"activity", "list", "--all", "--limit", "5"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			c.decode(args...)
		})
	}
}

// The same, for the commands that need an environment.
//
// Split out because they need one to exist: a control plane with no
// environments is a legitimate state for the commands above and not for these,
// and failing them for it would be failing the fixture rather than the code.
func TestEveryEnvironmentCommandAnswers(t *testing.T) {
	c := newCLI(t)

	project, environment := firstEnvironment(t, c)

	for _, args := range [][]string{
		{"env", "info"},
		{"url"},
		{"url", "--all"},
		{"service", "list"},
		{"domain", "list"},
		{"backup", "list"},
		{"var", "list"},
		{"release", "list"},
		{"env", "source"},
		{"activity", "list"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			c.decode(append(args, "--project", project, "--environment", environment)...)
		})
	}
}

// A release is named by the project's own number, everywhere, consistently.
//
// THE regression test. `release list` prints the project's sequence in its `#`
// column; `status` and `env info` printed the global entity id and called it a
// release; and `deploy --release` sent whichever of the two it had. On a young
// install the two coincide and everything looks right, which is exactly how
// this survived: it works in testing and deploys the wrong build later.
//
// Asserted across commands rather than within one, because each command was
// individually self-consistent and they disagreed with each other.
func TestOneReleaseIsNamedTheSameByEveryCommand(t *testing.T) {
	c := newCLI(t)

	project, environment := firstEnvironment(t, c)

	detail := c.decode("env", "info", "--project", project, "--environment", environment)

	current, ok := detail["environment"].(map[string]any)["current_release"].(map[string]any)
	if !ok {
		t.Skip("nothing is deployed here, so there is no release to agree about")
	}

	number, id := current["number"], current["id"]

	if number == nil {
		t.Fatal("current_release carries no number, so the CLI cannot name the build the way a person was shown it")
	}

	// The list is where somebody reads the number they will type back, so it
	// is the list this has to agree with.
	listed := c.decode("release", "list", "--project", project)

	var found bool

	for _, row := range listed["releases"].([]any) {
		release := row.(map[string]any)

		if release["id"] == id {
			found = true

			if release["number"] != number {
				t.Errorf("release %v is number %v in the list and %v on the environment",
					id, release["number"], number)
			}
		}
	}

	if !found {
		t.Errorf("the deployed release %v is not in `release list`, so the two disagree about what exists", id)
	}

	// And the human output prints the number, not the id. This is the half a
	// decode test cannot reach: the value was on the wire all along and the
	// command printed the wrong one of the two.
	//
	// Both streams, because `status` splits them deliberately: the line
	// somebody would capture goes to stdout and the commentary to stderr, so
	// checking stdout alone would look for the release where it is not.
	status := c.ok("status", "--project", project, "--environment", environment)
	shown := status.Stdout + status.Stderr

	if !strings.Contains(shown, fmt.Sprintf("release #%v", number)) {
		t.Errorf("status printed %q, want the project's own release number #%v", strings.TrimSpace(shown), number)
	}

	// The id must not be anywhere in it. This is the assertion that fails on
	// the original bug rather than merely not passing: printing both would
	// satisfy the check above and still offer a number that deploys the wrong
	// build when typed back.
	if id != number && strings.Contains(shown, fmt.Sprintf("%v", id)) {
		t.Errorf("status shows the global id %v, which is not what --release takes:\n%s", id, shown)
	}
}

// Commentary goes to stderr, so a captured value is the value and nothing else.
//
// `vallic url` exists to be captured — `curl "$(vallic url staging)"` — and a
// single stray line of explanation on stdout makes that produce a URL with
// prose attached. Cheap to assert and impossible to notice by eye, because
// both streams land on the same terminal.
func TestAValueMeantForCaptureIsAloneOnStdout(t *testing.T) {
	c := newCLI(t)

	project, environment := firstEnvironment(t, c)

	got := c.ok("url", "--project", project, "--environment", environment)

	captured := strings.TrimSpace(got.Stdout)

	if strings.Count(captured, "\n") != 0 {
		t.Errorf("stdout carried %d lines, want one address:\n%s", strings.Count(captured, "\n")+1, got.Stdout)
	}

	if !strings.HasPrefix(captured, "https://") {
		t.Errorf("stdout = %q, want an address a shell could pass to curl", captured)
	}
}

// --release takes the number a person was shown, and resolves it before asking.
//
// Deliberately an impossible number, so the refusal is the client's own and no
// request to deploy anything is ever made. Running this against a control plane
// with a live agent is how a smoke test queues a real deployment, which is not
// a hypothetical: it happened.
//
// What it proves is that resolution ran at all. A CLI that passed the number
// straight through would answer the control plane's "no such release", not
// this.
func TestAnImpossibleReleaseIsRefusedBeforeAnythingIsAsked(t *testing.T) {
	c := newCLI(t)

	project, environment := firstEnvironment(t, c)

	got := c.run("deploy", "--project", project, "--environment", environment, "--release", "999999999")

	if got.Code == 0 {
		t.Fatalf("deploying release 999999999 succeeded, which should not be possible\n%s", got.Stdout)
	}

	// The client's words, not the control plane's. "No such release in this
	// project" would mean the number went over the wire as an id.
	if !strings.Contains(got.Stderr, "this project has no release 999999999") {
		t.Errorf("stderr = %q, want the client's own refusal naming the newest release", strings.TrimSpace(got.Stderr))
	}
}

// A manifest that declares a service still decodes.
//
// The narrowest of the three, and the one that broke an entire command. PHP
// has one array type for lists and maps, so an empty `services[].environment`
// arrives as `[]`; unmarshalling that into a Go map fails the whole response.
// A manifest with no services would have passed.
func TestAManifestDeclaringAServiceDecodes(t *testing.T) {
	c := newCLI(t)

	dir := t.TempDir()

	writeFile(t, dir, "vallic.yaml", "version: 1\ntype: drupal\nservices:\n  - valkey: '8'\n")

	got := c.ok("validate", dir+"/vallic.yaml", "--local", "--format", "json")

	if !strings.Contains(got.Stdout, `"services"`) {
		t.Errorf("the echoed manifest carries no services: %s", got.Stdout)
	}
}
