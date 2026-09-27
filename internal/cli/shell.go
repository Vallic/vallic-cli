package cli

import (
	"context"
	"flag"
	"fmt"
	"os/exec"

	vssh "github.com/vallic/vallic-cli/internal/ssh"
)

// identityFlag is shared by every command that opens a connection.
func identityFlag(fs *flag.FlagSet, into *string) {
	fs.StringVar(into, "identity", "", "the private key to offer (default: VALLIC_SSH_KEY, or whatever ssh would choose)")
	fs.StringVar(into, "i", "", "the private key to offer")
}

// target resolves an environment and builds a connection to it.
func (e *Env) sshTarget(ctx context.Context, positional, identity string) (*vssh.Target, error) {
	resolved, err := e.ResolveEnvironment(ctx, positional)
	if err != nil {
		return nil, err
	}

	detail, err := e.Detail(ctx, resolved)
	if err != nil {
		return nil, err
	}

	target, err := vssh.New(detail, identity)
	if err != nil {
		return nil, err
	}

	if !target.HasKeys {
		// A warning, not a refusal. The control plane says whether this
		// person has a key on file, and without one the connection is
		// correctly formed and will still be refused — so saying where the
		// fix is beats letting sshd answer with "Permission denied
		// (publickey)".
		cfg, _ := e.Config()
		e.Printer.Warn("you have no SSH key on file; add one at %s", keysPage(cfg.API))
	}

	return target, nil
}

// sshCommand opens a shell, or runs one command.
func sshCommand() *Command {
	var (
		identity string
		dryRun   bool
	)

	cmd := &Command{
		Name:    "ssh",
		Summary: "open a shell in an environment",
		Usage:   "ssh [<env>] [-- <command>...]",
		Long: `Lands in the application container as the user the site runs as,
in /var/www/html, with the container's own environment — so drush, DB_HOST and
the rest are all there. The live code is one level down in current/, and it is
read only: a change to it goes through a deploy.

Nothing runs on the host. The account on the far side has a forced command
that hands what you asked for to a shell inside the container, and sshd
refuses every kind of forwarding.

With -- it runs one command instead of opening a shell.`,
		Flags: func(fs *flag.FlagSet) {
			identityFlag(fs, &identity)
			fs.BoolVar(&dryRun, "dry-run", false, "print the ssh command instead of running it")
		},
	}

	cmd.Run = func(ctx context.Context, env *Env, args []string) error {
		positional, remote := splitRemote(env, args)

		target, err := env.sshTarget(ctx, positional, identity)
		if err != nil {
			return err
		}

		var process *exec.Cmd
		if len(remote) > 0 {
			process = target.Run(remote, false)
		} else {
			process = target.Shell()
		}

		return runOrPrint(env, process, dryRun)
	}

	return cmd
}

// drushCommand runs drush in the application container.
//
// A passthrough over the same forced command `ssh --` uses, and nothing more.
// It exists because a Drupal host whose CLI does not say `drush` is a Drupal
// host that looks like it does not know what it is hosting, and because
// `vallic ssh -- drush` is three words of ceremony around the one people
// actually want.
//
// Not gated on the project being Drupal. The environment detail does not carry
// a project type, and asking for one would be a second request to refuse
// something the container refuses for itself in a sentence that names drush.
func drushCommand() *Command {
	var (
		identity string
		dryRun   bool
	)

	cmd := &Command{
		Name:    "drush",
		Summary: "run drush in an environment",
		Usage:   "drush [<env> --] <args>...",
		Long: `Runs drush inside the application container, as the user the site
runs as, with the container's own environment. The same forced command
` + "`vallic ssh`" + ` uses, so there is no capability here that a shell did not
already have.

    vallic drush status
    vallic drush cr
    vallic drush sql:query 'SELECT 1'

Everything after the command name is drush's, so flags are passed through
untouched rather than read here. That is why naming an environment needs the
separator, which is the one shape a plain argument could be confused with:

    vallic drush staging -- updb -y

Without it the environment comes from the checkout, like every other command.`,
		Flags: func(fs *flag.FlagSet) {
			identityFlag(fs, &identity)
			fs.BoolVar(&dryRun, "dry-run", false, "print the ssh command instead of running it")
		},
	}

	cmd.Run = func(ctx context.Context, env *Env, args []string) error {
		positional, remote := splitDrush(env, args)

		target, err := env.sshTarget(ctx, positional, identity)
		if err != nil {
			return err
		}

		// drush with nothing after it prints drush's own help, which is a
		// reasonable thing to have asked for and not worth refusing here.
		return runOrPrint(env, target.Run(append([]string{"drush"}, remote...), false), dryRun)
	}

	return cmd
}

