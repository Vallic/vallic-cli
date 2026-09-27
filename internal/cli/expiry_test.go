package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/vallic/vallic-cli/internal/auth"
	"github.com/vallic/vallic-cli/internal/output"
)

// A credential with no stated deadline must produce no warning at all.
//
// The case that matters: an OAuth session has no token expiry, because `/me`
// carries one only for a `vcp_`. Reading an absent value as "expired" would
// warn every browser sign-in that its credential had run out, on every
// command, for ever.
func TestNothingIsSaidAboutADeadlineNobodyStated(t *testing.T) {
	env, out := envWithCredential(t, &auth.Credentials{Kind: auth.KindOAuth, API: "https://example.test"})

	warnAboutExpiry(env, time.Now())

	if out.String() != "" {
		t.Errorf("said %q, want nothing where no deadline was stated", out)
	}
}

// And nothing where a credential was never loaded, which is how the commands
// that reach nothing stay quiet.
func TestNothingIsSaidWhenNoCredentialWasUsed(t *testing.T) {
	var out bytes.Buffer

	printer, err := output.NewPrinter(&out, &out, "")
	if err != nil {
		t.Fatal(err)
	}

	env := &Env{Printer: printer}

	warnAboutExpiry(env, time.Now())

	if out.String() != "" {
		t.Errorf("said %q, want nothing: `vallic init` reaches no control plane", out)
	}
}

func TestTheWarningStartsTwoWeeksOutAndNotBefore(t *testing.T) {
	cases := []struct {
		name  string
		left  time.Duration
		warns bool
	}{
		{"a fresh ninety-day token says nothing", 90 * 24 * time.Hour, false},
		{"nor does one with a month left", 30 * 24 * time.Hour, false},
		{"just outside the window is still quiet", 15 * 24 * time.Hour, false},
		{"inside it, it speaks", 13 * 24 * time.Hour, true},
		{"and keeps speaking", 2 * 24 * time.Hour, true},
		{"including once it has gone", -2 * 24 * time.Hour, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()

			env, out := envWithCredential(t, &auth.Credentials{
				Kind:        auth.KindToken,
				API:         "https://example.test",
				TokenExpiry: now.Add(tc.left),
			})

			warnAboutExpiry(env, now)

			if spoke := out.String() != ""; spoke != tc.warns {
				t.Errorf("warned = %v, want %v (said %q)", spoke, tc.warns, out)
			}
		})
	}
}

// Past and future are different sentences, and saying the wrong one is worse
// than saying nothing: "runs out in -2 days" is how a reader concludes the
// tool is broken and ignores the rest of the line.
func TestALapsedCredentialIsDescribedInThePastTense(t *testing.T) {
	now := time.Now()

	env, out := envWithCredential(t, &auth.Credentials{
		Kind:        auth.KindToken,
		API:         "https://example.test",
		TokenExpiry: now.Add(-49 * time.Hour),
	})

	warnAboutExpiry(env, now)

	said := out.String()

	if !strings.Contains(said, "ran out 2 days ago") {
		t.Errorf("said %q, want it in the past tense", said)
	}

	// Not a bare hyphen check: the advice carries `--token`. What must not
	// appear is a negative figure, which is what a future-tense sentence
	// about a past date produces.
	if strings.Contains(said, "-2") || strings.Contains(said, "runs out") {
		t.Errorf("said %q, want the past tense and no negative figure", said)
	}
}

// The advice differs by credential, and the token one must not say "rotate":
// there is deliberately no way for a token to mint its successor, so advice
// that implied otherwise would send somebody looking for a command that does
// not exist and should not.
func TestTheAdviceMatchesWhatCanActuallyRenewTheCredential(t *testing.T) {
	token := renewAdvice(&auth.Credentials{Kind: auth.KindToken, API: "https://console.example"})

	if !strings.Contains(token, "console.example/account") {
		t.Errorf("advice = %q, want it to name where a token is minted", token)
	}

	if !strings.Contains(token, tokensTab) {
		t.Errorf("advice = %q, want it to name the tab, since the URL cannot name the person", token)
	}

	if strings.Contains(strings.ToLower(token), "rotate") {
		t.Errorf("advice = %q, want no suggestion that a token renews itself", token)
	}

	session := renewAdvice(&auth.Credentials{Kind: auth.KindOAuth, API: "https://console.example"})

	if !strings.Contains(session, "vallic login") {
		t.Errorf("advice = %q, want the browser sign-in", session)
	}
}

// Rounded down, so a figure is never more optimistic than the truth: "in 2
// days" must not mean ten minutes from now.
func TestADurationIsNeverRoundedIntoMoreTimeThanThereIs(t *testing.T) {
	cases := map[time.Duration]string{
		13*24*time.Hour + 23*time.Hour: "in 13 days",
		2 * 24 * time.Hour:             "in 2 days",
		47 * time.Hour:                 "tomorrow",
		25 * time.Hour:                 "tomorrow",
		23 * time.Hour:                 "in 23 hours",
		30 * time.Minute:               "within the hour",
	}

	for left, want := range cases {
		if got := humanIn(left); got != want {
			t.Errorf("humanIn(%s) = %q, want %q", left, got, want)
		}
	}
}

// envWithCredential builds an Env holding a loaded credential.
func envWithCredential(t *testing.T, creds *auth.Credentials) (*Env, *bytes.Buffer) {
	t.Helper()

	var out bytes.Buffer

	printer, err := output.NewPrinter(&out, &out, "")
	if err != nil {
		t.Fatal(err)
	}

	return &Env{Printer: printer, creds: creds}, &out
}
