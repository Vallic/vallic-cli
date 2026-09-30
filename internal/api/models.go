package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The control plane's own field names, unabbreviated. See the package comment
// on why nothing is renamed on the way through.

// Me is who a credential belongs to and what it can reach.
type Me struct {
	User struct {
		ID    int    `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"user"`

	// Team is nil for a credential that is not narrowed to one — which is
	// what an OAuth token is, and what a personal access token never is.
	Team *struct {
		ID    int    `json:"id"`
		Label string `json:"label"`
	} `json:"team"`

	Token *struct {
		ID      int    `json:"id"`
		Label   string `json:"label"`
		Expires *int64 `json:"expires"`
	} `json:"token"`
}

// Team is one team a credential reaches.
type Team struct {
	ID int `json:"id"`

	// MachineName is what somebody types and what the console's URLs are
	// built from. The label is what they read, and an owner can rename it —
	// so a client matches on this.
	MachineName string `json:"machine_name"`
	Label       string `json:"label"`

	// Role is what the person holds here, so a client can say what they may
	// do before they try it.
	Role string `json:"role"`
}

// Project is one project of a team.
type Project struct {
	ID            int    `json:"id"`
	Label         string `json:"label"`
	MachineName   string `json:"machine_name"`
	DefaultBranch string `json:"default_branch"`

	// Repository is what a checkout is matched against to work out which
	// project it is, which is why the CLI almost never needs --project.
	Repository *struct {
		Provider string `json:"provider"`
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// Environment is one environment, as the list route returns it.
type Environment struct {
	ID      int    `json:"id"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	State   string `json:"state"`
	GitRef  string `json:"git_ref"`
	Project int    `json:"project"`
}

// EnvironmentDetail is one environment, with what is needed to act on it.
type EnvironmentDetail struct {
	Environment

	CurrentRelease *struct {
		ID int `json:"id"`

		// Number is the project's own sequence, and the one to print: the id
		// is global, so showing "release 3339" for what the project calls
		// release 7 offers a number that looks like the one somebody would
		// type back and is not.
		//
		// It is also what `--release` takes, but the deploy route does not:
		// that loads by entity id, so the CLI looks the number up and sends
		// the id. See releaseIDFor.
		Number int `json:"number"`

		GitSHA string `json:"git_sha"`
		GitRef string `json:"git_ref"`
	} `json:"current_release"`

	AutoDeploy bool `json:"auto_deploy"`
	Protected  bool `json:"protected"`

	// SSH is nil where the caller's role does not admit a shell, or where
	// nothing runs the environment yet. Nil rather than absent is the
	// control plane's decision and a meaningful one: it is a plain no, not a
	// field this version of the API forgot.
	SSH *SSHTarget `json:"ssh"`

	// Hostnames is what the environment answers on. Nil only against a
	// control plane that predates the member.
	Hostnames *Hostnames `json:"hostnames"`
}

// Hostnames is where an environment can be reached.
type Hostnames struct {
	// Platform is the address issued with the environment, which always
	// works and is never removed while it lives. Covered by the platform
	// wildcard certificate, so it always resolves and always has TLS.
	Platform string `json:"platform"`

	// Primary is the canonical hostname others redirect to, or nil. Nil is
	// reachable: deleting the canonical domain leaves none, so a client has
	// to handle it rather than assume one exists.
	Primary *string `json:"primary"`

	// Routed is every name the site actually answers on, which is the same
	// list the edge proxy's routing rule is built from. Unverified custom
	// domains are deliberately absent: a name in this list answers, a name
	// missing from it does not, whatever the customer typed into DNS.
	Routed []string `json:"routed"`

	// URL is the browsable address, scheme included. Sent whole rather than
	// assembled here, because the scheme is a control-plane fact — which of
	// several TLS modes terminates the connection is not a client's to
	// guess.
	URL string `json:"url"`
}

// SSHTarget is where to point ssh, rsync and a database client.
//
// Every field is one a client must not derive. The port especially: it is
// chosen at random per project in 2000-2999 and never changes, so there is no
// constant to fall back on and a CLI that guessed would be wrong for every
// project on the platform.
type SSHTarget struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`

	// DBExport, DBImport and DBCLI are the verbs the forced command expands
	// inside the container, or empty where the stack runs no database. Sent
	// rather than assumed, so the CLI neither offers `db export` for an
	// environment with nothing to export nor carries the platform's
	// vocabulary compiled into it.
	DBExport string `json:"db_export"`
	DBImport string `json:"db_import"`
	DBCLI    string `json:"db_cli"`

	// FilesPath is the one writable directory that is the same for every
	// project type and survives every deploy. What rsync should be aimed at.
	FilesPath string `json:"files_path"`

	// HasKeys is whether the person asking has a public key on file. Without
	// one the command is correct and still refused, and the fix is on their
	// profile rather than in what they typed.
	HasKeys bool `json:"has_keys"`

	// WritablePaths is every area under the environment's shared root: the
	// public files directory, the private one, and whatever the deployed
	// release's vallic.yaml asked to keep. Empty against a control plane that
	// predates the field, which is why FilesPath is still read on its own
	// rather than looked up in here.
	WritablePaths []WritablePath `json:"writable_paths"`
}

// WritablePath is one place on the far side that survives a deploy.
type WritablePath struct {
	// Name is what the platform calls it and what a person passes to --area:
	// "public", "private", or "mounts/<name>" for one the application declared.
	// One vocabulary for the wire, the disk and the command line.
	Name string `json:"name"`

	// Path is the absolute path inside the container.
	Path string `json:"path"`

	// Kind is "public", "private" or "shared". Sent rather than inferred from
	// the name's prefix, so a client grouping these does not parse a path to
	// learn which ones the application asked for.
	Kind string `json:"kind"`
}

// WritablePathNames is what --area accepts, in the order it was sent.
func (t *SSHTarget) WritablePathNames() []string {
	names := make([]string, 0, len(t.WritablePaths))

	for i := range t.WritablePaths {
		names = append(names, t.WritablePaths[i].Name)
	}

	return names
}

// WritablePathFor resolves an area name to its path.
//
// Falls back to FilesPath for "public" alone, and only where the control plane
// sent no list at all: that is an older control plane, where public is the one
// area a client could ever reach and the field it came from is still there.
// Every other name against such a control plane is a real "no", because
// offering a path this cannot know would be offering an rsync that fails.
func (t *SSHTarget) WritablePathFor(name string) (string, bool) {
	for i := range t.WritablePaths {
		if t.WritablePaths[i].Name == name {
			return t.WritablePaths[i].Path, true
		}
	}

	if len(t.WritablePaths) == 0 && name == "public" {
		return t.FilesPath, true
	}

	return "", false
}

// Server is one machine.
type Server struct {
	ID       int    `json:"id"`
	Label    string `json:"label"`
	State    string `json:"state"`
	Provider string `json:"provider"`
	Region   string `json:"region"`
	Size     string `json:"size"`
	IPv4     string `json:"ipv4"`
	Shared   bool   `json:"shared"`
}

// Task is one piece of work the platform did or is doing.
type Task struct {
	ID       int    `json:"id"`
	Type     string `json:"type"`
	State    string `json:"state"`
	Attempts int    `json:"attempts"`
	Message  string `json:"message"`
	Finished *int64 `json:"finished"`
	Output   any    `json:"output"`
}

// Terminal reports whether a task has stopped moving.
//
// The states are the control plane's (vc_task's TaskState). Compared as
// strings because the CLI has no business holding a copy of the enum: a state
// added on that side should leave this returning false — "not finished yet" —
// rather than reporting a task complete because the name was unfamiliar.
func (t *Task) Terminal() bool {
	switch t.State {
	case "succeeded", "failed", "cancelled":
		return true
	default:
		return false
	}
}

// Succeeded reports whether a finished task finished well.
func (t *Task) Succeeded() bool {
	return t.State == "succeeded"
}

// TaskLog is a window onto a task's output.
//
// Followed by polling with an offset rather than streamed: a long poll would
// hold a PHP-FPM worker on the control plane for its whole duration, and a
// deploy watched by six developers would be six workers doing nothing.
type TaskLog struct {
	// Offset is where this chunk starts, which is the requested offset
	// clamped to the log's current length. A smaller value than was asked
	// for means the log shrank — retention removed it — and a follower
	// should start again rather than wait for bytes that will never come.
	Offset int `json:"offset"`

	Content string `json:"content"`

	// NextOffset is where to ask from next.
	NextOffset int `json:"next_offset"`

	// Size is how long the log is now.
	Size int `json:"size"`

	// Truncated means more is already written: ask again at once rather than
	// waiting out the poll interval.
	Truncated bool `json:"truncated"`

	// State is the task's state, so a follower needs one request per tick
	// rather than two.
	State string `json:"state"`

	// Complete means the work has stopped moving and this chunk reached the
	// end. Both halves matter: a finished task with unread output is not
	// done being read, and a drained log on a running task is not done being
	// written.
	Complete bool `json:"complete"`
}

// Release is one build of a project.
type Release struct {
	ID int `json:"id"`

	// Number is the project's own sequence, claimed under a lock. It is what
	// a person types into `--release` and what the `#` column prints; ids are
	// global and would interleave wrongly after a backfill. The deploy route
	// takes the id, so releaseIDFor turns one into the other.
	Number int `json:"number"`

	Status string `json:"status"`

	// Deployable is the control plane's own predicate, not a second reading
	// of Status — so it means here what it means when a deploy is refused.
	Deployable bool `json:"deployable"`

	GitSHA     string `json:"git_sha"`
	GitRef     string `json:"git_ref"`
	GitMessage string `json:"git_message"`

	Size    *int64 `json:"size"`
	BuiltAt *int64 `json:"built_at"`
	Created int64  `json:"created"`

	// DeployedTo is the environments this build is live on right now, and is
	// empty rather than absent when it is live nowhere.
	DeployedTo []string `json:"deployed_to"`
}

// Deployment is a queued deployment.
type Deployment struct {
	ID    int    `json:"id"`
	State string `json:"state"`

	// Trigger says what asked for it — manual, api, rollback, redeploy — so a
	// client can report "rolled back" rather than "deployed" for the same
	// shape of answer.
	Trigger string `json:"trigger"`

	Environment string `json:"environment"`

	Release       *int `json:"release"`
	ReleaseNumber *int `json:"release_number"`

	// PreviousRelease is what this one replaces, so "rolled back to 46 from
	// 47" needs no second request.
	PreviousRelease *int `json:"previous_release"`
}

// Describe says what happened, in the words the trigger calls for.
//
// A rollback and a deploy are the same shape on the wire and different things
// to the person who asked, so reporting both as "deployed" would be reporting
// one of them wrongly.
func (d *Deployment) Describe() string {
	what := "release"
	if d.ReleaseNumber != nil {
		what = fmt.Sprintf("release %d", *d.ReleaseNumber)
	}

	switch d.Trigger {
	case "rollback":
		if d.PreviousRelease != nil {
			return fmt.Sprintf("Rolling %s back to %s", d.Environment, what)
		}

		return fmt.Sprintf("Rolling %s back", d.Environment)
	case "redeploy":
		return fmt.Sprintf("Deploying %s to %s again", what, d.Environment)
	default:
		return fmt.Sprintf("Queued %s to %s", what, d.Environment)
	}
}

// Variable is one environment variable, as it is in effect somewhere.
type Variable struct {
	Name string `json:"name"`

	// Value is nil for a secret, and that is the whole of the contract:
	// written is not the same as readable, and the control plane does not
	// hand a secret back to anybody. A `*string` rather than a `string`
	// because nil and "" are different answers — "withheld" and "set to the
	// empty string" — and a client that collapsed them would report a
	// configured secret as unset.
	Value *string `json:"value"`

	Secret bool `json:"secret"`

	// Description is nil where the route does not carry one — the
	// environment's resolved rows do not. A pointer rather than a string
	// because Go's zero value would render that absence as `""` in
	// `--format json`, which reads as "there is no description" rather than
	// "this answer does not say".
	Description *string `json:"description"`

	// Scope is which definition won: "environment" or "project".
	Scope string `json:"scope"`

	// InheritedFrom is the broader scope this would have come from
	// otherwise, so "overridden here" is visible rather than inferred.
	InheritedFrom *string `json:"inherited_from"`
	Overridden    bool    `json:"overridden"`

	// Definition is the record that supplied the value, so a client can
	// offer to edit the one actually in effect.
	Definition int `json:"definition"`
}

// Display renders a value for a table cell.
//
// Truncated, because a variable's value is arbitrary length and one long one
// makes every other row unreadable. The whole value is in `--format json`,
// which is where anything that needs it should be looking anyway.
func (v *Variable) Display() string {
	if v.Secret || v.Value == nil {
		return "(secret)"
	}

	value := *v.Value

	if value == "" {
		// Distinguished from a secret's "(secret)" and from an absent row.
		// An empty cell would read as "nothing here".
		return `""`
	}

	// Newlines flattened before the width check: a certificate pasted into a
	// variable would otherwise spill across the table and take the header
	// with it.
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")

	const width = 48
	if len(value) > width {
		return value[:width-1] + "…"
	}

	return value
}

// ScopeLabel says which definition won, and whether it hid another.
func (v *Variable) ScopeLabel() string {
	if v.Overridden && v.InheritedFrom != nil {
		return fmt.Sprintf("%s (overrides %s)", v.Scope, *v.InheritedFrom)
	}

	return v.Scope
}

// VariableWrite is what `var set` sends.
type VariableWrite struct {
	Name string `json:"name"`

	// Value is always sent, empty string included — the control plane tells
	// an omitted key from an empty one and refuses the first.
	Value string `json:"value"`

	// Secret is a pointer so it can be omitted. Omitting it is what lets an
	// existing secret stay secret: sending `false` would turn a password
	// into a readable variable, and somebody updating one should not have to
	// remember to say `--secret` a second time to avoid revealing it.
	Secret *bool `json:"secret,omitempty"`

	Description string `json:"description,omitempty"`
}

// VariableWritten is the answer to a write.
type VariableWritten struct {
	Variable Variable `json:"variable"`

	// TakesEffect is when the value reaches a container, in the control
	// plane's own words — "next deploy". Repeated to the person rather than
	// composed here, so a platform that one day pushes a variable without a
	// deploy says so without a new CLI being shipped to say it.
	TakesEffect string `json:"takes_effect"`
}

// VariableRemoved is the answer to a delete.
type VariableRemoved struct {
	Deleted     string `json:"deleted"`
	TakesEffect string `json:"takes_effect"`
}

// TaskRow is one task as the activity list returns it.
//
// Separate from Task rather than folded into it, because the two routes
// answer deliberately different things. A row carries max_attempts, the
// timestamps and the environment and server it ran against, and carries
// neither payload nor output: a payload is the agent protocol's wire format
// and an output is a map whose size is set by what a build printed, so a
// hundred of them is a response nobody sized. `activity get` fetches those
// one at a time, by id.
type TaskRow struct {
	ID          int    `json:"id"`
	Type        string `json:"type"`
	State       string `json:"state"`
	Attempts    int    `json:"attempts"`
	MaxAttempts int    `json:"max_attempts"`
	Message     string `json:"message"`

	// ExitCode is nil while the work has not run or did not report one, which
	// is different from zero.
	ExitCode *int `json:"exit_code"`

	Created  int64  `json:"created"`
	Started  *int64 `json:"started"`
	Finished *int64 `json:"finished"`

	// Environment and Server are nil for platform work that ran against
	// neither. Nil rather than an id of 0, because a script cannot tell "no
	// environment" from "environment 0" by comparison.
	Environment *struct {
		ID   int    `json:"id"`
		Slug string `json:"slug"`
	} `json:"environment"`

	Server *struct {
		ID    int    `json:"id"`
		Label string `json:"label"`
	} `json:"server"`
}

// Terminal reports whether a task has stopped moving.
//
// The same string comparison Task.Terminal() makes, and for the same reason:
// a state added on the control plane should read as "not finished yet" rather
// than be reported complete because the name was unfamiliar.
func (t *TaskRow) Terminal() bool {
	switch t.State {
	case "succeeded", "failed", "cancelled":
		return true
	default:
		return false
	}
}

// TaskPage is a page of the activity list.
type TaskPage struct {
	Tasks []TaskRow `json:"tasks"`

	// NextBefore is the cursor for the next page, or nil at the end.
	//
	// A cursor rather than an offset, and the reason is not style: the list
	// is sorted by id descending on a table that grows at the head, so every
	// deploy queued between one page and the next shifts every later row by
	// one. With an offset a client re-reads a task it has seen and skips one
	// it has not, and the skipped one is the failure it was looking for.
	NextBefore *int `json:"next_before"`
}

// Stack is what an environment runs.
//
// Answerable before an environment has been built, which is its most useful
// property: the control plane resolves it from the environment's stored
// service list, its runtime, the project's type and the manifest recorded at
// the last deploy. No machine is consulted, so a draft environment answers
// with the same list it will answer with once it is running.
type Stack struct {
	Environment struct {
		ID   int    `json:"id"`
		Slug string `json:"slug"`
	} `json:"environment"`

	Runtime string `json:"runtime"`

	// Application is the service an application's own commands run in, and
	// Entrypoint the one traffic arrives at. Two different services, and both
	// are sent: a shell wants the first, anything reasoning about the edge
	// wants the second. Either is nil where the stack has none.
	Application *string `json:"application"`
	Entrypoint  *string `json:"entrypoint"`

	Services []Service `json:"services"`

	// Added is what was pulled in as a dependency rather than asked for,
	// which is the answer to "why is ZooKeeper in a list I never wrote it
	// in": Solr needs it, and Solr without it starts and then fails in a way
	// that looks like a Solr problem.
	Added []string `json:"added"`

	// Rejected is names the environment still stores that the catalogue no
	// longer offers. Empty on a healthy environment, and a real answer when
	// it is not.
	Rejected []string `json:"rejected"`
}

// Service is one container in a stack.
type Service struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Group       string `json:"group"`

	// Tier is the machine role this runs on, or nil for a build-only service,
	// which runs nowhere and therefore needs no machine.
	Tier *string `json:"tier"`

	// Image, Tag and Version answer three different questions and all three
	// are sent. Version is the load-bearing one: it is what a customer writes
	// in vallic.yaml, and the rest of the tag is the platform's own image
	// build, which is rebuilt for security fixes and must not be pinned.
	Image   string `json:"image"`
	Tag     string `json:"tag"`
	Version string `json:"version"`

	// Source is where the service came from: "manifest" if the repository
	// asked for it, "included" if every stack has it, "plan" otherwise.
	Source string `json:"source"`

	BuildOnly bool `json:"build_only"`
}

// DomainList is every hostname claimed for an environment, verified or not.
type DomainList struct {
	Environment int `json:"environment"`

	// PlatformHostname is the address issued with the environment, and Target
	// is what a new DNS record should point at. Not always the same: a CDN in
	// front of the environment answers on its own name, and the platform
	// hostname is what that name resolves to.
	PlatformHostname string `json:"platform_hostname"`
	Target           string `json:"target"`

	// AllowsCustomDomains is false for an environment that cannot hold one at
	// all, which a client reads to decide whether to offer `domain add`
	// rather than letting somebody find out from a refusal.
	AllowsCustomDomains bool `json:"allows_custom_domains"`

	Domains []Domain `json:"domains"`
}

// Domain is one hostname claimed for an environment.
type Domain struct {
	ID          int    `json:"id"`
	Environment int    `json:"environment"`
	Hostname    string `json:"hostname"`

	// Kind is "platform" or "custom". A platform domain is issued with the
	// environment and cannot be removed on its own.
	Kind string `json:"kind"`

	IsPrimary bool `json:"is_primary"`
	IsApex    bool `json:"is_apex"`

	// VerificationState is three values, not two: "unverified" means never
	// checked, "failed" means checked and the record was wrong. A client with
	// only the boolean could not tell "waiting for DNS" from "your record is
	// wrong", which are different things to do next.
	VerificationState string `json:"verification_state"`

	// Verified is redundant with the state on purpose, so a script branching
	// on whether traffic is routed does not have to know the enum.
	Verified bool `json:"verified"`

	// VerificationMessage is English, for printing. Never branch on it.
	VerificationMessage string `json:"verification_message"`

	VerificationToken *string `json:"verification_token"`

	// LastChecked is nil rather than zero when nothing has ever checked,
	// because zero as a timestamp is 1970 and a client would render a date.
	LastChecked *int64 `json:"last_checked"`

	TLSMode    string `json:"tls_mode"`
	CDN        string `json:"cdn"`
	DNSManaged bool   `json:"dns_managed"`

	// DNSInstructions is what to publish, worked out by the control plane.
	// Empty for a platform domain, and the key is present either way.
	DNSInstructions []DNSInstruction `json:"dns_instructions"`
}

// DNSInstruction is one record a customer has to publish.
type DNSInstruction struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`

	// Note explains why the record is what it is, in the control plane's own
	// words, which is the half a client cannot write for itself.
	Note *string `json:"note"`

	// Optional marks a record that proves something the other one proves
	// anyway. Printed dimmer rather than dropped: somebody who wants to be
	// verified before moving traffic needs it.
	Optional bool `json:"optional"`
}

// Verification is the answer to a verification check.
type Verification struct {
	Verified bool `json:"verified"`

	// Message is English, for printing. Never branch on it.
	Message string `json:"message"`

	// PointsHere is whether traffic already reaches this environment, which
	// is separate from Verified on purpose. Somebody who published the TXT
	// but not the CNAME is half done, and somebody who published the CNAME is
	// verified by that alone.
	PointsHere bool `json:"points_here"`
}

// VerificationResult is the check and the domain as it now stands.
type VerificationResult struct {
	Verification Verification `json:"verification"`

	// Domain is rebuilt after the check, so its state and its instructions
	// are the new ones. The TXT record disappears from the instructions once
	// the hostname is verified.
	Domain Domain `json:"domain"`
}

// DomainRemoved is what a delete removed.
//
// A body rather than an empty 200, so a client prints what the control plane
// actually removed instead of echoing back the argument it was given.
type DomainRemoved struct {
	Deleted struct {
		ID          int    `json:"id"`
		Hostname    string `json:"hostname"`
		Kind        string `json:"kind"`
		Environment int    `json:"environment"`
	} `json:"deleted"`
}

// EnvironmentCreate is what `env create` sends.
//
// Four fields, and deliberately no more. The name is derived from the type and
// the branch, the slug is immutable and is five systems at once, `protected` is
// the lever that guards who may reshape the environment, and the machine-owned
// fields are the control plane's. A request that tries to set any of them is
// refused rather than ignored.
type EnvironmentCreate struct {
	GitRef string `json:"git_ref"`

	// Type is "staging" or "development". Never "production": a project has
	// one production environment and it is created with the project.
	Type string `json:"type"`

	AutoDeploy bool `json:"auto_deploy"`
}

// EnvironmentPatch changes what an environment builds from.
//
// Both fields are pointers so either can be left alone. An empty patch is
// refused by the control plane rather than answered 200, because a request
// that changes nothing is a client bug and not a no-op worth rewarding.
type EnvironmentPatch struct {
	GitRef     *string `json:"git_ref,omitempty"`
	AutoDeploy *bool   `json:"auto_deploy,omitempty"`
}

// EnvironmentDeleted is what a delete removed.
type EnvironmentDeleted struct {
	Environment struct {
		ID      int    `json:"id"`
		Slug    string `json:"slug"`
		Deleted bool   `json:"deleted"`
	} `json:"environment"`
}

// BackupListing is what an environment has been backed up to, and what exists.
type BackupListing struct {
	Environment string `json:"environment"`
	BackedUp    bool   `json:"backed_up"`

	// PlatformFallback is whether the platform has somewhere to write when a
	// team has configured nowhere of its own.
	PlatformFallback bool `json:"platform_fallback"`

	// Repository is nil for an environment that has never been backed up,
	// which is a legitimate state and not an error. It carries the kind of
	// destination and never its location or its key: the location is the
	// platform's business, and the key decrypts every snapshot in the
	// repository forever and cannot be rotated without rewriting it.
	Repository *BackupRepository `json:"repository"`

	Retention map[string]int `json:"retention"`

	// ListedAt is when the listing was taken, or nil when nothing has ever
	// listed. Listings are refreshed behind every backup and every removal
	// rather than on request, so a stale one means no backup has run since.
	ListedAt *int64 `json:"listed_at"`

	Snapshots []Snapshot `json:"snapshots"`

	// InFlight is the task currently taking each kind, keyed "database" and
	// "files", with nil where nothing is running.
	InFlight map[string]*int `json:"in_flight"`
}

// BackupRepository is where an environment's snapshots live.
type BackupRepository struct {
	ID          int  `json:"id"`
	Initialised bool `json:"initialised"`

	// DestinationKind is s3, sftp, rclone or local, or nil where the
	// destination row has been removed.
	DestinationKind *string `json:"destination_kind"`

	// OwnDestination is whether the team supplied the storage, which decides
	// whether anybody may ever be shown the repository key.
	OwnDestination bool `json:"own_destination"`

	LastDatabase *int64 `json:"last_database"`
	LastFiles    *int64 `json:"last_files"`
	LastCheck    *int64 `json:"last_check"`
	CheckFailed  bool   `json:"check_failed"`
}

// Snapshot is one backup.
//
// Not an entity anywhere: a snapshot is an opaque id that exists only in the
// output of the last successful listing for one environment. That is why every
// backup route is addressed through its environment.
type Snapshot struct {
	ID      string `json:"id"`
	ShortID string `json:"short_id"`

	// TakenAt is the timestamp as the backup tool reported it, and Taken the
	// same moment as an epoch, or nil where it could not be parsed. Both,
	// because the first is what the tool said and the second is what sorts.
	TakenAt string `json:"taken_at"`
	Taken   *int64 `json:"taken"`

	// Kind is "database", "files", or "unknown". Unknown is deliberate and
	// travels as such: it means the platform did not take this snapshot, so it
	// cannot say what is in it, and guessing would risk unpacking a database
	// dump over a site's files.
	Kind string `json:"kind"`

	// Manual is whether somebody asked for it. It changes how long it is kept.
	Manual bool `json:"manual"`

	// ExportTask is a finished export of this snapshot, if there is one.
	ExportTask *int `json:"export_task"`
}

// BackupQueued is the answer to asking for a backup now.
type BackupQueued struct {
	Backup struct {
		Environment string `json:"environment"`
		Kind        string `json:"kind"`
		Manual      bool   `json:"manual"`

		Task struct {
			ID    int    `json:"id"`
			Type  string `json:"type"`
			State string `json:"state"`
		} `json:"task"`
	} `json:"backup"`
}

// BackupDownload is the answer to asking for a copy.
//
// The same object whether the copy is ready or is being made, with nulls
// rather than absent keys. A download cannot be a link into the repository:
// the repository is encrypted with a key the customer never holds and sits in
// storage they have no credentials for, so a machine has to read the snapshot
// out and write it somewhere fetchable first. That is the work the first call
// queues.
type BackupDownload struct {
	Download struct {
		Environment string `json:"environment"`
		Snapshot    string `json:"snapshot"`
		Kind        string `json:"kind"`

		Task struct {
			ID    int    `json:"id"`
			Type  string `json:"type"`
			State string `json:"state"`
		} `json:"task"`

		// URL is nil until the export has finished. Signed on the ask and
		// never stored, because a link kept on a task row would be readable
		// by everyone who can see the row and would keep working after their
		// access was taken away.
		URL *string `json:"url"`

		// Expires is when the link stops working, as an epoch.
		Expires *int64 `json:"expires"`

		// Filename is known before the export runs, so it is present on a
		// queued answer too.
		Filename string `json:"filename"`
	} `json:"download"`
}

// Ready reports whether a download can be fetched now.
func (d *BackupDownload) Ready() bool {
	return d.Download.URL != nil && *d.Download.URL != ""
}

// RestoreQueued is the answer to asking for a restore.
type RestoreQueued struct {
	Restore struct {
		Environment string `json:"environment"`

		// Source is always the same as Environment today, and is sent anyway
		// so a client that later meets a cross-environment restore does not
		// have to start testing for a new key.
		Source string `json:"source"`

		Kind     string `json:"kind"`
		Snapshot string `json:"snapshot"`

		// SanitisationCommands is how many commands the application declared
		// to scrub the data on its way in. A count, never a boolean: zero
		// means nothing is scrubbed, which is the case worth warning about.
		//
		// Not a pointer. The control plane counts an array it has just written
		// to the task payload, so there is no "cannot say" state to model —
		// and a pointer would turn a renamed key into a quiet "unknown" where
		// a zero is a loud warning that real data is about to land unscrubbed.
		// For this field, over-warning is the safe way to be wrong.
		SanitisationCommands int `json:"sanitisation_commands"`

		Task struct {
			ID    int    `json:"id"`
			Type  string `json:"type"`
			State string `json:"state"`
		} `json:"task"`
	} `json:"restore"`
}

// Validation is what the control plane made of a manifest.
//
// One shape from both validate routes: the cheap one carries neither Clamps
// nor Adoptable and a null CheckedAgainst, and both decode into this with the
// same code, because an absent key and an empty one are the same zero value.
type Validation struct {
	// Filename and SchemaVersion are sent rather than assumed, so a client
	// that writes the file does not carry its own copy of either.
	Filename      string `json:"filename"`
	SchemaVersion int    `json:"schema_version"`

	// Valid means different things on the two routes, which is exactly why
	// CheckedAgainst exists. Without an environment it means only that the
	// file parses and every key is understood.
	Valid bool `json:"valid"`

	// CheckedAgainst is nil when nothing was compared against an environment.
	// This is the field that keeps the two answers honest: a true with a nil
	// here is not a promise that a deploy succeeds.
	CheckedAgainst *struct {
		ID   int    `json:"id"`
		Slug string `json:"slug"`
	} `json:"checked_against"`

	// Problems is every problem at once, in the control plane's own English.
	// Collected rather than thrown one at a time, because somebody fixing a
	// file wants the whole list.
	Problems []string `json:"problems"`

	// Clamps is each value the platform will not honour as written. Absent
	// on the cheap route, which cannot know: an absent key and an empty list
	// are the same nil slice here, so nothing has to tell them apart.
	Clamps []Clamp `json:"clamps"`

	// Adoptable is what a deploy would switch on for you and what it would
	// refuse. Absent where the control plane cannot answer it without
	// mutating the environment it was asked about.
	Adoptable *struct {
		Added   []string `json:"added"`
		Refused []string `json:"refused"`
	} `json:"adoptable"`

	// Manifest is what the parser understood, echoed back. This is what makes
	// the route useful beyond a yes or no: it shows which keys landed, so
	// somebody whose service was ignored can see that it was ignored rather
	// than wonder.
	Manifest Manifest `json:"manifest"`
}

// Manifest is a vallic.yaml as the control plane read it.
//
// Typed rather than left as a map, because every field here is one a person
// asked for and wants to see confirmed, and a map renders as whatever order
// Go felt like.
type Manifest struct {
	Type string `json:"type"`

	// Runtime is the language versions, keyed by language.
	Runtime StringMap `json:"runtime"`

	Start string `json:"start"`
	Port  int    `json:"port"`

	Services []ManifestService `json:"services"`
	Cron     []CronJob         `json:"cron"`
	Workers  []Worker          `json:"workers"`

	BuildSteps  []BuildStep `json:"build_steps"`
	BuildCache  []string    `json:"build_cache"`
	DeploySteps []string    `json:"deploy_steps"`

	RollbackOnFailure bool   `json:"rollback_on_failure"`
	HealthPath        string `json:"health_path"`
	HealthTimeout     int    `json:"health_timeout"`

	// RequiredEnv is the variables the application says it cannot run without.
	RequiredEnv []string `json:"required_env"`

	Mounts []string `json:"mounts"`

	// Extras are machines of the customer's own, running containers this
	// platform did not write. Declared in the manifest, defined by a compose
	// fragment the control plane does not echo and this does not ask for.
	Extras []ManifestExtra `json:"extras"`
}

// ManifestService is one service a repository asked to run beside it.
type ManifestService struct {
	Name    string `json:"name"`
	Version string `json:"version"`

	// Environment is what the manifest sets on that service, where the
	// catalogue allows it to be overridden.
	Environment StringMap `json:"environment"`
}

// ManifestExtra is one extra slot the repository declared.
//
// The slot names are fixed -- pioneer, voyager, galileo and so on -- because
// one word has to be the machine, the hostname on the private network, the
// directory in the repository and the line on the invoice.
type ManifestExtra struct {
	Name string `json:"name"`

	// Expose is what the application can reach, keyed by the compose service's
	// own name inside the fragment.
	Expose ExposeMap `json:"expose"`

	// InternetEgress is whether its containers may open outbound connections.
	// Off unless the manifest asks: an extra exists to be called.
	InternetEgress bool `json:"internet_egress"`
}

// ExposeMap is the ports one extra answers on, keyed by compose service.
//
// The same empty-list tolerance StringMap documents, and needed for the same
// reason: an extra that exposes nothing is a *reported problem* rather than a
// rejected document, so the manifest is still echoed with `expose` as PHP's
// empty array. Without this, a manifest with that mistake in it would fail to
// parse here and the reader would be told nothing at all -- losing the very
// problem that explains what they got wrong.
type ExposeMap map[string][]ExtraPort

// UnmarshalJSON reads an object, or PHP's empty list.
func (m *ExposeMap) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)

	if bytes.Equal(trimmed, []byte("[]")) || bytes.Equal(trimmed, []byte("null")) {
		*m = ExposeMap{}

		return nil
	}

	// Not ExposeMap, or this recurses.
	var plain map[string][]ExtraPort

	if err := json.Unmarshal(data, &plain); err != nil {
		return err
	}

	*m = plain

	return nil
}

// ExtraPort is one port pair on an extra.
//
// Two numbers because several of the customer's services share one machine and
// therefore one address: Listen is the port on the private network, Container
// the port that service listens on inside its own container.
type ExtraPort struct {
	Listen    int `json:"listen"`
	Container int `json:"container"`
}

// StringMap is a JSON object that the control plane may send as an empty list.
//
// PHP has one array type for both lists and maps, and `json_encode` renders an
// empty one as `[]`. So a field that is an object whenever it holds anything
// arrives as `[]` when it holds nothing, and unmarshalling that into a Go map
// fails outright — taking the whole response with it, not just the field.
//
// That is not hypothetical: typing this struct at all broke `vallic validate`
// for every manifest that declared a service, because `services[].environment`
// is empty far more often than not.
//
// Tolerated here rather than worked around at each call site, and only for the
// exact shape PHP produces: a list with anything in it is a real disagreement
// about the contract and still fails loudly.
type StringMap map[string]string

// UnmarshalJSON reads an object, or PHP's empty list.
func (m *StringMap) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)

	if bytes.Equal(trimmed, []byte("[]")) || bytes.Equal(trimmed, []byte("null")) {
		*m = StringMap{}

		return nil
	}

	// Not StringMap, or this recurses.
	var plain map[string]string

	if err := json.Unmarshal(data, &plain); err != nil {
		return err
	}

	*m = plain

	return nil
}

// Build is a build the control plane started.
//
// Answered by a 202: a build is minutes of somebody else's machine, so what
// comes back is what was started rather than what it produced.
type Build struct {
	// Release is the entity id, which is what the platform's own logs name.
	Release int `json:"release"`

	// ReleaseNumber is the project's own sequence -- what `--release` takes and
	// what to print. Nil against a control plane that could not read it back.
	ReleaseNumber *int `json:"release_number"`

	Environment string `json:"environment"`
	GitRef      string `json:"git_ref"`

	// WillDeploy is what happens when the build finishes, not what was asked
	// for: an environment that watches this branch deploys the result whether
	// or not anybody asked, because that is what auto-deploy means.
	WillDeploy bool `json:"will_deploy"`

	// DeployRequested is whether this request asked for it, so the difference
	// between "because you said so" and "because this environment always does"
	// can be said out loud.
	DeployRequested bool `json:"deploy_requested"`
}

// CronJob is one scheduled command.
type CronJob struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Command  string `json:"command"`

	// Default marks a schedule the platform put in because the file declared
	// none. Worth showing: a client that could not tell would offer to edit a
	// line that is not in the repository.
	Default bool `json:"default"`
}

// Worker is one long-running process beside the application.
type Worker struct {
	Name     string `json:"name"`
	Command  string `json:"command"`
	Replicas int    `json:"replicas"`
}

// BuildStep is one command run while the release is built.
type BuildStep struct {
	Name    string `json:"name"`
	Command string `json:"command"`

	// Image is the container it runs in, empty for the application's own.
	Image string `json:"image"`
}

// Clamp is one value the platform will not honour as asked.
type Clamp struct {
	// Field is the dotted path in the file, so it can be found and changed.
	Field   string `json:"field"`
	Asked   string `json:"asked"`
	Applied string `json:"applied"`
}

// CLIRelease is the build of this binary the control plane tells clients to
// run, and where to get it.
//
// The agent's release manifest, reused rather than reinvented: one URL and one
// SHA-256 per target, and a version advertised with nothing behind it treated
// as unpublished. What differs is the key. The agent runs on Linux only and
// keys its downloads by architecture; this is built for macOS as well, so
// "amd64" alone would offer a darwin build to a linux machine, whose checksum
// would match and which would not run.
// The release channels, spelled as the control plane spells them (CliRelease).
//
// Constants rather than literals at each comparison, because the cost of a
// misspelling is silence: a client comparing against "development" would never
// match, would never say it was on a dev build, and would be right about
// everything else.
const (
	ChannelStable = "stable"
	ChannelDev    = "dev"
)

type CLIRelease struct {
	// Channel is the build this answer is for: "stable" or "dev". Sent by the
	// control plane rather than echoed from the request, because the two can
	// differ -- an installation that serves dev answers dev to a client that
	// asked for nothing -- and what a client must print is the channel it
	// actually got.
	//
	// Empty against a control plane from before channels existed, which is a
	// stable answer and is read as one: an older console has one channel and
	// it is this one.
	Channel string `json:"channel"`

	// Version is empty when nothing is published. The control plane answers
	// that as null rather than 404, so a client can tell "there is no release"
	// from "there is no such route", and only the first is an ordinary answer.
	Version string `json:"version"`

	// Downloads is keyed by Go's platform string, "<goos>-<goarch>": the same
	// spelling the build writes into the file names. Every platform rather
	// than the one that asked, because the request does not say what it is
	// running on and the client knows exactly.
	Downloads CLIDownloads `json:"downloads"`
}

// CLIDownloads is what a release publishes, keyed by platform.
//
// Its own type for the reason StringMap has one, and worth restating because
// the case it covers is the important one here. PHP has a single array type
// for lists and maps, so json_encode renders an empty one as `[]`, which fails
// to unmarshal into a Go map and takes the whole response with it. These are
// empty in exactly the state a client most needs to read: a control plane that
// has published nothing, or whose entries were all dropped for missing a
// checksum. Refusing to parse it would tell the person their control plane
// sent something unreadable, when what it sent was "nothing is published".
type CLIDownloads map[string]CLIDownload

// UnmarshalJSON reads an object, or PHP's empty list.
func (d *CLIDownloads) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)

	if bytes.Equal(trimmed, []byte("[]")) || bytes.Equal(trimmed, []byte("null")) {
		*d = CLIDownloads{}

		return nil
	}

	// Not CLIDownloads, or this recurses.
	var plain map[string]CLIDownload

	if err := json.Unmarshal(data, &plain); err != nil {
		return err
	}

	*d = plain

	return nil
}

// CLIDownload is where one platform's binary is, and what it has to hash to.
type CLIDownload struct {
	URL string `json:"url"`

	// SHA256 is the control plane's own copy of the digest, and is the only
	// one worth checking. A checksum fetched from whoever served the bytes
	// proves the download was not corrupted on the way and says nothing about
	// a release that was replaced at the source.
	SHA256 string `json:"sha256"`
}

// Download is the entry for one platform key, if there is a usable one.
//
// Absent rather than empty when either half is missing. The control plane
// drops a half-configured entry before it sends it, and this drops it again on
// arrival, because the failure the rule prevents is the same at both ends: a
// URL with no checksum is a binary nobody verified, and installing one is how
// a client runs whatever answered that address.
func (r CLIRelease) Download(platform string) (CLIDownload, bool) {
	download, ok := r.Downloads[platform]

	if !ok || download.URL == "" || download.SHA256 == "" {
		return CLIDownload{}, false
	}

	return download, true
}

// Platforms is every platform this release can actually be installed for.
//
// Sorted, and filtered by the same rule as Download: it is printed to somebody
// whose own platform was not published, and a list that named a platform they
// would then be refused for would be worse than saying nothing.
func (r CLIRelease) Platforms() []string {
	platforms := make([]string, 0, len(r.Downloads))

	for platform := range r.Downloads {
		if _, ok := r.Download(platform); ok {
			platforms = append(platforms, platform)
		}
	}

	sort.Strings(platforms)

	return platforms
}

// Logs is where an environment's logs go. Not the logs themselves: a site's
// request and error logs go from its machine to the team's own destination
// and never to the control plane.
type Logs struct {
	Environment string `json:"environment"`

	// Destination is where the team reads them, or nil where none is chosen
	// or the one chosen cannot send.
	Destination *LogDestination `json:"destination"`

	// Unusable is a destination somebody chose that is switched off or
	// missing what it needs, so nothing reaches it.
	Unusable *LogDestination `json:"unusable"`

	// MachineDays is how long each machine keeps its own copy.
	MachineDays int `json:"machine_days"`

	// SettingsURL is the console page where the destination is chosen, or
	// nil where the control plane has no console.
	SettingsURL *string `json:"settings_url"`
}

// LogDestination names a destination by kind and label, and nothing that
// reaches it: its endpoint and token are the integration's.
type LogDestination struct {
	Kind      string `json:"kind"`
	KindLabel string `json:"kind_label"`
	Name      string `json:"name"`
}
