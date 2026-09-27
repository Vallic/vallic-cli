//go:build smoke

package smoke

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Where the control plane is, and what to present to it.
const (
	apiEnv   = "VALLIC_API"
	tokenEnv = "VALLIC_TOKEN"
)

var (
	buildOnce sync.Once
	buildPath string
	buildErr  error
)

// cli is a built binary pointed at the control plane under test.
type cli struct {
	t    *testing.T
	home string
}

// newCLI builds the binary once and gives each test its own home.
//
// Its own home because the binary reads a credential from disk and writes one
// back, and a smoke test that picked up the developer's own credential would
// be testing against whatever they last signed in to. The token comes from the
// environment, which wins over the disk, so the empty home is never consulted
// for one; it is there so that nothing can be.
func newCLI(t *testing.T) *cli {
	t.Helper()

	if os.Getenv(apiEnv) == "" || os.Getenv(tokenEnv) == "" {
		t.Skipf("set %s and %s to run the smoke tests against a control plane", apiEnv, tokenEnv)
	}

	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "vallic-smoke")
		if err != nil {
			buildErr = err

			return
		}

		buildPath = filepath.Join(dir, "vallic")

		// The binary, not the library. Two of the three bugs this package
		// exists for were in what the command printed rather than in what the
		// client decoded, and a test that called the package directly would
		// have passed through both.
		root, err := filepath.Abs("../..")
		if err != nil {
			buildErr = err

			return
		}

		build := exec.Command("go", "build", "-o", buildPath, "./cmd/vallic")
		build.Dir = root

		if out, err := build.CombinedOutput(); err != nil {
			buildErr = err
			buildPath = string(out)
		}
	})

	if buildErr != nil {
		t.Fatalf("building the binary: %v\n%s", buildErr, buildPath)
	}

	return &cli{t: t, home: t.TempDir()}
}

// result is one invocation.
type result struct {
	Stdout string
	Stderr string
	Code   int
}

// run invokes the binary and returns what it said.
//
// Never fails the test on a non-zero exit: several of these assert a refusal,
// and a helper that treated every refusal as a failure could not express them.
func (c *cli) run(args ...string) result {
	c.t.Helper()

	cmd := exec.Command(buildPath, args...)

	// A closed stdin, so nothing can sit waiting for a confirmation. The
	// commands here do not prompt, and a smoke run that hung until CI timed
	// out would be indistinguishable from a control plane that never answered.
	cmd.Stdin = nil

	cmd.Env = append(os.Environ(),
		"HOME="+c.home,
		"NO_COLOR=1",
	)

	var stdout, stderr strings.Builder

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		c.t.Fatalf("running %v: %v", args, err)
	}

	return result{Stdout: stdout.String(), Stderr: stderr.String(), Code: code}
}

// ok runs a command that must succeed.
func (c *cli) ok(args ...string) result {
	c.t.Helper()

	got := c.run(args...)

	if got.Code != 0 {
		c.t.Fatalf("vallic %s exited %d\nstdout: %s\nstderr: %s",
			strings.Join(args, " "), got.Code, got.Stdout, got.Stderr)
	}

	return got
}

// decode runs a command with --format json and unmarshals its answer.
//
// Decoding through the CLI rather than reading the API directly is the point:
// a json tag that does not match the key the control plane sends fails here
// and nowhere else, and the failure is the whole response rather than the one
// field.
func (c *cli) decode(args ...string) map[string]any {
	c.t.Helper()

	got := c.ok(append(args, "--format", "json")...)

	var out map[string]any

	if err := json.Unmarshal([]byte(got.Stdout), &out); err != nil {
		c.t.Fatalf("vallic %s --format json did not answer JSON: %v\n%s",
			strings.Join(args, " "), err, got.Stdout)
	}

	return out
}
