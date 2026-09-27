// Package resolve works out which project and environment a command means.
//
// The CLI should almost never need --project or --environment, and the data
// to avoid both is already on the API: a project carries its repository's
// full name, and an environment carries the branch it builds from. So a
// checkout identifies itself, with no local state and nothing to link.
//
// Two rules shape everything here.
//
// It never falls through to production. A branch that matches no environment
// is not a reason to act on the most important one; it is a reason to stop
// and say which environments exist. The default target of an unqualified
// deploy from an unrecognised branch is nothing at all.
//
// It never prompts in CI. With no terminal, every rung that would have asked
// is an error naming the flag and the variable that would have settled it. A
// CLI that hangs on a prompt in a pipeline hangs for twenty minutes and then
// fails on a timeout.
package resolve

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/vallic/vallic-cli/internal/api"
)

// Client is the part of the API client this package needs.
//
// An interface so the resolution can be tested without a control plane —
// this is the code most likely to pick the wrong environment, and the code
// where picking the wrong one is worst.
type Client interface {
	Projects(ctx context.Context) ([]api.Project, error)
	Environments(ctx context.Context) ([]api.Environment, error)
}

// Ambiguous is returned when nothing in the checkout or the flags settles it.
//
// It carries the candidates so the caller can list them, which is the only
// useful thing to print: "which environment?" with no list is a question
// nobody can answer.
type Ambiguous struct {
	// What is "project" or "environment".
	What string

	// Candidates are the names that would have worked.
	Candidates []string

	// Because is why the CLI could not decide, in a clause.
	Because string
}

func (e *Ambiguous) Error() string {
	var b strings.Builder

	fmt.Fprintf(&b, "cannot tell which %s you mean: %s", e.What, e.Because)

	if len(e.Candidates) > 0 {
		fmt.Fprintf(&b, "\n  try one of: %s", strings.Join(e.Candidates, ", "))
	}

	fmt.Fprintf(&b, "\n  or name it: --%s <name>", e.What)

	// Only for a project, because `link` records only a project. Offering it
	// for an environment would point somebody at a command that cannot fix
	// what they just hit.
	if e.What == "project" {
		fmt.Fprint(&b, "\n  or record it for this checkout: vallic link <name>")
	}

	return b.String()
}

// Resolver turns flags plus a checkout into a project and an environment.
type Resolver struct {
	Client Client

	// Dir is the working directory whose git remote and branch are read.
	Dir string

	// ProjectHint and EnvironmentHint are the flag, then the environment
	// variable, then the config file — settled by the caller before it gets
	// here, because the precedence is the same for both and belongs in one
	// place.
	ProjectHint     string
	EnvironmentHint string
}

// Project picks the project.
func (r *Resolver) Project(ctx context.Context) (*api.Project, error) {
	projects, err := r.Client.Projects(ctx)
	if err != nil {
		return nil, err
	}

	if len(projects) == 0 {
		return nil, fmt.Errorf("this credential can see no projects")
	}

	if r.ProjectHint != "" {
		return matchProject(projects, r.ProjectHint)
	}

	// The git remote, which is the whole point: a checkout knows what it is.
	if remote := gitRemote(r.Dir); remote != "" {
		if match := projectByRepository(projects, remote); match != nil {
			return match, nil
		}
	}

	if len(projects) == 1 {
		return &projects[0], nil
	}

	return nil, &Ambiguous{
		What:       "project",
		Candidates: projectNames(projects),
		Because:    because(r.Dir, "no git remote here matches a project you can see"),
	}
}

// Environment picks the environment, within a project.
func (r *Resolver) Environment(ctx context.Context, project *api.Project, positional string) (*api.Environment, error) {
	all, err := r.Client.Environments(ctx)
	if err != nil {
		return nil, err
	}

	// Narrowed to the project first, so a slug that exists in two projects
	// is not a coin toss and "the only environment" means the only one of
	// this project.
	var environments []api.Environment
	for _, env := range all {
		if project == nil || env.Project == project.ID {
			environments = append(environments, env)
		}
	}

	if len(environments) == 0 {
		return nil, fmt.Errorf("%s has no environments", project.Label)
	}

	// A positional argument is the most explicit thing there is, so it wins
	// over every hint.
	if positional != "" {
		return matchEnvironment(environments, positional)
	}

	if r.EnvironmentHint != "" {
		return matchEnvironment(environments, r.EnvironmentHint)
	}

	if branch := gitBranch(r.Dir); branch != "" {
		var matches []api.Environment
		for _, env := range environments {
			if env.GitRef == branch {
				matches = append(matches, env)
			}
		}

		if len(matches) == 1 {
			return &matches[0], nil
		}

		if len(matches) > 1 {
			// Two environments of one project tracking one branch. The
			// console stops this happening, so it means the data is odd
			// rather than the command.
			return nil, &Ambiguous{
				What:       "environment",
				Candidates: environmentNames(matches),
				Because:    fmt.Sprintf("%d environments track the branch %q", len(matches), branch),
			}
		}

		if len(environments) > 1 {
			// Deliberately not falling through to production. See the
			// package comment.
			return nil, &Ambiguous{
				What:       "environment",
				Candidates: environmentNames(environments),
				Because:    fmt.Sprintf("no environment tracks the branch %q", branch),
			}
		}
	}

	if len(environments) == 1 {
		return &environments[0], nil
	}

	return nil, &Ambiguous{
		What:       "environment",
		Candidates: environmentNames(environments),
		Because:    because(r.Dir, "this is not a git checkout, so there is no branch to match"),
	}
}

