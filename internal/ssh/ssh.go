// Package ssh builds the commands that reach inside an environment.
//
// It adds no capability. Everything here stands on the forced command the
// agent already installs behind a tenant's key: one account per team, one
// `command=` that hands what the client asked for to a shell *inside* the
// application container, and sshd refusing every kind of forwarding. What
// this package does is derive the connection so nobody has to retype it, and
// get the quoting right.
//
// It shells out to ssh and rsync rather than speaking either protocol.
// That inherits ~/.ssh/config, the agent, known_hosts and every key the
// person has already set up — and inherits the "Too many authentication
// failures" problem, which is why IdentityFile is passed with
// IdentitiesOnly when the configuration knows which key to offer.
package ssh

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/vallic/vallic-cli/internal/api"
)

// Target is a resolved connection, as the control plane described it.
type Target struct {
	api.SSHTarget

	// Identity is a private key to offer, or empty for whatever ssh would
	// have chosen. From --identity or VALLIC_SSH_KEY.
	Identity string

	// Machine is the machine to be carried on to from Host, by name; empty
	// for JumpTo, or for Host itself where there is none.
	Machine string

	// Container is a container to open other than the application's: a
	// worker, by name.
	Container string
}

// jumpTarget is the machine a connection is carried on to, or empty.
func (t *Target) jumpTarget() string {
	if t.Machine != "" {
		return t.Machine
	}

	return t.JumpTo
}

// ChooseMachine sets the machine to log in to, if the environment has one by
// that name.
func (t *Target) ChooseMachine(name string) error {
	if name == "" {
		return nil
	}
	names := make([]string, 0, len(t.Machines))
	for _, m := range t.Machines {
		if m.Name == name {
			t.Machine = name
			return nil
		}
		names = append(names, m.Name)
	}
	if len(names) == 0 {
		return fmt.Errorf("this environment runs on one machine; there is no %q to choose", name)
	}

	return fmt.Errorf("no machine called %q; this environment has: %s", name, strings.Join(names, ", "))
}

// ErrNoShell is what a nil SSH target means.
//
// The control plane sends nil where the person's role does not admit a shell
// or nothing runs the environment yet, and both want a sentence rather than
// a nil dereference.
type ErrNoShell struct {
	Environment string
}

func (e *ErrNoShell) Error() string {
	return fmt.Sprintf(
		"no shell access to %s\n"+
			"  either it is not built yet, or your role in this team does not include a shell (developer and above)",
		e.Environment,
	)
}

// New builds a Target from an environment's detail response.
func New(detail *api.EnvironmentDetail, identity string) (*Target, error) {
	if detail.SSH == nil {
		return nil, &ErrNoShell{Environment: detail.Slug}
	}

	if identity == "" {
		identity = os.Getenv("VALLIC_SSH_KEY")
	}

	return &Target{SSHTarget: *detail.SSH, Identity: identity}, nil
}

// Destination is the `user@host` ssh takes.
//
// Carried on to another machine, the host is that machine's label under the
// environment's — `web-2.production.acme.vallic.cloud` — which nothing
// resolves: ProxyCommand makes the connection, and the label is what keeps
// one known_hosts entry per machine.
func (t *Target) Destination() string {
	if machine := t.jumpTarget(); machine != "" {
		return t.User + "@" + machine + "." + t.Host
	}

	return t.User + "@" + t.Host
}

// options are the -o and -p arguments every invocation shares.
func (t *Target) options() []string {
	args := t.base()

	// Through the environment's front, which carries the connection on to
	// the machine over the private network: the machines behind it often
	// have no public address, and the front may run no site of its own.
	if machine := t.jumpTarget(); machine != "" {
		hop := append([]string{"ssh"}, t.base()...)
		hop = append(hop, t.User+"@"+t.Host)
		if t.Name != "" {
			hop = append(hop, t.Name)
		}
		hop = append(hop, "jump", machine)
		args = append(args, "-o", "ProxyCommand="+strings.Join(hop, " "))
	}

	return args
}

// base is the port and the identity, which the hop to the front needs as
// much as the login itself.
func (t *Target) base() []string {
	args := []string{"-p", fmt.Sprint(t.Port)}

	if t.Identity != "" {
		// Both, together. -i alone still lets the agent offer everything it
		// holds first, and sshd allows six attempts before refusing — so a
		// developer with a handful of keys is refused *before* the right one
		// is tried, on a key that is correctly installed. This is the single
		// most common first-contact failure the platform has.
		args = append(args, "-o", "IdentitiesOnly=yes", "-i", t.Identity)
	}

	return args
}

