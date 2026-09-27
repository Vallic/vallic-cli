package resolve

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vallic/vallic-cli/internal/api"
)

// A remote is written at least four ways and the API stores one of them.
// Comparing anything but the last two segments fails to match a checkout
// cloned over a different protocol to the one the project was added with,
// which is most of them.
func TestNormaliseRemote(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"git@github.com:acme/shop.git", "acme/shop"},
		{"git@github.com:acme/shop", "acme/shop"},
		{"https://github.com/acme/shop.git", "acme/shop"},
		{"https://github.com/acme/shop", "acme/shop"},
		{"ssh://git@github.com/acme/shop.git", "acme/shop"},
		{"https://github.com/acme/shop/", "acme/shop"},
		{"acme/shop", "acme/shop"},

		// A port is not a path separator. Without the distinction this
		// becomes "22/acme/shop" and matches nothing.
		{"ssh://git@git.example.com:2222/acme/shop.git", "acme/shop"},

		// A self-hosted host with a nested group keeps only the last pair,
		// which is what the API stores.
		{"https://gitlab.example.com/group/sub/acme/shop.git", "acme/shop"},

		{"", ""},
		{"shop", ""},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := normaliseRemote(tc.in); got != tc.want {
				t.Errorf("normaliseRemote(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The rule that matters most: an unrecognised branch does not fall through to
// production. Deploying the most important environment because a branch name
// was not understood is the single worst thing this package could do.
func TestUnmatchedBranchDoesNotFallThroughToProduction(t *testing.T) {
	client := &fakeClient{
		projects: []api.Project{{ID: 1, MachineName: "webshop"}},
		environments: []api.Environment{
			{ID: 10, Project: 1, Name: "production", Slug: "webshop-production", GitRef: "main"},
			{ID: 11, Project: 1, Name: "staging", Slug: "webshop-staging", GitRef: "develop"},
		},
	}

	// No directory, so there is no git branch to match — the same position a
	// checkout on an unrecognised branch is in.
	r := &Resolver{Client: client, Dir: t.TempDir()}

	_, err := r.Environment(context.Background(), &client.projects[0], "")

	var ambiguous *Ambiguous
	if !errors.As(err, &ambiguous) {
		t.Fatalf("resolved to something on an unmatched branch: %v", err)
	}

	if ambiguous.What != "environment" {
		t.Errorf("What = %q, want environment", ambiguous.What)
	}

	// The candidates are the only useful thing to print. "Which
	// environment?" with no list is a question nobody can answer.
	joined := strings.Join(ambiguous.Candidates, ",")
	if !strings.Contains(joined, "production") || !strings.Contains(joined, "staging") {
		t.Errorf("Candidates = %v, want both environments listed", ambiguous.Candidates)
	}

	if !strings.Contains(ambiguous.Error(), "--environment") {
		t.Errorf("Error() = %q, want it to name the flag that would settle it", ambiguous.Error())
	}
}

// A single environment needs no disambiguation: there is only one thing the
// command could mean.
func TestASingleEnvironmentIsChosen(t *testing.T) {
	client := &fakeClient{
		projects: []api.Project{{ID: 1, MachineName: "webshop"}},
		environments: []api.Environment{
			{ID: 10, Project: 1, Name: "production", GitRef: "main"},
		},
	}

	r := &Resolver{Client: client, Dir: t.TempDir()}

	env, err := r.Environment(context.Background(), &client.projects[0], "")
	if err != nil {
		t.Fatalf("Environment: %v", err)
	}

	if env.ID != 10 {
		t.Errorf("ID = %d, want 10", env.ID)
	}
}

// A positional argument is the most explicit thing there is and wins over
// every hint, including one set in the configuration file.
func TestPositionalBeatsTheHint(t *testing.T) {
	client := &fakeClient{
		projects: []api.Project{{ID: 1, MachineName: "webshop"}},
		environments: []api.Environment{
			{ID: 10, Project: 1, Name: "production", GitRef: "main"},
			{ID: 11, Project: 1, Name: "staging", GitRef: "develop"},
		},
	}

	r := &Resolver{Client: client, Dir: t.TempDir(), EnvironmentHint: "production"}

	env, err := r.Environment(context.Background(), &client.projects[0], "staging")
	if err != nil {
		t.Fatalf("Environment: %v", err)
	}

	if env.Name != "staging" {
		t.Errorf("Name = %q, want staging", env.Name)
	}
}

// Environments of another project are not candidates, so "the only
// environment" means the only one of this project and a slug that exists
// twice is not a coin toss.
func TestEnvironmentsAreNarrowedToTheProject(t *testing.T) {
	client := &fakeClient{
		projects: []api.Project{
			{ID: 1, MachineName: "webshop"},
			{ID: 2, MachineName: "blog"},
		},
		environments: []api.Environment{
			{ID: 10, Project: 1, Name: "production", GitRef: "main"},
			{ID: 20, Project: 2, Name: "production", GitRef: "main"},
		},
	}

	r := &Resolver{Client: client, Dir: t.TempDir()}

	env, err := r.Environment(context.Background(), &client.projects[1], "")
	if err != nil {
		t.Fatalf("Environment: %v", err)
	}

	if env.ID != 20 {
		t.Errorf("ID = %d, want 20 — the blog's production, not the webshop's", env.ID)
	}
}

// A name that matches nothing is refused with the list, rather than silently
// resolving to something near it.
func TestAnUnknownNameIsRefused(t *testing.T) {
	client := &fakeClient{
		projects: []api.Project{{ID: 1, MachineName: "webshop"}},
		environments: []api.Environment{
			{ID: 10, Project: 1, Name: "production", GitRef: "main"},
		},
	}

	r := &Resolver{Client: client, Dir: t.TempDir()}

	_, err := r.Environment(context.Background(), &client.projects[0], "prod")

	var ambiguous *Ambiguous
	if !errors.As(err, &ambiguous) {
		t.Fatalf("error = %v, want an *Ambiguous", err)
	}

	if !strings.Contains(ambiguous.Because, `"prod"`) {
		t.Errorf("Because = %q, want the name that did not match", ambiguous.Because)
	}
}

// A project is matched from the git remote, which is the whole reason
// --project is rarely needed.
func TestProjectIsMatchedByRepository(t *testing.T) {
	client := &fakeClient{
		projects: []api.Project{
			{ID: 1, MachineName: "blog", Repository: repo("acme/blog")},
			{ID: 2, MachineName: "webshop", Repository: repo("acme/shop")},
		},
	}

	// The resolution is driven through the hint rather than a real git
	// checkout, and the remote matching itself is covered by
	// TestNormaliseRemote and here through projectByRepository.
	if got := projectByRepository(client.projects, "git@github.com:acme/shop.git"); got == nil || got.ID != 2 {
		t.Fatalf("projectByRepository matched %v, want the webshop", got)
	}

	if got := projectByRepository(client.projects, "git@github.com:acme/other.git"); got != nil {
		t.Errorf("projectByRepository matched %v, want no match", got)
	}
}

// Two projects and nothing to choose between them is a question, not a guess.
func TestAmbiguousProjectIsRefused(t *testing.T) {
	client := &fakeClient{
		projects: []api.Project{
			{ID: 1, MachineName: "blog"},
			{ID: 2, MachineName: "webshop"},
		},
	}

	r := &Resolver{Client: client, Dir: t.TempDir()}

	_, err := r.Project(context.Background())

	var ambiguous *Ambiguous
	if !errors.As(err, &ambiguous) {
		t.Fatalf("error = %v, want an *Ambiguous", err)
	}

	if len(ambiguous.Candidates) != 2 {
		t.Errorf("Candidates = %v, want both projects", ambiguous.Candidates)
	}
}

func repo(fullName string) *struct {
	Provider string `json:"provider"`
	FullName string `json:"full_name"`
} {
	return &struct {
		Provider string `json:"provider"`
		FullName string `json:"full_name"`
	}{Provider: "github", FullName: fullName}
}

type fakeClient struct {
	projects     []api.Project
	environments []api.Environment
}

func (f *fakeClient) Projects(context.Context) ([]api.Project, error) {
	return f.projects, nil
}

func (f *fakeClient) Environments(context.Context) ([]api.Environment, error) {
	return f.environments, nil
}
