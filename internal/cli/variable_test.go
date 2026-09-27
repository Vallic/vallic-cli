package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestVariableScopeRefusesAnythingElse(t *testing.T) {
	for _, in := range []string{"", "environment"} {
		got, err := variableScope(in)
		if err != nil {
			t.Fatalf("variableScope(%q) = %v", in, err)
		}

		if got != scopeEnvironment {
			t.Errorf("variableScope(%q) = %q, want %q", in, got, scopeEnvironment)
		}
	}

	if got, err := variableScope("project"); err != nil || got != scopeProject {
		t.Errorf(`variableScope("project") = %q, %v`, got, err)
	}

	// A misspelling must not fall back to the environment. Silently acting on
	// one environment when somebody asked for the project would be the wrong
	// write, not a narrower one.
	_, err := variableScope("Project")
	if err == nil {
		t.Fatal(`variableScope("Project") = nil error, want a refusal`)
	}

	var usage *UsageError
	if !errors.As(err, &usage) {
		t.Errorf("error = %T, want a *UsageError so the exit code is 2", err)
	}

	if !strings.Contains(err.Error(), "environment or project") {
		t.Errorf("error = %q, want it to name the two it takes", err)
	}
}

// Where the control plane says nothing, "next deploy" is what every version
// so far does — but the platform's own words win, so a CLI does not have to
// ship again the day that changes.
func TestTakesEffectPrefersTheControlPlanesWords(t *testing.T) {
	if got, want := takesEffect(""), "next deploy"; got != want {
		t.Errorf("takesEffect(``) = %q, want %q", got, want)
	}

	if got, want := takesEffect("restart"), "restart"; got != want {
		t.Errorf("takesEffect(`restart`) = %q, want %q", got, want)
	}
}

// --stdin with a terminal on the other end would wait for typing with no
// prompt to say so, which reads as a hang. It has to refuse instead.
func TestReadValueRefusesATerminal(t *testing.T) {
	// A bytes.Buffer is not an *os.File, so Interactive() is already false —
	// which is the pipeline case, and it must read rather than refuse.
	env := &Env{In: bytes.NewBufferString("pk_live_abc")}

	got, err := readValue(env)
	if err != nil {
		t.Fatalf("readValue() = %v", err)
	}

	if got != "pk_live_abc" {
		t.Errorf("readValue() = %q, want the piped value", got)
	}

	// --no-input makes Interactive() false too, and that must not turn into
	// "read from a terminal nobody is at".
	env = &Env{In: bytes.NewBufferString("x"), flagNoInput: true}

	if _, err := readValue(env); err != nil {
		t.Errorf("readValue() with --no-input = %v, want the piped value", err)
	}
}

// A value is not trimmed. A token with a stripped character is a token that
// fails opaquely, hours later, somewhere else.
func TestReadValueKeepsWhatWasPipedIn(t *testing.T) {
	env := &Env{In: bytes.NewBufferString("line one\nline two\n")}

	got, err := readValue(env)
	if err != nil {
		t.Fatal(err)
	}

	if got != "line one\nline two\n" {
		t.Errorf("readValue() = %q, want it verbatim, trailing newline included", got)
	}
}

// Truncating a key would store something that looks set and fails at
// runtime, which is much worse than being told now.
func TestReadValueRefusesMoreThanAVariableHolds(t *testing.T) {
	env := &Env{In: strings.NewReader(strings.Repeat("a", maxVariable+1))}

	if _, err := readValue(env); err == nil {
		t.Fatal("readValue() = nil error, want a refusal")
	}

	// Exactly at the limit is accepted: the boundary belongs to the value.
	env = &Env{In: strings.NewReader(strings.Repeat("a", maxVariable))}

	got, err := readValue(env)
	if err != nil {
		t.Fatalf("readValue() at the limit = %v, want it accepted", err)
	}

	if len(got) != maxVariable {
		t.Errorf("readValue() read %d bytes, want %d", len(got), maxVariable)
	}
}
