package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/vallic/vallic-cli/internal/api"
	"github.com/vallic/vallic-cli/internal/auth"
	"github.com/vallic/vallic-cli/internal/resolve"
)

// Root is the whole command tree.
func Root() *Command {
	return &Command{
		Name:    "vallic",
		Summary: "work on Vallic Cloud sites from a terminal",
		Long: `Deploy, open a shell, move a database, read an activity log.

Inside a checkout the project and environment are worked out from the git
remote and the current branch, so --project and --environment are rarely
needed. A branch that matches no environment is refused rather than defaulted
to production.`,
		Children: []*Command{
			// Signing in.
			loginCommand(),
			logoutCommand(),
			whoamiCommand(),
			teamCommand(),

			// Starting in a checkout. Both run before anything on the
			// platform has to exist: init needs no credential at all, and
			// link is what the resolver's "cannot tell which project you
			// mean" is asking for.
			initCommand(),
			linkCommand(),

			// Finding your way around.
			projectCommand(),
			envCommand(),
			serverCommand(),
			statusCommand(),
			urlCommand(),

			// Configuring what a site runs with.
			varCommand(),
			serviceCommand(),
			validateCommand(),
			domainCommand(),

			// Keeping copies.
			backupCommand(),

			// Deploying.
			deployCommand(),
			rollbackCommand(),
			// Before deploy in the list because it is before it in the
			// order of work: a build produces the release a deploy ships.
			buildCommand(),
			redeployCommand(),
			releaseCommand(),
			activityCommand(),
			// Where the site's own logs are, which is not here. Beside
			// activity because the two names are the ones people confuse.
			logsCommand(),

			// Shell plumbing. Last because it is about the terminal rather
			// than about anything on the platform.
			completionCommand(),

			// Keeping this binary current. A question about the binary
			// itself rather than about anything on the platform, which is
			// why it sits on its own: everything above asks the control
			// plane to do something to a site, and this asks it which
			// version of the client to run and then replaces the client.
			selfUpdateCommand(),

			// Getting inside.
			sshCommand(),
			drushCommand(),
			dbCommand(),
			sqlCommand(),
			mountCommand(),
		},
	}
}

// confirm asks a yes/no question on stderr.
//
// Returns false with no terminal. A confirmation that defaults to yes in CI
// is a confirmation that does not exist, and the commands that ask are the
// ones where that matters — so a pipeline that means it passes --yes, or
// names the environment, rather than being assumed to have meant it.
func (e *Env) confirm(question string) bool {
	if !e.Interactive() {
		return false
	}

	fmt.Fprintf(e.Err, "%s [y/N] ", question)

	line, err := bufio.NewReader(e.In).ReadString('\n')
	if err != nil {
		return false
	}

	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// ExitCode maps an error onto the code a pipeline reads.
//
// One place, so the mapping is a table rather than a decision each command
// makes differently. The order matters: the specific types first, then the
// classes of API error, then the fallback.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}

	// A child process's own status, passed through. `vallic ssh -- drush
	// status` that exits 1 exits 1 here, because the caller is asking about
	// drush and not about the CLI.
	var passthrough *PassthroughError
	if errors.As(err, &passthrough) {
		return passthrough.Code
	}

	var taskFailed *TaskFailedError
	if errors.As(err, &taskFailed) {
		return ExitTaskFailed
	}

	var usage *UsageError
	if errors.As(err, &usage) {
		return ExitUsage
	}

	// An unresolvable target is a usage problem: the fix is an argument, and
	// the error already names which one.
	var ambiguous *resolve.Ambiguous
	if errors.As(err, &ambiguous) {
		return ExitUsage
	}

	if errors.Is(err, auth.ErrNoCredential) || api.IsUnauthorized(err) {
		return ExitUnauthenticated
	}

	// Reached, but refused. An API error carries a status, so it is the
	// platform answering rather than the network failing.
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		return ExitRefused
	}

	var oauthErr *auth.OAuthError
	if errors.As(err, &oauthErr) {
		return ExitUnauthenticated
	}

	// Anything that never got an answer.
	if errors.Is(err, context.DeadlineExceeded) || isConnectionFailure(err) {
		return ExitUnreachable
	}

	return ExitRefused
}

// isConnectionFailure reports whether nothing on the other end answered.
//
// Matched on the wrapping this CLI adds rather than by unwrapping to a net
// error, because the message is the contract: every transport failure in
// internal/api and internal/auth is wrapped as "cannot reach <url>", and
// that is deliberately the one phrase both packages use.
func isConnectionFailure(err error) bool {
	return strings.Contains(err.Error(), "cannot reach ")
}

// Report writes an error the way the CLI reports errors, and returns its code.
//
// The message goes to stderr whatever --format says: a JSON consumer reading
// stdout should find valid JSON or nothing, never an error object it has to
// distinguish from a result.
func Report(env *Env, root *Command, err error) int {
	if err == nil {
		return ExitOK
	}

	code := ExitCode(err)

	// A child process that failed has already said why, on the stderr it
	// inherited. Saying it again in the CLI's own words would be two errors
	// for one failure.
	var passthrough *PassthroughError
	if errors.As(err, &passthrough) {
		return code
	}

	fmt.Fprintf(env.Err, "vallic: %s\n", err)

	var usage *UsageError
	if errors.As(err, &usage) {
		cmd, path := usage.Command, []string{root.Name}

		switch {
		case cmd != nil && cmd != root:
			// Raised by the dispatcher, which names the command it was
			// walking. Its own path is not carried, so the best available is
			// the root plus its name.
			path = append(path, cmd.Name)
		case cmd == nil && env.command != nil:
			// Raised inside a Run closure. Execute remembered which command
			// that was, along with how it was reached — so `vallic team
			// switch` prints what switch takes rather than the whole tree.
			cmd, path = env.command, env.path
		default:
			cmd = root
		}

		fmt.Fprintln(env.Err)
		PrintHelp(env.Err, cmd, path)
	}

	if errors.Is(err, auth.ErrNoCredential) {
		fmt.Fprintln(env.Err, "\nSign in with: vallic login")
	}

	return code
}
