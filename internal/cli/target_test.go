package cli

import (
	"testing"

	"github.com/vallic/vallic-cli/internal/api"
)

// A question about a protected environment names the project too.
//
// Every project has a "production". Asked "Deploy to production, which is
// protected?", somebody whose checkout had resolved to another project's
// would answer yes, and deploy that one.
func TestTheTargetIsNamedWithItsProject(t *testing.T) {
	target := &Target{
		Project:     &api.Project{MachineName: "acme", Label: "Acme"},
		Environment: &api.Environment{Slug: "acme-production", Name: "production"},
	}

	if got := target.Named(); got != "acme-production (Acme)" {
		t.Errorf("Named() = %q", got)
	}

	target.Project.Label = ""
	if got := target.Named(); got != "acme-production" {
		t.Errorf("Named() without a label = %q", got)
	}

	if got := (&Target{}).Named(); got != "this environment" {
		t.Errorf("Named() with nothing = %q", got)
	}
}
