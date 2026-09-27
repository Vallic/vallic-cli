package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/vallic/vallic-cli/internal/api"
)

// The address is printed whole, scheme included, because which TLS mode
// terminates the connection is the control plane's answer and not one a
// client can work out. A regression that rebuilt it from the platform
// hostname would print http for nothing and would ignore the canonical name.
func TestCreatedAddressPrintsTheControlPlanesURL(t *testing.T) {
	created := &api.EnvironmentDetail{Hostnames: &api.Hostnames{
		Platform: "acme-staging.vallic.app",
		Routed:   []string{"acme-staging.vallic.app"},
		URL:      "https://acme-staging.vallic.app",
	}}

	if got, want := createdAddress(created), "https://acme-staging.vallic.app"; got != want {
		t.Errorf("createdAddress() = %q, want %q", got, want)
	}
}

// An older control plane sends no hostnames block at all, and the address
// cannot be assembled here: the platform domain is configuration this binary
// does not hold. Empty drops the line. A regression that guessed would print
// a confident wrong address on every environment created against it.
func TestCreatedAddressIsEmptyWithoutAHostnamesBlock(t *testing.T) {
	created := &api.EnvironmentDetail{}
	created.Slug = "acme-staging"

	if got := createdAddress(created); got != "" {
		t.Errorf("createdAddress() = %q, want nothing to print", got)
	}
}

// The two shape rules need opposite advice, and getting it backwards sends
// somebody to create a second staging the platform has just refused.
//
// Matched on the message rather than a code, which is normally what this
// codebase refuses to do. Tolerated here because the advice is appended to
// the control plane's own sentence: a wrong guess costs a line of hint rather
// than a wrong action, and the fallback is true for either. If the two rules
// ever get codes of their own, this is the function that should stop reading
// English.
func TestShapeAdvicePointsTheOppositeWayForEachRule(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    string
	}{
		{
			name:    "nothing to merge towards yet",
			message: "Acme has no staging environment yet. Staging comes first.",
			want:    "--type staging",
		},
		{
			name:    "and a project only gets one",
			message: "Acme already has a staging environment.",
			want:    "--type development",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shapeAdvice(errors.New(tc.message))

			if !strings.Contains(got, tc.want) {
				t.Errorf("shapeAdvice(%q) = %q, want it to suggest %s", tc.message, got, tc.want)
			}
		})
	}

	// A refusal this does not recognise must still say something true. A
	// wrong suggestion is worse than a general one, and the rules are the
	// control plane's to change without telling this function.
	unknown := shapeAdvice(errors.New("some rule nobody here has heard of"))

	if strings.Contains(unknown, "--type") {
		t.Errorf("shapeAdvice(unknown) = %q, want no guess about which type", unknown)
	}

	if !strings.Contains(unknown, "env list") {
		t.Errorf("shapeAdvice(unknown) = %q, want something a person can act on", unknown)
	}
}