// splitDrush reads an optional environment and the drush command.
//
// Deliberately not splitRemote. `vallic ssh production ls` reads the first
// word as the environment because that is how ssh is typed, but
// `vallic drush status` is the natural form and has no environment in it at
// all. Reading `status` as an environment would refuse a correct command with
// a message about environments, so without a separator every word is drush's.
func splitDrush(env *Env, args []string) (positional string, remote []string) {
	if passthrough, found := env.Passthrough(); found {
		return firstOf(args), passthrough
	}

	return "", args
}

// dbCommand moves a database in or out.
func dbCommand() *Command {
	var identity string
	var dryRun bool

	flags := func(fs *flag.FlagSet) {
		identityFlag(fs, &identity)
		fs.BoolVar(&dryRun, "dry-run", false, "print the ssh command instead of running it")
	}

	return &Command{
		Name:    "db",
		Summary: "move a database in or out of an environment",
		Long: `Both directions stream, so size is not a limit and neither is a
request timeout.

What runs on the far side is a verb, not a command: the agent expands
db-export and db-import inside the container into the engine's own client —
mysqldump or pg_dump, mysql or psql — with credentials that come from the
container's environment and never appear on a command line. Which engine it
is is decided in the container, not guessed from here.`,
		Children: []*Command{
			{
				Name:    "export",
				Summary: "stream a dump out, to stdout",
				Usage:   "db export [<env>] > dump.sql",
				Flags:   flags,
				Run: func(ctx context.Context, env *Env, args []string) error {
					target, err := env.sshTarget(ctx, first(args), identity)
					if err != nil {
						return err
					}

					if target.DBExport == "" {
						return fmt.Errorf("this environment runs no database")
					}

					// To stderr, because stdout is the dump. A progress line
					// in the middle of a redirect would corrupt the file.
					env.Printer.Say("Streaming %s out of %s…", "the database", target.Host)

					return runOrPrint(env, target.Verb(target.DBExport, false), dryRun)
				},
			},
			{
				Name:    "import",
				Summary: "stream a dump in, from stdin",
				Usage:   "db import [<env>] < dump.sql",
				Flags:   flags,
				Run: func(ctx context.Context, env *Env, args []string) error {
					target, err := env.sshTarget(ctx, first(args), identity)
					if err != nil {
						return err
					}

					if target.DBImport == "" {
						return fmt.Errorf("this environment runs no database")
					}

					if env.Interactive() {
						// stdin is the terminal, which means no redirect was
						// given and this would sit waiting for somebody to
						// type a database. Better to say so than to hang.
						return Usagef(nil, "nothing to import: redirect a dump into this command, as `vallic db import < dump.sql`")
					}

					return runOrPrint(env, target.Verb(target.DBImport, false), dryRun)
				},
			},
		},
	}
}

// sqlCommand is a database prompt.
func sqlCommand() *Command {
	var identity string
	var dryRun bool

	return &Command{
		Name:    "sql",
		Summary: "open a database prompt in an environment",
		Usage:   "sql [<env>]",
		Long: `A prompt inside the container, not a local port.

sshd on a managed machine refuses forwarding outright, deliberately — so
there is no tunnel to a tenant's database and this is not one. What you get is
the engine's own client, running beside the site, reading its credentials from
the container's environment.`,
		Flags: func(fs *flag.FlagSet) {
			identityFlag(fs, &identity)
			fs.BoolVar(&dryRun, "dry-run", false, "print the ssh command instead of running it")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			target, err := env.sshTarget(ctx, first(args), identity)
			if err != nil {
				return err
			}

			// The verb the control plane named, not one spelled here. Empty
			// means the stack runs no database — which is also the only
			// honest way to know, since the engine is decided in the
			// container rather than out here.
			if target.DBCLI == "" {
				return fmt.Errorf("this environment runs no database")
			}

			// A terminal, because this is a prompt: the forced command asks
			// sshd for a pty and that is what makes the client interactive
			// rather than something that reads stdin and exits.
			return runOrPrint(env, target.Verb(target.DBCLI, true), dryRun)
		},
	}
}

