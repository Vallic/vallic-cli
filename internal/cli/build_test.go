package cli

import (
	"strings"
	"testing"

	"github.com/vallic/vallic-cli/internal/api"
)

// What happens when the build finishes has to be said, and said accurately.
//
// Three outcomes and they are genuinely different to the person reading. The
// middle one is why `will_deploy` is a separate field from `deploy_requested`:
// somebody who ran `vallic build` to look at a branch before shipping it, on an
// environment that deploys that branch automatically, is about to ship it. A
// client that echoed the request back would tell them nothing is being
// deployed while a deployment starts.
func TestTheBuildOutputSaysWhatWillActuallyHappen(t *testing.T) {
	for _, tc := range []struct {
		name  string
		built api.Build
		want  string
		avoid string
	}{
		{
			name:  "asked for",
			built: api.Build{Environment: "acme-staging", WillDeploy: true, DeployRequested: true},
			want:  "it will deploy to acme-staging",
		},
		{
			name:  "not asked for, but auto-deploy will",
			built: api.Build{Environment: "acme-staging", WillDeploy: true, DeployRequested: false},
			want:  "deploys this branch automatically",
			avoid: "nothing is deployed",
		},
		{
			name:  "nothing will",
			built: api.Build{Environment: "acme-staging", WillDeploy: false, DeployRequested: false},
			want:  "nothing is deployed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := strings.Join(buildLines(&tc.built), "\n")

			if !strings.Contains(lines, tc.want) {
				t.Errorf("lines = %q, want them to say %q", lines, tc.want)
			}

			if tc.avoid != "" && strings.Contains(lines, tc.avoid) {
				t.Errorf("lines = %q, want them NOT to say %q", lines, tc.avoid)
			}
		})
	}
}

// The release is named by its number, because the number is what --release
// takes.
//
// The id is global and the number is the project's own sequence. Printing 3339
// for what the project calls 7 offers a figure that looks typeable and is not —
// a mistake already made twice on this surface, in `status` and in the agent's
// deploy log, so the route sends both and this prints the right one.
func TestTheBuildOutputNamesTheReleaseByItsNumber(t *testing.T) {
	number := 7

	got := buildStarted(&api.Build{Release: 3339, ReleaseNumber: &number, GitRef: "staging"})

	if !strings.Contains(got, "release 7") {
		t.Errorf("got %q, want it to name release 7", got)
	}

	if strings.Contains(got, "3339") {
		t.Errorf("got %q, want the id kept out of it", got)
	}

	// And where the control plane could not read one back, the id is printed
	// and labelled as one rather than passed off as typeable.
	bare := buildStarted(&api.Build{Release: 3339, GitRef: "staging"})

	if !strings.Contains(bare, "release id 3339") {
		t.Errorf("got %q, want the id said to be an id", bare)
	}
}

// Shipping to a protected environment is confirmed, and only then.
//
// `build --deploy` skipped this until [2026-09-30] while `vallic deploy` made
// it, which turned the flag into a way round a question the other path asks --
// on an environment marked protected precisely so that shipping to it is a
// deliberate act.
func TestOnlyABuildThatShipsToAProtectedEnvironmentIsConfirmed(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		deploy, protected, interactive bool
		want                           bool
	}{
		{"shipping to a protected environment at a terminal", true, true, true, true},

		// A build that deploys nothing cannot reach the live site, so there is
		// nothing to be careful about however protected the environment is.
		{"building only, however protected", false, true, true, false},

		// Protection guards reshaping rather than operating. A prompt on every
		// staging deploy is a prompt nobody reads by the third one.
		{"shipping to an unprotected environment", true, false, true, false},

		// A pipeline has nobody to answer. Blocking there would hang a build
		// rather than refuse it.
		{"no terminal to ask at", true, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := asksBeforeShipping(tc.deploy, tc.protected, tc.interactive)

			if got != tc.want {
				t.Errorf("asksBeforeShipping(%v, %v, %v) = %v, want %v",
					tc.deploy, tc.protected, tc.interactive, got, tc.want)
			}
		})
	}
}
