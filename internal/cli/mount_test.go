package cli

import (
	"strings"
	"testing"

	"github.com/vallic/vallic-cli/internal/api"
)

// areas is an environment with all three kinds.
func areas() *api.SSHTarget {
	return &api.SSHTarget{
		FilesPath: "/mnt/files/public",
		WritablePaths: []api.WritablePath{
			{Name: "public", Path: "/mnt/files/public", Kind: "public"},
			{Name: "private", Path: "/mnt/files/private", Kind: "private"},
			{Name: "mounts/uploads", Path: "/mnt/files/mounts/uploads", Kind: "shared"},
		},
	}
}

// Each kind resolves to the path the platform puts it at.
func TestEachKindOfAreaResolves(t *testing.T) {
	for _, tc := range []struct{ area, want string }{
		{"public", "/mnt/files/public"},
		{"private", "/mnt/files/private"},
		{"mounts/uploads", "/mnt/files/mounts/uploads"},

		// Empty is public, so an unset flag behaves as the command always did.
		{"", "/mnt/files/public"},

		// A trailing slash is what somebody pastes out of `mount list` or a
		// shell completion, and it must not turn into a different area.
		{"private/", "/mnt/files/private"},
	} {
		t.Run(tc.area, func(t *testing.T) {
			got, err := resolveArea(areas(), tc.area)
			if err != nil {
				t.Fatalf("resolveArea(%q): %v", tc.area, err)
			}

			if got != tc.want {
				t.Errorf("resolveArea(%q) = %q, want %q", tc.area, got, tc.want)
			}
		})
	}
}

// An unknown area is refused, and the refusal lists what there is.
//
// Refused rather than assembled optimistically: a path built from a name the
// control plane never sent is an rsync against a directory that is not there,
// and rsync reports that as a protocol error rather than as the one sentence
// that would have helped.
func TestAnUnknownAreaIsRefusedWithTheAlternatives(t *testing.T) {
	_, err := resolveArea(areas(), "nope")
	if err == nil {
		t.Fatal("an area nobody has resolved to something")
	}

	for _, want := range []string{"public", "private", "mounts/uploads"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to list %q", err, want)
		}
	}
}

// A mount named without its prefix gets the spelling, not the list.
//
// The near-miss worth its own sentence: somebody reads `uploads` in their
// vallic.yaml and types that, and a list of three names does not tell them
// which of the three they meant.
func TestAMountNamedWithoutItsPrefixIsCorrected(t *testing.T) {
	_, err := resolveArea(areas(), "uploads")
	if err == nil {
		t.Fatal("uploads resolved, but the area is mounts/uploads")
	}

	if !strings.Contains(err.Error(), "mounts/uploads") {
		t.Errorf("err = %q, want it to give the spelling", err)
	}
}

// An older control plane sends no list. Public still works, from the field it
// has always sent; anything else is an honest refusal rather than a guess.
func TestAnOlderControlPlaneStillAllowsPublicAndNothingElse(t *testing.T) {
	old := &api.SSHTarget{FilesPath: "/mnt/files/public"}

	got, err := resolveArea(old, "public")
	if err != nil {
		t.Fatalf("public against an older control plane: %v", err)
	}

	if got != "/mnt/files/public" {
		t.Errorf("got %q, want the files_path it sent", got)
	}

	if _, err := resolveArea(old, "private"); err == nil {
		t.Error("private resolved against a control plane that never said where it is")
	}
}

// A declared mount is only offered once it has been deployed.
//
// The control plane sends the *deployed* release's mounts, so this asserts the
// client side of that: a name it was not sent is refused, which is what stops
// `mount upload --area mounts/new` from rsyncing into a directory no deploy has
// created yet.
func TestAMountThatIsNotDeployedYetIsNotOffered(t *testing.T) {
	deployed := areas()

	if _, err := resolveArea(deployed, "mounts/added-but-not-deployed"); err == nil {
		t.Error("a mount the environment does not have was accepted")
	}
}