// because picks the explanation that fits whether this is a checkout at all.
func because(dir, whenCheckout string) string {
	if gitBranch(dir) == "" && gitRemote(dir) == "" {
		return "this is not a git checkout, so there is nothing to match"
	}

	return whenCheckout
}

// matchProject finds a project by machine name, label or id.
func matchProject(projects []api.Project, want string) (*api.Project, error) {
	if id, err := strconv.Atoi(want); err == nil {
		for i := range projects {
			if projects[i].ID == id {
				return &projects[i], nil
			}
		}
	}

	for i := range projects {
		if strings.EqualFold(projects[i].MachineName, want) {
			return &projects[i], nil
		}
	}

	for i := range projects {
		if strings.EqualFold(projects[i].Label, want) {
			return &projects[i], nil
		}
	}

	return nil, &Ambiguous{
		What:       "project",
		Candidates: projectNames(projects),
		Because:    fmt.Sprintf("no project is called %q", want),
	}
}

// matchEnvironment finds an environment by slug, name or id.
//
// The name is matched too, because the slug is
// `{project}-{environment}` and nobody types that: within a project,
// `production` is unambiguous and is what people mean.
func matchEnvironment(environments []api.Environment, want string) (*api.Environment, error) {
	if id, err := strconv.Atoi(want); err == nil {
		for i := range environments {
			if environments[i].ID == id {
				return &environments[i], nil
			}
		}
	}

	for i := range environments {
		if strings.EqualFold(environments[i].Slug, want) {
			return &environments[i], nil
		}
	}

	for i := range environments {
		if strings.EqualFold(environments[i].Name, want) {
			return &environments[i], nil
		}
	}

	return nil, &Ambiguous{
		What:       "environment",
		Candidates: environmentNames(environments),
		Because:    fmt.Sprintf("no environment is called %q", want),
	}
}

// projectByRepository matches a git remote against a project's repository.
func projectByRepository(projects []api.Project, remote string) *api.Project {
	want := normaliseRemote(remote)
	if want == "" {
		return nil
	}

	for i := range projects {
		if projects[i].Repository == nil {
			continue
		}

		if strings.EqualFold(normaliseRemote(projects[i].Repository.FullName), want) {
			return &projects[i]
		}
	}

	return nil
}

// normaliseRemote reduces a git remote to `owner/name`.
//
// The same repository is written at least four ways —
// git@github.com:acme/shop.git, https://github.com/acme/shop.git,
// ssh://git@github.com/acme/shop, acme/shop — and the API stores the last.
// Comparing anything but the last pair of segments would fail to match a
// checkout cloned over a different protocol to the one the project was
// added with, which is most of them.
func normaliseRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}

	remote = strings.TrimSuffix(remote, ".git")
	remote = strings.TrimSuffix(remote, "/")

	// scp-style, which has no scheme and a colon instead of a slash.
	if i := strings.LastIndex(remote, ":"); i != -1 && !strings.Contains(remote[i:], "/") {
		// A port, not a path separator — ssh://host:22/owner/name.
		remote = remote[:i] + "/" + remote[i+1:]
	} else if i != -1 {
		remote = remote[i+1:]
	}

	parts := strings.Split(remote, "/")
	if len(parts) < 2 {
		return ""
	}

	return strings.Join(parts[len(parts)-2:], "/")
}

func gitRemote(dir string) string {
	return git(dir, "remote", "get-url", "origin")
}

func gitBranch(dir string) string {
	branch := git(dir, "rev-parse", "--abbrev-ref", "HEAD")

	// A detached HEAD has no branch to match, and "HEAD" is not a branch
	// name any environment tracks.
	if branch == "HEAD" {
		return ""
	}

	return branch
}

// git runs a read-only git command, and treats every failure as "no answer".
//
// Not having git, not being in a checkout and having a repository with no
// commits are all the same thing here: there is nothing to infer from, and
// the resolution falls to the next rung.
func git(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir

	out, err := cmd.Output()
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(out))
}

func projectNames(projects []api.Project) []string {
	names := make([]string, 0, len(projects))
	for _, p := range projects {
		names = append(names, p.MachineName)
	}
	sort.Strings(names)

	return names
}

func environmentNames(environments []api.Environment) []string {
	names := make([]string, 0, len(environments))
	for _, e := range environments {
		name := e.Name
		if name == "" {
			name = e.Slug
		}
		names = append(names, name)
	}
	sort.Strings(names)

	return names
}
