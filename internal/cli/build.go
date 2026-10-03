package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/vallic/vallic-cli/internal/api"
)

// buildCommand builds what an environment's branch points at now.
//
// The gap this fills is the one a pipeline hits first. A build otherwise comes
// from a push, so "deploy the current branch" meant making a commit nobody
// wanted, and `vallic deploy` could only ever ship a build that already
// existed -- which is why it answers `no_release` when run seconds after a
// push, before the build it is waiting for has finished.
//
// The commit is resolved by the control plane, not here. A release names one
// commit, which is what makes "deploy release 47" mean a set of bytes rather
// than a moment in time; resolving the branch on the builder would record a sha
// nobody chose, minutes after the person who asked looked at what was on it.
func buildCommand() *Command {
	var (
		deploy    bool
		skipSteps bool
	)

	return &Command{
		Name:    "build",
		Summary: "build what an environment's branch points at now",
		Usage:   "build [<env>] [--deploy [--skip-steps]]",
		Long: `Builds the current commit of the branch this environment tracks, and
returns the release it opened. It does not wait: a build is minutes of a build
machine, so follow it with ` + "`vallic activity list`" + ` or watch for the
release in ` + "`vallic release list`" + `.

Building and deploying are separate on purpose. A build produces a release;
deploying one is a second decision, and keeping them apart is what lets you
build a branch, look at what came out, and ship it -- or not.

--deploy asks for the result to be deployed here when the build finishes, which
is the shape a pipeline wants: one command, one outcome.

It is not a promise that nothing is deployed without it. An environment set to
deploy this branch automatically deploys the build either way, because that is
what auto-deploy means, and this command is not a way around a rule the project
set. The output says which it is going to be.`,
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&deploy, "deploy", false, "deploy the result here when the build finishes")
			fs.BoolVar(&skipSteps, "skip-steps", false, "with --deploy: go live without running the deploy steps")
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

			// Only where this is going to ship something. A build that deploys
			// nothing touches nothing on the live site, and an extra request
			// per build to ask a question that cannot apply is a slower command
			// for no gain.
			if deploy {
				detail, err := env.Detail(ctx, target)
				if err != nil {
					return err
				}

				// The same guard `vallic deploy` makes, and it belongs here for
				// the same reason: protection guards reshaping an environment
				// rather than operating one, so a developer shipping production
				// is doing their job — but a command run from the wrong
				// checkout should be recoverable before it happens rather than
				// after. `build --deploy` skipped it until [2026-09-30], which
				// made the flag a way round a question the other path asks.
				if asksBeforeShipping(deploy, detail.Protected, env.Interactive()) {
					if !env.confirm(fmt.Sprintf("Build and deploy to %s, which is protected?", detail.Name)) {
						return fmt.Errorf("cancelled")
					}
				}
			}

			built, err := client.Build(ctx, target.Environment.ID, deploy, skipSteps)
			if err != nil {
				return describeBuildFailure(err)
			}

			if env.Printer.Structured() {
				return env.Printer.Value(map[string]any{"build": built})
			}

			env.Printer.Good("%s", buildStarted(built))

			for _, line := range buildLines(built) {
				env.Printer.Say("%s", line)
			}

			return nil
		},
	}
}

// buildStarted is the one sentence worth reading.
//
// Named by its number rather than its id, because the number is what
// `--release` takes. Where the control plane could not read one back the id is
// printed and said to be one, rather than passed off as something typeable.
func buildStarted(built *api.Build) string {
	if built.ReleaseNumber != nil {
		return fmt.Sprintf("Building %s as release %d.", built.GitRef, *built.ReleaseNumber)
	}

	return fmt.Sprintf("Building %s (release id %d).", built.GitRef, built.Release)
}

// buildLines says what happens when it finishes, and how to watch.
func buildLines(built *api.Build) []string {
	lines := make([]string, 0, 3)

	switch {
	case built.DeployRequested:
		lines = append(lines, fmt.Sprintf("  it will deploy to %s when the build finishes", built.Environment))
	case built.WillDeploy:
		// Asked for nothing and getting a deployment anyway. The one case that
		// has to be said out loud: somebody who ran this to look before
		// shipping is about to ship.
		lines = append(lines, fmt.Sprintf(
			"  %s deploys this branch automatically, so it will deploy there when the build finishes",
			built.Environment,
		))
	default:
		lines = append(lines,
			"  nothing is deployed; it becomes a release you can deploy",
			fmt.Sprintf("  ship it with: vallic deploy --environment %s", built.Environment),
		)
	}

	return append(lines, "  watch it with: vallic activity list")
}

// describeBuildFailure adds the sentence that makes a refusal actionable.
//
// The control plane's own message is kept in every case: each of its refusals
// names something specific -- a branch that is not there, a repository host
// that is down, a project whose build machine is still being provisioned -- and
// a client that replaced those with one generic failure would be throwing away
// the only useful half.
func describeBuildFailure(err error) error {
	if api.Code(err) == api.CodeNotBuildable {
		return fmt.Errorf("%w\n  `vallic env info` shows which branch this environment tracks", err)
	}

	return err
}

// asksBeforeShipping reports whether this build has to be confirmed first.
//
// All three, and each for its own reason. Only a build that deploys can reach a
// live site, so one that produces a release and stops needs no question however
// protected the environment is. Only a protected environment asks at all --
// protection guards reshaping an environment rather than operating one, and a
// prompt on every staging deploy is a prompt nobody reads. And only at a
// terminal, because a pipeline has nobody to answer and a command that blocked
// there would hang a build rather than refuse it.
func asksBeforeShipping(deploy, protected, interactive bool) bool {
	return deploy && protected && interactive
}
