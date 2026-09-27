package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/vallic/vallic-cli/internal/api"
	"github.com/vallic/vallic-cli/internal/output"
)

// deployCommand queues a deployment.
func deployCommand() *Command {
	var (
		release int
		wait    bool
		timeout time.Duration
	)

	return &Command{
		Name:    "deploy",
		Summary: "deploy a release to an environment",
		Usage:   "deploy [<env>] [--release <n>] [--wait]",
		Long: `With no --release, deploys the newest release that is ready.
Naming one is what makes a script repeatable.

--release takes the number in the ` + "`#`" + ` column of ` + "`vallic release list`" + `,
which is the project's own sequence. That is deliberately not the id: ids are
global across the platform, so the number beside a build is the only one that
means anything to the person reading it.

The control plane decides whether a deployment may happen; this only asks. A
deployment already in flight answers 409, which --wait treats as something to
wait for rather than an error to retry differently.

Deploying is not gated on confirming who you are, on any environment. A deploy
moves a pointer and is the thing you do *during* an incident, so a prompt here
would arrive at the worst moment and buy nothing. Protection still decides who
may ask: ` + "`operate`" + ` on a protected environment is a developer's, and that is
the whole of the check.`,
		Flags: func(fs *flag.FlagSet) {
			fs.IntVar(&release, "release", 0, "the release number to deploy (default: the newest that is ready)")
			fs.BoolVar(&wait, "wait", false, "block until the deployment finishes")
			fs.DurationVar(&timeout, "timeout", 30*time.Minute, "how long --wait waits before giving up")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			target, err := env.ResolveEnvironment(ctx, first(args))
			if err != nil {
				return err
			}

			client, err := env.Client()
			if err != nil {
				return err
			}

			detail, err := env.Detail(ctx, target)
			if err != nil {
				return err
			}

			if detail.Protected && env.Interactive() {
				// Named, not blocked. A developer deploying production is
				// doing their job — protection guards reshaping an
				// environment, not operating one — but a `vallic deploy` run
				// from the wrong checkout should be recoverable before it
				// happens rather than after.
				if !env.confirm(fmt.Sprintf("Deploy to %s, which is protected?", detail.Name)) {
					return fmt.Errorf("cancelled")
				}
			}

			// The number somebody typed is the project's own sequence, and the
			// route takes an id. Resolved here rather than by sending the
			// number and hoping: a release id that happens to equal somebody
			// else's release number is a deploy of the wrong build reported as
			// a success, and on a young platform where ids and numbers still
			// coincide it would work in testing and stop working later.
			wanted := release

			if wanted > 0 {
				var named api.Release

				wanted, named, err = releaseIDFor(ctx, client, target.Project.ID, release)
				if err != nil {
					return err
				}

				if warning := branchWarning(named, detail.Name, detail.GitRef); warning != "" {
					env.Printer.Warn("%s", warning)
				}
			}

			// No step-up wrapper. Deploying stopped being gated on confirming
			// who you are, so there is no challenge for one to answer, and a
			// retry that can never fire is a retry somebody later reasons
			// about as though it can.
			deployment, err := client.Deploy(ctx, target.Environment.ID, wanted)
			if err != nil {
				return describeDeployFailure(err, detail)
			}

			// A fallback, and only that. The deploy route now answers with the
			// same shape as rollback and redeploy, number included, so this
			// fills nothing in against a current control plane. It matters
			// against one that predates that change: there the answer carries
			// no number and Describe() would say "Queued release" with nothing
			// after it, for the one case where the caller certainly knew which
			// build it meant. Filled only where this asked for a specific one,
			// because anywhere else it would be inventing a fact the control
			// plane did not state.
			if deployment.ReleaseNumber == nil && release > 0 {
				deployment.ReleaseNumber = &release
			}

			env.Printer.Good("%s (deployment %d)", deployment.Describe(), deployment.ID)

			if !wait {
				if env.Printer.Structured() {
					return env.Printer.Value(map[string]any{"deployment": deployment})
				}

				return nil
			}

			return env.waitForDeployment(ctx, deployment, timeout)
		},
	}
}

