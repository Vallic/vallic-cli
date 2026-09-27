package cli

import (
	"testing"

	"github.com/vallic/vallic-cli/internal/api"
)

// The reason is the half the state cannot carry. "failed" says a record was
// checked and was wrong; only the message says which way, and the control
// plane keeps those two answers apart on purpose. If this stops printing, the
// only way back to them is `domain verify`, which spends one of twelve hourly
// slots and a DNS lookup to re-read what the list already carried.
func TestCheckReasonsSaysWhichWayACheckFailed(t *testing.T) {
	cases := []struct {
		name    string
		domains []api.Domain
		want    []string
	}{
		{
			name: "nothing published",
			domains: []api.Domain{{
				Hostname:            "one.example",
				VerificationState:   "failed",
				VerificationMessage: "No TXT record found at _vallic.one.example.",
			}},
			want: []string{"one.example: No TXT record found at _vallic.one.example."},
		},
		{
			name: "published, wrong value",
			domains: []api.Domain{{
				Hostname:            "two.example",
				VerificationState:   "failed",
				VerificationMessage: "A TXT record exists at _vallic.two.example but does not hold the expected value.",
			}},
			want: []string{"two.example: A TXT record exists at _vallic.two.example but does not hold the expected value."},
		},
		{
			// A verified hostname carries a message too, and it is not a
			// reason anything is unverified. Printing it under that heading
			// would be a line saying the opposite of what it sits under.
			name: "verified says nothing here",
			domains: []api.Domain{{
				Hostname:            "live.example",
				Verified:            true,
				VerificationState:   "verified",
				VerificationMessage: "Verified, and traffic is reaching us.",
			}},
			want: nil,
		},
		{
			// Nothing has checked, so there is nothing the control plane
			// found. "unverified: " with an empty sentence after it reads as
			// a truncated message.
			name: "never checked",
			domains: []api.Domain{{
				Hostname:          "new.example",
				VerificationState: "unverified",
			}},
			want: nil,
		},
		{
			name: "mixed list keeps the table's order",
			domains: []api.Domain{
				{Hostname: "acme-dev.vallic.app", Kind: "platform", Verified: true, VerificationState: "verified"},
				{Hostname: "new.example", VerificationState: "unverified"},
				{Hostname: "one.example", VerificationState: "failed", VerificationMessage: "No TXT record found at _vallic.one.example."},
				{Hostname: "elsewhere.example", VerificationState: "failed", VerificationMessage: "This hostname is already verified on another environment."},
			},
			want: []string{
				"one.example: No TXT record found at _vallic.one.example.",
				"elsewhere.example: This hostname is already verified on another environment.",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := checkReasons(c.domains)

			if len(got) != len(c.want) {
				t.Fatalf("checkReasons() = %q, want %q", got, c.want)
			}

			for i := range c.want {
				if got[i] != c.want[i] {
					t.Errorf("line %d = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}