// Shell is an interactive shell in the application container.
//
// No command, so the forced command runs its own default — `podman compose
// exec <service> bash -l`, with a terminal.
func (t *Target) Shell() *exec.Cmd {
	args := append(t.options(), "-t", t.Destination())
	if first := t.named(""); first != "" {
		// The name is a command to ssh, and the -t above is what still
		// gives it a terminal.
		args = append(args, first)
	}

	return command("ssh", args...)
}

// named puts the environment's name in front of a remote command, where the
// machine carries several.
func (t *Target) named(remote string) string {
	var words []string
	if t.Name != "" {
		words = append(words, t.Name)
	}
	if t.Container != "" {
		// `@` marks it as a container, so it is never read as a command.
		words = append(words, "@"+t.Container)
	}
	if remote != "" {
		words = append(words, remote)
	}

	return strings.Join(words, " ")
}

// Run is one command, inside the container.
//
// The arguments are joined into the single string sshd puts in
// SSH_ORIGINAL_COMMAND, which the forced command hands to `sh -c` inside the
// container. Each one is quoted here because that string is parsed by a shell
// on the other side, and an unquoted argument with a space in it arrives as
// two.
func (t *Target) Run(command_ []string, tty bool) *exec.Cmd {
	args := t.options()

	if tty {
		args = append(args, "-t")
	} else {
		// -T where there is no terminal. Without it ssh warns about a
		// pseudo-terminal it could not allocate, on stderr, into the middle
		// of whatever a pipeline was capturing.
		args = append(args, "-T")
	}

	args = append(args, t.Destination(), t.named(quoteAll(command_)))

	return command("ssh", args...)
}

// Verb runs one of the forced command's verbs.
//
// A verb is a word — `db-export`, `db-import`, `files-path` — that the agent
// expands inside the container into the database engine's own client, with
// credentials that come from the container's environment and never appear on
// a command line. The platform holds the details, which is what makes this a
// word rather than a client, five flags and a password.
func (t *Target) Verb(verb string, tty bool) *exec.Cmd {
	args := t.options()

	if tty {
		// A prompt. The pty is what makes the engine's client interactive
		// rather than something that reads stdin and exits.
		args = append(args, "-t")
	} else {
		// A stream: a dump going out or coming in. A pseudo-terminal in the
		// middle of either would translate newlines and corrupt it.
		args = append(args, "-T")
	}

	args = append(args, t.Destination(), t.named(verb))

	return command("ssh", args...)
}

// Rsync builds an rsync in one direction.
//
// `-e ssh …` rather than rsync's own --port, because the port is only half of
// what has to be passed and the identity is the other half.
func (t *Target) Rsync(source, destination string, extra []string) *exec.Cmd {
	transport := append([]string{"ssh"}, t.options()...)

	// Quoted, because rsync splits -e on spaces and honours quotes: a
	// ProxyCommand is one argument with spaces in it.
	args := []string{
		"-avz",
		"--human-readable",
		"-e", quoteAll(transport),
	}
	if t.Name != "" {
		// rsync starts `rsync --server …` on the far side; the name in front
		// of it is what tells the forced command which environment.
		args = append(args, "--rsync-path="+t.Name+" rsync")
	}
	args = append(args, extra...)
	args = append(args, source, destination)

	return command("rsync", args...)
}

// Remote qualifies a path on the far side for rsync.
func (t *Target) Remote(path string) string {
	return t.Destination() + ":" + path
}

// command builds an exec.Cmd wired to this process's streams.
//
// Wired rather than captured: an interactive shell needs the terminal, a
// dump needs stdout to be the file the caller redirected, and rsync's
// progress needs stderr. Nothing here should ever buffer.
func command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd
}

// quoteAll renders arguments as one shell-safe string.
func quoteAll(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = quote(arg)
	}

	return strings.Join(quoted, " ")
}

// quote wraps a single argument for the shell on the far side.
//
// Wrapped in single quotes, with an embedded single quote broken out into an
// escaped one. That is the only form safe for every other character, because
// nothing inside single quotes is special to a POSIX shell. Left bare only
// where every character is
// one that cannot mean anything, which keeps the common case readable in the
// `--dry-run` output and in a bug report.
func quote(arg string) string {
	if arg == "" {
		return "''"
	}

	if isPlain(arg) {
		return arg
	}

	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

func isPlain(arg string) bool {
	for _, r := range arg {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '/', r == ':', r == '=', r == '@', r == ',', r == '+':
		default:
			return false
		}
	}

	return true
}

// Describe renders a command for printing.
//
// For --dry-run, and for the line the CLI shows when it wants somebody to be
// able to run the same thing themselves. Quoted as a shell would need it, so
// what is printed is what can be pasted.
func Describe(cmd *exec.Cmd) string {
	parts := make([]string, 0, len(cmd.Args))
	for _, arg := range cmd.Args {
		parts = append(parts, quote(arg))
	}

	return strings.Join(parts, " ")
}
