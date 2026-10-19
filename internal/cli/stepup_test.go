package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vallic/vallic-cli/internal/api"
	"github.com/vallic/vallic-cli/internal/auth"
)

// What the control plane answers when an action wants a recent sign-in.
func reauthenticationRequired() error {
	return &api.Error{
		Code:    api.CodeReauthenticationRequired,
		Message: "Adding an SSH key asks you to confirm who you are.",
		Status:  403,
	}
}

// An action that goes through is run once, and nothing about credentials is
// asked.
func TestWithStepUpLeavesAnActionThatWentThroughAlone(t *testing.T) {
	env := &Env{In: bytes.NewBuffer(nil)}
	calls := 0

	err := env.WithStepUp(context.Background(), func() error {
		calls++

		return nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("WithStepUp() = %v after %d calls, want nil after one", err, calls)
	}
}

// Any other refusal is passed back as it came.
func TestWithStepUpPassesOtherErrorsBack(t *testing.T) {
	env := &Env{In: bytes.NewBuffer(nil)}
	refused := &api.Error{Code: "forbidden", Message: "Not yours.", Status: 403}

	err := env.WithStepUp(context.Background(), func() error { return refused })
	if !errors.Is(err, refused) {
		t.Fatalf("WithStepUp() = %v, want the refusal itself", err)
	}
}

// With no terminal there is nobody to send to the browser, and saying so is
// the answer: a pipeline cannot confirm who it is.
func TestWithStepUpRefusesWithoutATerminal(t *testing.T) {
	env := &Env{In: bytes.NewBuffer(nil), creds: &auth.Credentials{Kind: auth.KindOAuth}}
	calls := 0

	err := env.WithStepUp(context.Background(), func() error {
		calls++

		return reauthenticationRequired()
	})
	if err == nil || !strings.Contains(err.Error(), "no terminal to ask on") {
		t.Fatalf("WithStepUp() = %v, want it to say there is no terminal", err)
	}

	if calls != 1 {
		t.Errorf("the action ran %d times, want once: nothing was confirmed in between", calls)
	}

	if strings.Contains(err.Error(), "restore") {
		t.Errorf("WithStepUp() = %q, which names one action for all of them", err)
	}
}

// A personal access token has nothing to confirm with, and the error says
// what to do instead.
func TestWithStepUpTellsATokenToSignIn(t *testing.T) {
	env := &Env{In: bytes.NewBuffer(nil), creds: &auth.Credentials{Kind: auth.KindToken}}

	err := env.WithStepUp(context.Background(), reauthenticationRequired)
	if err == nil || !strings.Contains(err.Error(), "vallic login") {
		t.Fatalf("WithStepUp() = %v, want it to point at vallic login", err)
	}
}