// waitForDeployment follows the deployment's log until the work stops moving.
//
// Polled rather than streamed, because the control plane answers with a
// window and a next offset: a long poll there would hold a PHP-FPM worker for
// its whole duration, and a deploy watched by six developers would be six
// workers doing nothing.
//
// The log goes to **stderr**, not stdout. During a deploy it is progress
// rather than the result — which is what keeps `--format json` valid, since
// stdout must carry the JSON and nothing else. `vallic activity log` prints
// the same bytes to stdout, because there the log is what was asked for.
func (e *Env) waitForDeployment(ctx context.Context, deployment *api.Deployment, timeout time.Duration) error {
	client, err := e.Client()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	e.Printer.Say("Waiting for deployment %d…", deployment.ID)

	state, err := e.follow(ctx, client, deployment.ID, e.Err)
	if err != nil {
		return err
	}

	// One last read of the task itself, for the message and the output. The
	// log route carries the state so the loop above needs one request per
	// tick; it deliberately does not carry the rest, because that is the
	// task's contract and not the log's.
	task, err := client.Task(ctx, deployment.ID)
	if err != nil {
		// The work finished and this is only the epilogue failing. Reporting
		// the state we already saw beats turning a successful deploy into an
		// error because one extra request did not land.
		if state == "succeeded" {
			e.Printer.Good("Deployed.")

			return nil
		}

		return err
	}

	if !task.Succeeded() {
		// Exit code 4, not 1. "We would not deploy that" and "we deployed it
		// and it broke" are different outcomes and a pipeline wants to tell
		// them apart.
		return &TaskFailedError{Task: task}
	}

	e.Printer.Good("Deployed.")

	if e.Printer.Structured() {
		return e.Printer.Value(map[string]any{"task": task})
	}

	return nil
}

// followTask watches any task and reports how it ended.
//
// The generic form of waitForDeployment, for the work that is not a deploy: a
// backup, an export, a restore. It reports in the task's own words rather than
// saying "deployed", and it exits 4 on a failure for the same reason — a
// pipeline needs to tell "the platform refused" from "the platform tried and
// it broke".
func (e *Env) followTask(ctx context.Context, id int, timeout time.Duration) error {
	client, err := e.Client()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	state, err := e.follow(ctx, client, id, e.Err)
	if err != nil {
		return err
	}

	task, err := client.Task(ctx, id)
	if err != nil {
		// The work finished and only the epilogue failed. Reporting the state
		// already seen beats turning finished work into an error because one
		// extra request did not land.
		if state == "succeeded" {
			e.Printer.Good("Finished.")

			return nil
		}

		return err
	}

	if !task.Succeeded() {
		return &TaskFailedError{Task: task}
	}

	e.Printer.Good("Finished.")

	if e.Printer.Structured() {
		return e.Printer.Value(map[string]any{"task": task})
	}

	return nil
}

// follow prints a task's log as it arrives and returns its final state.
//
// The cadence backs off while nothing is happening and collapses to nothing
// while output is flowing: `truncated` means the control plane already holds
// more than it just sent, so waiting would only add latency to bytes that
// exist. A deploy that takes four seconds should not be reported four seconds
// late, and one that takes twenty minutes should not be asked about four
// hundred times.
func (e *Env) follow(ctx context.Context, client *api.Client, id int, out io.Writer) (string, error) {
	const (
		minInterval = 500 * time.Millisecond
		maxInterval = 5 * time.Second
	)

	interval := minInterval
	offset := 0
	state := ""
	limited := 0

	// Every poll costs a request whether or not it returns anything, and a
	// transient failure mid-deploy is not a failed deploy: the work is
	// running on the platform whether or not this process can see it.
	failures := 0
	const maxFailures = 5

	for {
		log, err := client.TaskLog(ctx, id, offset)
		if err != nil {
			if ctx.Err() != nil {
				return state, fmt.Errorf(
					"gave up waiting for %d after the timeout; it may still be running\n  check it with: vallic activity get %d",
					id, id,
				)
			}

			// 429 is the one 4xx that means "ask again". The control plane
			// limits a token to 600 requests a minute and says, in as many
			// words, to wait and repeat the request unchanged — so returning
			// here would abandon a deploy that is still running, over a
			// pause the server asked for. Counted separately and waited out
			// for longer than a 5xx, because the window it is waiting for is
			// a minute wide.
			if api.IsRateLimited(err) {
				limited++

				if limited >= maxLimited {
					return state, fmt.Errorf(
						"%w\n  still rate limited after %s; the deploy is unaffected and may still be running\n"+
							"  watch it with: vallic activity log %d --follow",
						err, (time.Duration(maxLimited) * rateLimitWait).Round(time.Second), id,
					)
				}

				// Once, not on every retry: a wall of identical warnings is
				// how somebody stops reading them.
				if limited == 1 {
					e.Printer.Warn("rate limited by the control plane; waiting rather than giving up on the deploy")
				}

				if waitErr := sleep(ctx, rateLimitWait); waitErr != nil {
					return state, waitErr
				}

				continue
			}

			// A 5xx or a dropped connection is worth retrying; any other
			// refusal is not — a 404 or a 403 will answer the same way
			// forever.
			var apiErr *api.Error
			if errors.As(err, &apiErr) && apiErr.Status < 500 {
				return state, err
			}

			failures++
			if failures >= maxFailures {
				return state, err
			}

			if waitErr := sleep(ctx, interval); waitErr != nil {
				return state, waitErr
			}

			continue
		}

		failures = 0
		limited = 0
		state = log.State

		// A log shorter than where we were reading means retention removed it
		// under us. Said once rather than silently restarting from nothing,
		// which would reprint output the person has already read.
		if log.Offset < offset {
			e.Printer.Warn("the log was truncated while following it")
		}

		if log.Content != "" {
			if _, writeErr := io.WriteString(out, log.Content); writeErr != nil {
				return state, writeErr
			}
		}

		offset = log.NextOffset

		if log.Complete {
			return state, nil
		}

		if log.Truncated {
			// More is already written, so this asks again immediately rather
			// than waiting out a backoff for bytes that exist — but not
			// *without* waiting. This branch used to `continue` with no sleep
			// at all, and a loop whose only brake is the round trip runs at
			// whatever the network allows: measured at 23,873 requests a
			// second against a local control plane, which is 2,387 times the
			// 600 a minute one token is allowed, and still several times over
			// it across the internet. A deploy with a log bigger than one
			// window would rate-limit its own token within seconds.
			if waitErr := sleep(ctx, drainGap); waitErr != nil {
				return state, waitErr
			}

			interval = minInterval

			continue
		}

		if waitErr := sleep(ctx, interval); waitErr != nil {
			return state, waitErr
		}

		if interval < maxInterval {
			interval *= 2
			if interval > maxInterval {
				interval = maxInterval
			}
		}
	}
}

