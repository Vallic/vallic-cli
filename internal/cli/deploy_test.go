package cli

import (
	"strings"
	"testing"

	"github.com/vallic/vallic-cli/internal/api"
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
