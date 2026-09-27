package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vallic/vallic-cli/internal/api"
	"github.com/vallic/vallic-cli/internal/auth"
	"github.com/vallic/vallic-cli/internal/output"
)

// A named release from another branch is worth saying, and must not be refused.
//
// Only the unnamed deploy path is branch-scoped on the control plane:
// UserDeployController::release() checks an explicitly named release for
// project membership and nothing else. So this is a warning, and a test that
// asserted a refusal would be asserting a rule the platform does not have --
// and would break the case the freedom exists for, putting a hotfix built
// elsewhere onto production deliberately.
func TestBranchWarningNamesBothBranches(t *testing.T) {
	warning := branchWarning(
		api.Release{Number: 9, GitRef: "drupal-automated-updates"},
		"production", "main",
	)

	if warning == "" {
		t.Fatal("a build from another branch was deployed with nothing said")
	}

	for _, want := range []string{"9", "drupal-automated-updates", "production", "main"} {
		if !strings.Contains(warning, want) {
			t.Errorf("warning = %q, want it to name %q", warning, want)
		}
	}
}

// And says nothing when there is nothing to say. A warning on every ordinary
// deploy is a warning nobody reads by the third one.
func TestBranchWarningStaysQuietWhenTheBranchesAgree(t *testing.T) {
	for _, tc := range []struct {
		name    string
		release api.Release
		ref     string
	}{
		{"same branch", api.Release{Number: 7, GitRef: "main"}, "main"},
		// A control plane that sends no git_ref, and an environment that
		// tracks none: neither is a disagreement, and guessing at one would
		// warn about every deploy on an older control plane.
		{"release says nothing", api.Release{Number: 7}, "main"},
		{"environment tracks nothing", api.Release{Number: 7, GitRef: "main"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := branchWarning(tc.release, "production", tc.ref); got != "" {
				t.Errorf("branchWarning = %q, want silence", got)
			}
		})
	}
}

// The drain path must not run as fast as the network allows.
//
// It did. `truncated` means the control plane already holds more than it just
// sent, so this branch asks again at once rather than waiting out a backoff --
// and it used to do that with no sleep whatsoever. Measured against a loopback
// server that always says `truncated`, the loop made 23,873 requests a second:
// 1.4 million a minute, against a control plane that allows one token 600. A
// deploy whose log ran past a single 256 KiB window would rate-limit its own
// token within seconds, and the token is shared by every CI job using that
// credential.
//
// Asserted as a ceiling on the rate rather than by counting sleeps, because
// the rate is the thing that matters and it survives the loop being rewritten.
func TestTheDrainPathIsRateLimitedBelowWhatThePlatformAllows(t *testing.T) {
	var requests int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&requests, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"log":{"offset":0,"content":"x","next_offset":1,` +
			`"size":999999,"truncated":true,"state":"running","complete":false}}`))
	}))
	defer server.Close()

	env, _, _ := followEnv(t)
	client := api.New(server.URL, auth.Anonymous{}, "test")

	const window = 1200 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()

	start := time.Now()
	_, _ = env.follow(ctx, client, 1, io.Discard)
	elapsed := time.Since(start)

	perMinute := float64(atomic.LoadInt64(&requests)) / elapsed.Seconds() * 60

	// The platform's own ceiling, and half of it is the budget this loop may
	// take: the rest belongs to everything else holding the same token.
	const platformLimit = 600.0

	if perMinute > platformLimit/2 {
		t.Errorf("drain asked %.0f times a minute, want at most %.0f (the platform allows %.0f per token)",
			perMinute, platformLimit/2, platformLimit)
	}
}

// A 429 is the one 4xx a poll must not give up on.
//
// The control plane's message is "wait and repeat the request unchanged", and
// the loop used to return on any status below 500 -- so the first rate limit
// mid-deploy abandoned a deploy that was still running, after four requests.
// This asserts it rides it out instead, and that the deploy's own outcome is
// what comes back.
func TestAFollowRidesOutARateLimitInsteadOfAbandoningTheDeploy(t *testing.T) {
	var requests int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&requests, 1)
		w.Header().Set("Content-Type", "application/json")

		// Limited in the middle, then the deploy finishes. What a token that
		// went briefly over its minute looks like.
		switch n {
		case 1:
			_, _ = w.Write([]byte(`{"log":{"offset":0,"content":"step\n","next_offset":5,` +
				`"size":5,"truncated":false,"state":"running","complete":false}}`))
		case 2:
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":"rate_limited",` +
				`"message":"This token has made more than 600 requests in 60 seconds."}}`))
		default:
			_, _ = w.Write([]byte(`{"log":{"offset":5,"content":"done\n","next_offset":10,` +
				`"size":10,"truncated":false,"state":"succeeded","complete":true}}`))
		}
	}))
	defer server.Close()

	env, _, errOut := followEnv(t)
	client := api.New(server.URL, auth.Anonymous{}, "test")

	// The real wait is ten seconds and there is nothing to learn from
	// spending it here: what is under test is that it waits at all and comes
	// back with the deploy's outcome, not how long.
	restore := rateLimitWait
	rateLimitWait = 20 * time.Millisecond

	defer func() { rateLimitWait = restore }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	state, err := env.follow(ctx, client, 1, io.Discard)
	if err != nil {
		t.Fatalf("follow: %v, want it to wait and carry on", err)
	}

	if state != "succeeded" {
		t.Errorf("state = %q, want the deploy's own outcome", state)
	}

	// Said once, so somebody watching knows why it went quiet.
	if !strings.Contains(errOut.String(), "rate limited") {
		t.Errorf("stderr = %q, want it to say why it paused", errOut.String())
	}
}