// drainGap is the least time between two reads of the same log.
//
// The ceiling it keeps this under is the control plane's: 600 requests a
// minute per token, which is ten a second. At 200ms this loop asks five times
// a second at most, leaving half the token's allowance for everything else
// using it — which on a CI runner is every other job sharing that credential,
// because the limit counts the token and not the machine.
//
// It costs nothing worth having. A window is 256 KiB, so draining still moves
// at better than a megabyte a second, and the case this protects is the one
// where that matters least: a log already written, being read after the fact.
const drainGap = 200 * time.Millisecond

// rateLimitWait is how long to wait out a 429, and how many times.
//
// The volume window is a minute wide, so riding one out takes up to a minute:
// eight waits of ten seconds covers it with room to spare. Slow, and the
// alternative is what this replaced — abandoning a running deploy the first
// time the server asked for a pause.
//
// Variables rather than constants only so the tests that hold this behaviour
// do not have to spend eighty seconds proving it. Nothing outside a test
// changes them.
var (
	rateLimitWait = 10 * time.Second
	maxLimited    = 8
)

// sleep waits, or gives up if the context does first.
func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// describeDeployFailure adds the sentence that makes a refusal actionable.
func describeDeployFailure(err error, detail *api.EnvironmentDetail) error {
	switch api.Code(err) {
	case api.CodeEnvironmentBusy:
		return fmt.Errorf("%w\n  a deployment is already running; wait for it, or watch it with: vallic activity list", err)
	case api.CodeNoRelease:
		return fmt.Errorf(
			"%w\n  %s builds from %q — push to it, or check that the last build finished",
			err, detail.Name, detail.GitRef,
		)
	default:
		return err
	}
}

// activityCommand reads what the platform has been doing.
func activityCommand() *Command {
	return &Command{
		Name:    "activity",
		Summary: "read what the platform has been doing",
		Long: `A task is the platform narrating its own work — a deploy's
steps, a provision's output. It is not the site's own request and error logs,
which go to wherever the project sends them and never to the control plane.`,
		Children: []*Command{
			activityListCommand(),
			activityLogCommand(),
			{
				Name:    "get",
				Summary: "show one task",
				Usage:   "activity get <id>",
				Run: func(ctx context.Context, env *Env, args []string) error {
					if len(args) == 0 {
						return Usagef(nil, "name a task id")
					}

					id, err := atoi(args[0])
					if err != nil {
						return Usagef(nil, "%q is not a task id", args[0])
					}

					client, err := env.Client()
					if err != nil {
						return err
					}

					task, err := client.Task(ctx, id)
					if err != nil {
						return err
					}

					if env.Printer.Structured() {
						return env.Printer.Value(map[string]any{"task": task})
					}

					table := output.Table{
						Columns: []string{"field", "value"},
						Rows: [][]string{
							{"id", fmt.Sprint(task.ID)},
							{"type", task.Type},
							{"state", task.State},
							{"attempts", fmt.Sprint(task.Attempts)},
						},
					}

					if task.Message != "" {
						table.Rows = append(table.Rows, []string{"message", task.Message})
					}

					if task.Finished != nil {
						table.Rows = append(table.Rows, []string{
							"finished", time.Unix(*task.Finished, 0).Format(time.RFC3339),
						})
					}

					return env.Printer.Print(table, map[string]any{"task": task})
				},
			},
		},
	}
}

