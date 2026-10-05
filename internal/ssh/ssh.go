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
func (t *Target) Destination() string {
	return t.User + "@" + t.Host
}

// options are the -o and -p arguments every invocation shares.
func (t *Target) options() []string {
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
	if t.Name != "" {
		// The name is a command to ssh, and the -t above is what still
		// gives it a terminal.
		args = append(args, t.Name)
	}

	return command("ssh", args...)
}

// named puts the environment's name in front of a remote command, where the
// machine carries several.
func (t *Target) named(remote string) string {
	if t.Name == "" {
		return remote
	}

	return t.Name + " " + remote
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

	args := []string{
		"-avz",
		"--human-readable",
		"-e", strings.Join(transport, " "),
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
