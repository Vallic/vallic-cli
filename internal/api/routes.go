package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Me returns who the credential belongs to.
//
// Also the cheapest way to verify a credential works, which is what `login`
// uses it for: a token that cannot reach this route is a token worth
// refusing before it is written to disk.
func (c *Client) Me(ctx context.Context) (*Me, error) {
	var out Me

	if err := c.get(ctx, "/me", &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// Teams lists the teams this credential reaches.
//
// One row for a personal access token, which is minted for a single team;
// every team the person belongs to for a credential that is not narrowed.
func (c *Client) Teams(ctx context.Context) ([]Team, error) {
	var out struct {
		Teams []Team `json:"teams"`
	}

	if err := c.get(ctx, "/teams", &out); err != nil {
		return nil, err
	}

	return out.Teams, nil
}

// Projects lists what the credential can see.
func (c *Client) Projects(ctx context.Context) ([]Project, error) {
	var out struct {
		Projects []Project `json:"projects"`
	}

	if err := c.get(ctx, "/projects", &out); err != nil {
		return nil, err
	}

	return out.Projects, nil
}

// Environments lists every environment the credential can see.
//
// Across projects, because that is what the route returns; callers that want
// one project's filter on Project.
func (c *Client) Environments(ctx context.Context) ([]Environment, error) {
	var out struct {
		Environments []Environment `json:"environments"`
	}

	if err := c.get(ctx, "/environments", &out); err != nil {
		return nil, err
	}

	return out.Environments, nil
}

// Environment reads one environment in full, including its SSH target.
func (c *Client) Environment(ctx context.Context, id int) (*EnvironmentDetail, error) {
	var out struct {
		Environment EnvironmentDetail `json:"environment"`
	}

	if err := c.get(ctx, fmt.Sprintf("/environments/%d", id), &out); err != nil {
		return nil, err
	}

	return &out.Environment, nil
}

// Servers lists the machines the credential can see.
func (c *Client) Servers(ctx context.Context) ([]Server, error) {
	var out struct {
		Servers []Server `json:"servers"`
	}

	if err := c.get(ctx, "/servers", &out); err != nil {
		return nil, err
	}

	return out.Servers, nil
}

// Task reads one task, so a script can wait for work to finish.
func (c *Client) Task(ctx context.Context, id int) (*Task, error) {
	var out struct {
		Task Task `json:"task"`
	}

	if err := c.get(ctx, fmt.Sprintf("/tasks/%d", id), &out); err != nil {
		return nil, err
	}

	return &out.Task, nil
}

// TaskLog reads a task's output from a byte offset.
func (c *Client) TaskLog(ctx context.Context, id int, offset int) (*TaskLog, error) {
	if offset < 0 {
		offset = 0
	}

	var out struct {
		Log TaskLog `json:"log"`
	}

	if err := c.get(ctx, fmt.Sprintf("/tasks/%d/log?offset=%d", id, offset), &out); err != nil {
		return nil, err
	}

	return &out.Log, nil
}

// Releases lists a project's builds, newest first.
//
// branch narrows to one branch's builds, and is empty for the project's. Empty
// leaves the parameter off rather than sending it blank, so the request a
// client makes without it is the one every client made before it existed.
func (c *Client) Releases(ctx context.Context, projectID int, limit int, branch string) ([]Release, error) {
	path := fmt.Sprintf("/projects/%d/releases", projectID)

	query := url.Values{}

	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}

	if branch != "" {
		query.Set("branch", branch)
	}

	if len(query) > 0 {
		path += "?" + query.Encode()
	}

	var out struct {
		Releases []Release `json:"releases"`
	}

	if err := c.get(ctx, path, &out); err != nil {
		return nil, err
	}

	return out.Releases, nil
}

// Rollback puts the release the current one replaced back.
func (c *Client) Rollback(ctx context.Context, environmentID int) (*Deployment, error) {
	return c.queueDeployment(ctx, fmt.Sprintf("/environments/%d/rollback", environmentID))
}

// Redeploy deploys what is already live, again.
func (c *Client) Redeploy(ctx context.Context, environmentID int) (*Deployment, error) {
	return c.queueDeployment(ctx, fmt.Sprintf("/environments/%d/redeploy", environmentID))
}

// queueDeployment posts to a route that answers with a queued deployment.
func (c *Client) queueDeployment(ctx context.Context, path string) (*Deployment, error) {
	var out struct {
		Deployment Deployment `json:"deployment"`
	}

	if err := c.post(ctx, path, nil, &out); err != nil {
		return nil, err
	}

	return &out.Deployment, nil
}

// Deploy queues a deployment.
//
// A zero release means "the newest one that is ready", which is what makes
// this scriptable; naming one is what makes it repeatable.
func (c *Client) Deploy(ctx context.Context, environmentID int, release int) (*Deployment, error) {
	body := map[string]any{}
	if release > 0 {
		body["release"] = release
	}

	var out struct {
		Deployment Deployment `json:"deployment"`
	}

	if err := c.post(ctx, fmt.Sprintf("/environments/%d/deploy", environmentID), body, &out); err != nil {
		return nil, err
	}

	return &out.Deployment, nil
}

// EnvironmentVariables lists everything in effect for an environment,
// inherited definitions included.
func (c *Client) EnvironmentVariables(ctx context.Context, environmentID int) ([]Variable, error) {
	var out struct {
		Variables []Variable `json:"variables"`
	}

	if err := c.get(ctx, fmt.Sprintf("/environments/%d/variables", environmentID), &out); err != nil {
		return nil, err
	}

	return out.Variables, nil
}

// ProjectVariables lists what a project defines, before any environment
// overrides it.
func (c *Client) ProjectVariables(ctx context.Context, projectID int) ([]Variable, error) {
	var out struct {
		Variables []Variable `json:"variables"`
	}

	if err := c.get(ctx, fmt.Sprintf("/projects/%d/variables", projectID), &out); err != nil {
		return nil, err
	}

	return out.Variables, nil
}

// EnvironmentVariable reads one variable as it is in effect here.
func (c *Client) EnvironmentVariable(ctx context.Context, environmentID int, name string) (*Variable, error) {
	var out struct {
		Variable Variable `json:"variable"`
	}

	path := fmt.Sprintf("/environments/%d/variables/%s", environmentID, url.PathEscape(name))

	if err := c.get(ctx, path, &out); err != nil {
		return nil, err
	}

	return &out.Variable, nil
}

// SetEnvironmentVariable creates or updates a variable on one environment.
func (c *Client) SetEnvironmentVariable(ctx context.Context, environmentID int, write VariableWrite) (*VariableWritten, error) {
	return c.setVariable(ctx, fmt.Sprintf("/environments/%d/variables", environmentID), write)
}

// SetProjectVariable creates or updates a variable on a project, where it
// reaches every environment that does not override it.
func (c *Client) SetProjectVariable(ctx context.Context, projectID int, write VariableWrite) (*VariableWritten, error) {
	return c.setVariable(ctx, fmt.Sprintf("/projects/%d/variables", projectID), write)
}

// setVariable posts to a route that answers with the written variable.
func (c *Client) setVariable(ctx context.Context, path string, write VariableWrite) (*VariableWritten, error) {
	var out VariableWritten

	if err := c.post(ctx, path, write, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// DeleteEnvironmentVariable removes an environment's own definition.
//
// Its own, and only its own: a variable inherited from the project is not
// defined here, and the control plane refuses rather than reaching up a scope
// and changing every other environment.
func (c *Client) DeleteEnvironmentVariable(ctx context.Context, environmentID int, name string) (*VariableRemoved, error) {
	path := fmt.Sprintf("/environments/%d/variables/%s", environmentID, url.PathEscape(name))

	var out VariableRemoved

	if err := c.delete(ctx, path, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// DeleteProjectVariable removes a project's definition.
func (c *Client) DeleteProjectVariable(ctx context.Context, projectID int, name string) (*VariableRemoved, error) {
	path := fmt.Sprintf("/projects/%d/variables/%s", projectID, url.PathEscape(name))

	var out VariableRemoved

	if err := c.delete(ctx, path, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// TaskFilter narrows the activity list.
type TaskFilter struct {
	// Types is the kinds to show. Several, because what somebody thinks of
	// as "a deploy" is more than one type on the platform, and a CLI that
	// offered only one at a time would make them ask three times.
	Types []string

	// Environment is an environment id, or zero for every one the credential
	// can see. The id rather than the slug, because `12` is a legal slug and
	// a parameter whose meaning depends on whether it looks numeric is a
	// parameter nobody can document.
	Environment int

	// Before is a cursor: only tasks older than this id. Zero for the first
	// page.
	Before int

	// Limit is how many rows. Zero takes the control plane's default of 25;
	// anything above its ceiling is clamped there rather than refused.
	Limit int
}

// Tasks lists what the platform has been doing.
func (c *Client) Tasks(ctx context.Context, filter TaskFilter) (*TaskPage, error) {
	query := url.Values{}

	if len(filter.Types) > 0 {
		query.Set("type", strings.Join(filter.Types, ","))
	}

	if filter.Environment > 0 {
		query.Set("environment", strconv.Itoa(filter.Environment))
	}

	if filter.Before > 0 {
		query.Set("before", strconv.Itoa(filter.Before))
	}

	if filter.Limit > 0 {
		query.Set("limit", strconv.Itoa(filter.Limit))
	}

	path := "/tasks"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}

	var out TaskPage

	if err := c.get(ctx, path, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// Logs reads where an environment's logs go.
func (c *Client) Logs(ctx context.Context, environmentID int) (*Logs, error) {
	var out Logs

	if err := c.get(ctx, fmt.Sprintf("/environments/%d/logs", environmentID), &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// Services reads what an environment runs.
func (c *Client) Services(ctx context.Context, environmentID int) (*Stack, error) {
	var out struct {
		Stack Stack `json:"stack"`
	}

	if err := c.get(ctx, fmt.Sprintf("/environments/%d/services", environmentID), &out); err != nil {
		return nil, err
	}

	return &out.Stack, nil
}

// Domains lists every hostname claimed for an environment, verified or not.
//
// Unverified ones included, deliberately: this is the route that answers "why
// does my hostname not work", and the filtered list is the environment's own
// `hostnames.routed`.
func (c *Client) Domains(ctx context.Context, environmentID int) (*DomainList, error) {
	var out DomainList

	if err := c.get(ctx, fmt.Sprintf("/environments/%d/domains", environmentID), &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// AddDomain claims a hostname for an environment.
//
// Only the hostname is sent. Everything else about a domain — its TLS mode,
// whether a CDN fronts it, whether it is an apex, its verification token — is
// derived by the control plane from the hostname and the environment, and is
// refused rather than ignored if a client sends it.
func (c *Client) AddDomain(ctx context.Context, environmentID int, hostname string) (*Domain, error) {
	var out struct {
		Domain Domain `json:"domain"`
	}

	body := map[string]any{"hostname": hostname}

	if err := c.post(ctx, fmt.Sprintf("/environments/%d/domains", environmentID), body, &out); err != nil {
		return nil, err
	}

	return &out.Domain, nil
}

// VerifyDomain checks a hostname's DNS records.
//
// Synchronous: the control plane does the lookups inline and answers with the
// result, so there is no task to follow. It answers 200 even when the check
// failed, because nothing about the request was wrong and the answer is "not
// yet" — a retry loop that exits non-zero on that is a retry loop that works
// correctly and reports failure.
func (c *Client) VerifyDomain(ctx context.Context, domainID int) (*VerificationResult, error) {
	var out VerificationResult

	if err := c.post(ctx, fmt.Sprintf("/domains/%d/verify", domainID), nil, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// DeleteDomain removes a hostname.
func (c *Client) DeleteDomain(ctx context.Context, domainID int) (*DomainRemoved, error) {
	var out DomainRemoved

	if err := c.delete(ctx, fmt.Sprintf("/domains/%d", domainID), &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// CreateEnvironment raises a new environment on a project.
//
// It answers with a draft: the environment exists, has no machines, and will
// refuse a deploy until somebody builds it. Creating is not building, and the
// API deliberately does not spend a customer's money on machines they did not
// ask for in the same breath.
func (c *Client) CreateEnvironment(ctx context.Context, projectID int, create EnvironmentCreate) (*EnvironmentDetail, error) {
	var out struct {
		Environment EnvironmentDetail `json:"environment"`
	}

	if err := c.post(ctx, fmt.Sprintf("/projects/%d/environments", projectID), create, &out); err != nil {
		return nil, err
	}

	return &out.Environment, nil
}

// PatchEnvironment changes the branch an environment builds from, or whether a
// push to it deploys.
//
// Answers with the whole environment rather than the two changed fields, so a
// client parses one shape for this and for the read.
func (c *Client) PatchEnvironment(ctx context.Context, environmentID int, patch EnvironmentPatch) (*EnvironmentDetail, error) {
	body, err := json.Marshal(patch)
	if err != nil {
		return nil, err
	}

	var out struct {
		Environment EnvironmentDetail `json:"environment"`
	}

	if err := c.do(ctx, http.MethodPatch, fmt.Sprintf("/environments/%d", environmentID), body, &out); err != nil {
		return nil, err
	}

	return &out.Environment, nil
}

// DeleteEnvironment removes an environment and everything that follows it.
//
// Production is refused at every role, by the control plane. The way to remove
// a live site is to archive its project, which destroys the machines and keeps
// the backups.
func (c *Client) DeleteEnvironment(ctx context.Context, environmentID int) (*EnvironmentDeleted, error) {
	var out EnvironmentDeleted

	if err := c.delete(ctx, fmt.Sprintf("/environments/%d", environmentID), &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// Backups reads what exists for an environment.
//
// A plain read: it queues nothing. Listings are refreshed behind every backup
// and every removal rather than on request, because a list that is only
// current when somebody presses something is a list that is wrong the morning
// after a backup ran.
func (c *Client) Backups(ctx context.Context, environmentID int) (*BackupListing, error) {
	var out struct {
		Backups BackupListing `json:"backups"`
	}

	if err := c.get(ctx, fmt.Sprintf("/environments/%d/backups", environmentID), &out); err != nil {
		return nil, err
	}

	return &out.Backups, nil
}

// TakeBackup asks for a backup now.
//
// kind is required rather than defaulted: a database dump is minutes and a
// files sync is hours, so they are genuinely different acts and a client
// should say which one it means.
func (c *Client) TakeBackup(ctx context.Context, environmentID int, kind string) (*BackupQueued, error) {
	var out BackupQueued

	body := map[string]any{"kind": kind}

	if err := c.post(ctx, fmt.Sprintf("/environments/%d/backups", environmentID), body, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// DownloadBackup asks for a fetchable copy of a snapshot.
//
// Called twice in the normal case: the first queues the export and answers
// with no URL, and a later call answers with one once the work has finished.
// The kind is not sent, deliberately: the listing is the authority on what a
// snapshot holds, and a mismatched kind would put a database dump through the
// path that unpacks an archive over a site's files.
func (c *Client) DownloadBackup(ctx context.Context, environmentID int, snapshot string) (*BackupDownload, error) {
	var out BackupDownload

	path := fmt.Sprintf("/environments/%d/backups/%s/download", environmentID, url.PathEscape(snapshot))

	if err := c.post(ctx, path, nil, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// RestoreBackup puts a snapshot back over the environment it came from.
//
// The same environment on both sides: this is recovery, not a copy between
// environments. Restoring production's data into staging is a different act
// with a different threat model, and it is not reachable from here.
func (c *Client) RestoreBackup(ctx context.Context, environmentID int, snapshot string, kind string) (*RestoreQueued, error) {
	var out RestoreQueued

	body := map[string]any{"kind": kind}

	path := fmt.Sprintf("/environments/%d/backups/%s/restore", environmentID, url.PathEscape(snapshot))

	if err := c.post(ctx, path, body, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// Validate checks a manifest's syntax and schema.
//
// It reaches no git host and touches no environment, which is what makes it
// usable on a file in a working tree before anything is committed, and before
// the project exists at all. The cost is that it can only answer half the
// question: a pass here is not a promise that a deploy succeeds, which is why
// the answer carries a nil `checked_against` rather than leaving that implied.
func (c *Client) Validate(ctx context.Context, manifest string) (*Validation, error) {
	return c.validate(ctx, "/validate", manifest)
}

// ValidateAgainst checks a manifest against one environment.
//
// The thorough answer: it compares what the file asks for against what the
// environment runs, what the catalogue offers and which variables are set. A
// pass here is the same expression a deploy branches on.
func (c *Client) ValidateAgainst(ctx context.Context, environmentID int, manifest string) (*Validation, error) {
	return c.validate(ctx, fmt.Sprintf("/environments/%d/validate", environmentID), manifest)
}

func (c *Client) validate(ctx context.Context, path string, manifest string) (*Validation, error) {
	var out struct {
		Validation Validation `json:"validation"`
	}

	if err := c.post(ctx, path, map[string]any{"manifest": manifest}, &out); err != nil {
		return nil, err
	}

	return &out.Validation, nil
}

// LatestCLI reads the version of this binary the control plane publishes.
//
// The bytes are not here. The answer carries an address at the release host
// and the digest the control plane holds for what is at it, and self-update
// fetches one and checks it against the other. Asking a single system for both
// would prove only that the download arrived intact, which is not the question
// a client replacing its own executable is asking.
//
// No envelope, unlike the routes around it, because this is the agent's
// release manifest read by a different audience: two shapes for one mechanism
// would be two parsers to keep in step.
func (c *Client) LatestCLI(ctx context.Context, channel string) (*CLIRelease, error) {
	var out CLIRelease

	path := "/cli/latest"

	// Asked for only when it is wanted. An empty channel means "whatever this
	// installation serves", which is the ordinary case and is also the request
	// every older client makes -- so the parameter is absent rather than
	// present and blank, and one URL stays one cache entry.
	if channel != "" {
		path += "?channel=" + url.QueryEscape(channel)
	}

	// Without a credential where there is none. This is the one route a
	// client asks before it has signed in, and the one it may need most when
	// signing in is what has stopped working.
	if err := c.getPublic(ctx, path, &out); err != nil {
		return nil, err
	}

	return &out, nil
}