// activityLogCommand prints a task's output.
func activityLogCommand() *Command {
	var (
		follow  bool
		timeout time.Duration
	)

	return &Command{
		Name:    "log",
		Summary: "print a task's output",
		Usage:   "activity log <id> [--follow]",
		Long: `Prints to stdout, because here the log is what was asked for.

During ` + "`vallic deploy --wait`" + ` the same bytes go to stderr instead, since
there they are progress and stdout has to stay parseable.

--follow keeps printing until the work stops moving. It backs off while
nothing is happening and asks again at once while output is flowing, because
the control plane says which of those it is.`,
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&follow, "follow", false, "keep printing until the work finishes")
			fs.BoolVar(&follow, "f", false, "keep printing until the work finishes")
			fs.DurationVar(&timeout, "timeout", 30*time.Minute, "how long --follow waits before giving up")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			if len(args) == 0 {
				return Usagef(nil, "name a task id")
			}

			id, err := atoi(args[0])
			if err != nil {
				return Usagef(nil, "%q is not a task id", args[0])
			}

			client, err := env.Client()
			if err != nil {
				return err
			}

			if follow {
				ctx, cancel := context.WithTimeout(ctx, timeout)
				defer cancel()

				_, err := env.follow(ctx, client, id, env.Out)

				return err
			}

			// One read, from the beginning, looping only while the control
			// plane says it is holding more than it just sent. Without the
			// loop a log longer than one window would be silently cut off at
			// 256 KiB, which is the kind of truncation somebody discovers by
			// missing the error they were looking for.
			offset := 0
			for {
				log, err := client.TaskLog(ctx, id, offset)
				if err != nil {
					return err
				}

				if _, err := io.WriteString(env.Out, log.Content); err != nil {
					return err
				}

				offset = log.NextOffset

				if !log.Truncated {
					return nil
				}

				// The same brake `follow` has, for the same reason: without
				// it this is a tight request loop over however many windows
				// the log is long, and the token it is spending belongs to
				// everything else using that credential.
				if err := sleep(ctx, drainGap); err != nil {
					return err
				}
			}
		},
	}
}

func atoi(s string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, err
	}

	return n, nil
}

// releaseIDFor turns the number a person was shown into the id the route takes.
//
// `release list` prints the project's own sequence, and the deploy route loads
// by entity id, so sending one where the other is expected either refuses or —
// where an id of that value happens to exist in the same project — deploys a
// different build and reports success. One extra request buys the guarantee
// that the number in the `#` column is the number that deploys.
func releaseIDFor(ctx context.Context, client *api.Client, projectID int, number int) (int, api.Release, error) {
	// The route's own ceiling. A build older than this cannot be named by
	// number, which is said plainly below rather than left as a bare refusal.
	const window = 100

	// Every branch, not the target environment's. A build from another branch
	// is a legitimate thing to name -- see branchWarning -- so narrowing here
	// would turn "that is unusual" into "no such release", which is false.
	releases, err := client.Releases(ctx, projectID, window, "")
	if err != nil {
		return 0, api.Release{}, err
	}

	for i := range releases {
		if releases[i].Number == number {
			return releases[i].ID, releases[i], nil
		}
	}

	if len(releases) == 0 {
		return 0, api.Release{}, fmt.Errorf("this project has no releases yet")
	}

	newest := releases[0].Number
	oldest := releases[len(releases)-1].Number

	if number > newest {
		return 0, api.Release{}, fmt.Errorf(
			"this project has no release %d; the newest is %d\n  `vallic release list` shows what has been built",
			number, newest,
		)
	}

	return 0, api.Release{}, fmt.Errorf(
		"release %d is older than the %d builds this can look through, which reach back to %d\n"+
			"  `vallic release list --limit %d` shows them",
		number, len(releases), oldest, window,
	)
}

// branchWarning says so when a named release was built from somewhere else.
//
// A warning and not a refusal, because the control plane permits it. Only the
// *unnamed* path is branch-scoped: UserDeployController::release() checks an
// explicitly named release for project membership and nothing else, so
// `--release 9` will put a feature branch's build onto production and answer
// 202. Refusing here would be this client inventing a rule the platform does
// not have, and the one case that rule would break is the legitimate one --
// putting a hotfix built elsewhere onto production on purpose.
//
// Worth saying because the number came from a list that shows every branch, and
// two builds a person is choosing between look identical apart from a column.
func branchWarning(release api.Release, environment string, ref string) string {
	if release.GitRef == "" || ref == "" || release.GitRef == ref {
		return ""
	}

	return fmt.Sprintf(
		"release %d was built from %q, and %s deploys %q",
		release.Number, release.GitRef, environment, ref,
	)
}