// followEnv is an Env that writes where a test can read it.
func followEnv(t *testing.T) (*Env, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()

	var out, errOut bytes.Buffer

	printer, err := output.NewPrinter(&out, &errOut, "table")
	if err != nil {
		t.Fatalf("printer: %v", err)
	}

	return &Env{Out: &out, Err: &errOut, Printer: printer}, &out, &errOut
}

// A Retry-After is obeyed in preference to the CLI's own guess.
//
// The whole point of reading the header: the server knows when its window
// frees and this does not. The control plane sends none today, so the case
// under test is something in front of it — a proxy, a CDN — whose limit this
// client never reasoned about.
func TestAFollowObeysRetryAfterRatherThanItsOwnGuess(t *testing.T) {
	var requests int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&requests, 1)
		w.Header().Set("Content-Type", "application/json")

		switch n {
		case 1:
			_, _ = w.Write([]byte(`{"log":{"offset":0,"content":"step\n","next_offset":5,` +
				`"size":5,"truncated":false,"state":"running","complete":false}}`))
		case 2:
			// Shorter than rateLimitWait below, so obeying it is the only way
			// this finishes inside the deadline.
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"slow down"}}`))
		default:
			_, _ = w.Write([]byte(`{"log":{"offset":5,"content":"done\n","next_offset":10,` +
				`"size":10,"truncated":false,"state":"succeeded","complete":true}}`))
		}
	}))
	defer server.Close()

	env, _, errOut := followEnv(t)
	client := api.New(server.URL, auth.Anonymous{}, "test")

	// The guess is made long on purpose: if it were used, this would not
	// finish before the deadline and the test would fail on the timeout.
	restoreWait, restoreBudget := rateLimitWait, rateLimitBudget
	rateLimitWait = time.Hour
	rateLimitBudget = 90 * time.Second

	defer func() { rateLimitWait, rateLimitBudget = restoreWait, restoreBudget }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	state, err := env.follow(ctx, client, 1, io.Discard)
	if err != nil {
		t.Fatalf("follow: %v, want it to wait the second it was asked for", err)
	}

	if state != "succeeded" {
		t.Errorf("state = %q, want the deploy's own outcome", state)
	}

	// The figure is repeated back, because "waiting" and "waiting twenty
	// minutes" are different things to be told while watching a deploy.
	if !strings.Contains(errOut.String(), "asked for 1s") {
		t.Errorf("stderr = %q, want it to name what was asked for", errOut.String())
	}
}

// And a Retry-After longer than the budget stops the watch rather than
// honouring it.
//
// Obeying without a ceiling is how a terminal ends up held for an afternoon
// by one header. The deploy is unaffected either way, so the useful thing is
// to say the figure and how to reattach.
func TestARetryAfterLongerThanTheBudgetIsRefusedWithTheFigure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "1800")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"slow down"}}`))
	}))
	defer server.Close()

	env, _, _ := followEnv(t)
	client := api.New(server.URL, auth.Anonymous{}, "test")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := env.follow(ctx, client, 77, io.Discard)

	if err == nil {
		t.Fatal("waited half an hour inside a deploy watch, or did not report giving up")
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v to refuse; it must not sleep first", elapsed)
	}

	for _, want := range []string{"30m", "vallic activity log 77"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err, want)
		}
	}
}
