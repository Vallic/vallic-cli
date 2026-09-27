package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/vallic/vallic-cli/internal/api"
)

func linkable() []api.Project {
	return []api.Project{
		{ID: 1, MachineName: "acme-shop", Label: "Acme storefront"},
		{ID: 2, MachineName: "storefront", Label: "acme-shop"},
	}
}

// A machine name is matched before any label, because the two namespaces
// overlap: one project's label can be another's machine name. Matching labels
// first would record project 2 for `vallic link acme-shop` and every later
// command would act on the wrong site, with the right name printed back.
func TestChooseProjectPrefersAMachineNameOverALabel(t *testing.T) {
	got, err := chooseProject(linkable(), "acme-shop")
	if err != nil {
		t.Fatal(err)
	}

	if got.ID != 1 {
		t.Errorf("chooseProject() = %d (%s), want the project whose machine name it is", got.ID, got.MachineName)
	}

	// A label still matches where no machine name does, since that is what
	// the console shows somebody.
	got, err = chooseProject(linkable(), "Acme storefront")
	if err != nil {
		t.Fatal(err)
	}

	if got.ID != 1 {
		t.Errorf("chooseProject() by label = %d, want 1", got.ID)
	}

	// Case is not a second name. `vallic project list` prints machine names
	// in lower case and people type them either way.
	if got, err = chooseProject(linkable(), "ACME-SHOP"); err != nil || got.ID != 1 {
		t.Errorf("chooseProject() ignoring case = %v, %v", got, err)
	}
}

// Recording a name nothing answers to would be a link that fails on the next
// command instead of this one, so it is refused here, as usage, with the
// names that would have worked.
func TestChooseProjectRefusesANameAndListsWhatWouldWork(t *testing.T) {
	_, err := chooseProject(linkable(), "acme-shopp")
	if err == nil {
		t.Fatal(`chooseProject("acme-shopp") = nil error, want a refusal`)
	}

	var usage *UsageError
	if !errors.As(err, &usage) {
		t.Errorf("error = %T, want a *UsageError so the exit code is 2", err)
	}

	for _, want := range []string{"acme-shop", "storefront"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to list %q", err, want)
		}
	}
}

// With no projects the answer is not "no such project": the credential cannot
// see any, which is a different thing to go and fix.
func TestChooseProjectSaysWhenThereIsNothingToLinkTo(t *testing.T) {
	_, err := chooseProject(nil, "acme-shop")
	if err == nil {
		t.Fatal("chooseProject() with no projects = nil error, want a refusal")
	}

	if !strings.Contains(err.Error(), "no projects") {
		t.Errorf("error = %q, want it to say the credential sees none", err)
	}
}