// mountCommand copies files in and out.
func mountCommand() *Command {
	var identity string
	var dryRun bool
	var delete_ bool

	flags := func(fs *flag.FlagSet) {
		identityFlag(fs, &identity)
		fs.BoolVar(&dryRun, "dry-run", false, "print the rsync command instead of running it")
	}

	return &Command{
		Name:    "mount",
		Aliases: []string{"files"},
		Summary: "copy files in and out of an environment",
		Long: `rsync, so a transfer resumes rather than restarting and tens of
thousands of small files are copied incrementally.

Aimed at the environment's shared writable directory, which is the same path
for every project type and survives every deploy. The release itself is
mounted read only, so a path inside the code is a transfer that fails.`,
		Children: []*Command{
			{
				Name:    "path",
				Summary: "print the writable path on the far side",
				Usage:   "mount path [<env>]",
				Flags:   flags,
				Run: func(ctx context.Context, env *Env, args []string) error {
					target, err := env.sshTarget(ctx, first(args), identity)
					if err != nil {
						return err
					}

					env.Printer.Line("%s", target.FilesPath)

					return nil
				},
			},
			{
				Name:    "download",
				Summary: "copy files down, into a local directory",
				Usage:   "mount download [<env>] <local-dir>",
				Flags:   flags,
				Run: func(ctx context.Context, env *Env, args []string) error {
					positional, local, err := splitMountArgs(args)
					if err != nil {
						return err
					}

					target, err := env.sshTarget(ctx, positional, identity)
					if err != nil {
						return err
					}

					// Trailing slashes on both sides, which is what makes
					// rsync copy the *contents* rather than nesting the
					// directory inside itself. The single most common rsync
					// mistake, and one the CLI can simply not make.
					source := target.Remote(withSlash(target.FilesPath))

					return runOrPrint(env, target.Rsync(source, withSlash(local), nil), dryRun)
				},
			},
			{
				Name:    "upload",
				Summary: "copy files up, from a local directory",
				Usage:   "mount upload [<env>] <local-dir>",
				Flags: func(fs *flag.FlagSet) {
					flags(fs)
					fs.BoolVar(&delete_, "delete", false, "remove files on the far side that are not local")
				},
				Run: func(ctx context.Context, env *Env, args []string) error {
					positional, local, err := splitMountArgs(args)
					if err != nil {
						return err
					}

					target, err := env.sshTarget(ctx, positional, identity)
					if err != nil {
						return err
					}

					var extra []string
					if delete_ {
						// Confirmed, because --delete against a live site's
						// files directory is the one flag here that destroys
						// something no deploy puts back.
						if !env.confirm(fmt.Sprintf(
							"--delete will remove files under %s on %s that are not in %s. Continue?",
							target.FilesPath, target.Host, local,
						)) {
							return fmt.Errorf("cancelled")
						}

						extra = append(extra, "--delete")
					}

					destination := target.Remote(withSlash(target.FilesPath))

					return runOrPrint(env, target.Rsync(withSlash(local), destination, extra), dryRun)
				},
			},
		},
	}
}

// splitRemote separates an environment name from the command to run.
//
// With a "--" the split is unambiguous and the dispatcher has already made
// it: everything before is this command's, everything after is the far
// side's. Without one, a second argument is still taken as a command, so
// `vallic ssh production drush status` works — but the help shows the
// separator, because without it a command whose first word happens to name
// an environment cannot be told from an environment.
func splitRemote(env *Env, args []string) (positional string, remote []string) {
	if passthrough, found := env.Passthrough(); found {
		return firstOf(args), passthrough
	}

	if len(args) > 1 {
		return args[0], args[1:]
	}

	return firstOf(args), nil
}

// splitMountArgs reads an optional environment and a required local path.
func splitMountArgs(args []string) (positional, local string, err error) {
	switch len(args) {
	case 1:
		return "", args[0], nil
	case 2:
		return args[0], args[1], nil
	case 0:
		return "", "", Usagef(nil, "name a local directory")
	default:
		return "", "", Usagef(nil, "unexpected argument %q", args[2])
	}
}

func withSlash(path string) string {
	if path == "" {
		return path
	}

	if path[len(path)-1] == '/' {
		return path
	}

	return path + "/"
}

func firstOf(args []string) string {
	if len(args) > 0 {
		return args[0]
	}

	return ""
}

// runOrPrint runs a command, or prints it.
func runOrPrint(env *Env, cmd *exec.Cmd, dryRun bool) error {
	if dryRun {
		env.Printer.Line("%s", vssh.Describe(cmd))

		return nil
	}

	if _, err := exec.LookPath(cmd.Path); err != nil {
		if cmd.Args[0] == "rsync" {
			return fmt.Errorf("rsync is not installed, and this command needs it")
		}

		return fmt.Errorf("%s is not installed, and this command needs it", cmd.Args[0])
	}

	err := cmd.Run()

	// An exit status from the far side is the far side's answer, not a
	// failure of this CLI — a drush command that exited 1 should exit 1 here
	// and print nothing extra. Reported as its own type so the top level can
	// pass the code through.
	var exitErr *exec.ExitError
	if err != nil && asExitError(err, &exitErr) {
		return &PassthroughError{Code: exitErr.ExitCode()}
	}

	return err
}

// PassthroughError carries a child process's exit code to the top level.
type PassthroughError struct {
	Code int
}

func (e *PassthroughError) Error() string {
	return fmt.Sprintf("command exited %d", e.Code)
}

func asExitError(err error, target **exec.ExitError) bool {
	if exitErr, ok := err.(*exec.ExitError); ok {
		*target = exitErr

		return true
	}

	return false
}
