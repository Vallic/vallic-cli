package cli

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/vallic/vallic-cli/internal/api"
	"github.com/vallic/vallic-cli/internal/output"
)

// releaseCommand lists what has been built.
func releaseCommand() *Command {
	return &Command{
		Name:    "release",
		Summary: "list a project's builds",
		Children: []*Command{
			releaseListCommand(),
		},
	}
}

func releaseListCommand() *Command {
	var (
		limit  int
		branch string
	)

	return &Command{
		Name:    "list",
		Summary: "list builds, newest first",
		Usage:   "release list [--limit <n>] [--branch <name>]",
		Long: `Newest first, by the project's own release number — which is what
` + "`--release`" + ` takes, and is not the same as the id.

` + "`deployable`" + ` is the control plane's own answer, not a reading of the
status: a build can be present and still refused, because its artifact has
been pruned. That is why a rollback onto an old release sometimes cannot
happen, and this column is where to see it coming.

--branch narrows to one branch's builds, which is the list to look at when
choosing what to deploy: ` + "`vallic deploy`" + ` with no --release takes the
newest build of the environment's own branch, so a number from another branch
is one you have to name deliberately.

It is a flag and not the current branch, deliberately. This command asks about
a project and needs no environment, so it works on a checkout of a branch that
has none — and filtering by that branch on its own would answer "nothing has
been built for this project" for a branch nobody has built, which is a
different and much more alarming sentence.`,
		Flags: func(fs *flag.FlagSet) {
			fs.IntVar(&limit, "limit", 0, "how many to show (default 25, most 100)")
			fs.StringVar(&branch, "branch", "", "only builds from this branch (default: every branch)")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			if err := ensureNoExtra(nil, args, 0); err != nil {
				return err
			}

			project, err := env.ResolveProject(ctx)
			if err != nil {
				return err
			}

			client, err := env.Client()
			if err != nil {
				return err
			}

			releases, err := client.Releases(ctx, project.ID, limit, branch)
			if err != nil {
				return err
			}

			// The empty line says which question was asked. "Nothing has been
			// built for this project" is alarming and, where a branch was
			// named, untrue: the project may have plenty.
			empty := "Nothing has been built for this project yet."
			if branch != "" {
				empty = fmt.Sprintf("Nothing has been built from %q.", branch)
			}

			table := output.Table{
				Columns: []string{"#", "status", "branch", "commit", "built", "live on"},
				Empty:   empty,
			}

			for _, release := range releases {
				status := release.Status
				if !release.Deployable && status == "available" {
					// Present, and still not deployable. Worth its own word,
					// because "available" would be a lie a person acts on.
					status = "pruned"
				}

				table.Rows = append(table.Rows, []string{
					fmt.Sprint(release.Number),
					status,
					release.GitRef,
					shortSHA(release.GitSHA),
					ago(release.BuiltAt),
					joinOrDash(release.DeployedTo),
				})
			}

			return env.Printer.Print(table, map[string]any{
				"project":  project.MachineName,
				"releases": releases,
			})
		},
	}
}

// rollbackCommand puts the previous release back.
func rollbackCommand() *Command {
	var (
		wait    bool
		timeout time.Duration
		yes     bool
	)

	return &Command{
		Name:    "rollback",
		Summary: "put the release the current one replaced back",
		Usage:   "rollback [<env>] [--wait]",
		Long: `Deploys the build that was live before the current one.

Not gated on confirming who you are, on any environment. A rollback happens
during the incident, and it only puts back a release that was already live
here: a prompt at three in the morning would make an outage longer and buy
nothing. It is still an ` + "`operate`" + ` action, so who may ask is a question of
role.

Refused when there is nothing behind the current release, or when that
release's artifact has since been pruned. Both answer ` + "`no_release`" + `.`,
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&wait, "wait", false, "block until it finishes")
			fs.BoolVar(&yes, "yes", false, "do not ask for confirmation")
			fs.DurationVar(&timeout, "timeout", 30*time.Minute, "how long --wait waits")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			return env.putBack(ctx, first(args), wait, yes, timeout, "rollback")
		},
	}
}

// redeployCommand runs the live build again.
func redeployCommand() *Command {
	var (
		wait    bool
		timeout time.Duration
		yes     bool
	)

	return &Command{
		Name:    "redeploy",
		Summary: "deploy what is already live, again",
		Usage:   "redeploy [<env>] [--wait]",
		Long: `The same build, through the application's own deploy steps again,
migrations included. So it is a change rather than a repetition, and worth
meaning: it runs whatever the release's deploy steps do, against the data
that is there now.

Not gated on confirming who you are. That was true of deploys and rollbacks
until deploying stopped being gated at all, and this went with them: what a
redeploy runs is what a deploy of the same build would have run.

` + "`backup restore`" + ` is the one thing on this platform that still asks a
person to confirm, because it is the one that overwrites a database rather
than moving a pointer at code.`,
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&wait, "wait", false, "block until it finishes")
			fs.BoolVar(&yes, "yes", false, "do not ask for confirmation")
			fs.DurationVar(&timeout, "timeout", 30*time.Minute, "how long --wait waits")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			return env.putBack(ctx, first(args), wait, yes, timeout, "redeploy")
		},
	}
}

// putBack runs a rollback or a redeploy and reports it.
//
// One body for both, because they differ only in which route is posted to and
// in what the prompt asks. The parts that are the same are the parts most
// worth not having twice: the protected-environment prompt and the wait loop.
func (e *Env) putBack(
	ctx context.Context,
	positional string,
	wait bool,
	yes bool,
	timeout time.Duration,
	kind string,
) error {
	target, err := e.ResolveEnvironment(ctx, positional)
	if err != nil {
		return err
	}

	client, err := e.Client()
	if err != nil {
		return err
	}

	detail, err := e.Detail(ctx, target)
	if err != nil {
		return err
	}

	if detail.Protected && !yes && e.Interactive() {
		question := fmt.Sprintf("Roll %s back, which is protected?", detail.Name)
		if kind == "redeploy" {
			question = fmt.Sprintf("Redeploy %s, which is protected?", detail.Name)
		}

		if !e.confirm(question) {
			return fmt.Errorf("cancelled")
		}
	}

	// No step-up wrapper. Neither of these is gated on confirming who you
	// are any more, so there is no challenge for one to answer.
	var deployment *api.Deployment

	if kind == "rollback" {
		deployment, err = client.Rollback(ctx, target.Environment.ID)
	} else {
		deployment, err = client.Redeploy(ctx, target.Environment.ID)
	}

	if err != nil {
		if api.Code(err) == api.CodeNoRelease {
			return fmt.Errorf("%w\n  `vallic release list` shows what has been built", err)
		}

		return describeDeployFailure(err, detail)
	}

	e.Printer.Good("%s (deployment %d)", deployment.Describe(), deployment.ID)

	if !wait {
		if e.Printer.Structured() {
			return e.Printer.Value(map[string]any{"deployment": deployment})
		}

		return nil
	}

	return e.waitForDeployment(ctx, deployment, timeout)
}

// ago renders a timestamp as a rough age, or a dash.
//
// Rough on purpose: the exact second a build finished is in `--format json`
// for anything that needs it, and a column of full timestamps is a column
// nobody reads.
func ago(at *int64) string {
	if at == nil || *at == 0 {
		return "—"
	}

	d := time.Since(time.Unix(*at, 0))

	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// joinOrDash renders a list, or a dash when it is empty.
func joinOrDash(values []string) string {
	if len(values) == 0 {
		return "—"
	}

	out := values[0]
	for _, v := range values[1:] {
		out += ", " + v
	}

	return out
}
