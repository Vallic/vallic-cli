package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/vallic/vallic-cli/internal/api"
	"github.com/vallic/vallic-cli/internal/output"
)

func envCreateCommand() *Command {
	var (
		kind       string
		autoDeploy bool
	)

	return &Command{
		Name:    "create",
		Summary: "raise a new environment from a branch",
		Usage:   "env create <branch> [--type staging|development] [--auto-deploy]",
		Long: `Creates the environment. It does not build it.

The new environment comes back as a draft: it exists, it has no machines, and
it will refuse a deploy until somebody builds it. That separation is
deliberate — building machines spends money, and one command should not do it
as a side effect of another.

The name is derived from the type and the branch, and the slug from the name.
Neither can be given, and neither can be changed afterwards: the slug is the
compose project, the systemd instance, the site directory, the backup prefix
and a DNS label, all at once.

The branch does not have to exist yet. Nothing here checks the repository, so
raising the environment before pushing the branch is a reasonable order to
work in, and it is the order auto-deploy assumes.

--type is worked out from what the project already has, because the platform
allows exactly one staging environment and refuses a development one before
there is a staging to merge towards. So the first environment of a project is
staging and every one after it is development, which is what --type would
have had to say anyway. Naming it yourself still works.

Production cannot be created. A project has one, and it is created with the
project.`,
		Flags: func(fs *flag.FlagSet) {
			// No default. A project may have exactly one staging environment
			// and cannot have a development one before it, so the right
			// answer depends on what the project already has, and a fixed
			// default is wrong for every project past its first environment.
			fs.StringVar(&kind, "type", "", "staging or development (default: whichever the project can take)")
			fs.BoolVar(&autoDeploy, "auto-deploy", false, "deploy on every push to the branch")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			if len(args) == 0 {
				return Usagef(nil, "name a branch")
			}

			branch := args[0]

			if err := ensureNoExtra(nil, args, 1); err != nil {
				return err
			}

			switch kind {
			case "staging", "development", "":
			case "production":
				return Usagef(nil, "a project has one production environment, and it is created with the project")
			default:
				return Usagef(nil, "--type takes staging or development, not %q", kind)
			}

			project, err := env.ResolveProject(ctx)
			if err != nil {
				return err
			}

			client, err := env.Client()
			if err != nil {
				return err
			}

			chosen := kind

			if chosen == "" {
				chosen, err = typeForProject(ctx, client, project.ID)
				if err != nil {
					return err
				}
			}

			created, err := client.CreateEnvironment(ctx, project.ID, api.EnvironmentCreate{
				GitRef:     branch,
				Type:       chosen,
				AutoDeploy: autoDeploy,
			})
			if err != nil {
				return describeCreateFailure(err)
			}

			if env.Printer.Structured() {
				return env.Printer.Value(map[string]any{"environment": created})
			}

			env.Printer.Good("%s created on %s, tracking %s", created.Name, project.MachineName, created.GitRef)

			// Which kind, where nobody said. A command that silently picked
			// between two things that are priced and shaped differently
			// would be a command somebody has to check afterwards.
			if kind == "" {
				env.Printer.Say("  a %s environment, because %s", chosen, whyThatType(chosen))
			}

			// Said here rather than left to be met as a refusal. A draft
			// environment answers `not_deployable`, and somebody who has just
			// created one and been told it was created will read that as a bug.
			env.Printer.Say("  it has no machines yet, so a deploy will be refused until it is built")
			env.Printer.Say("  build it from the console: %s", created.Slug)

			// The address, here, because this is where somebody copies it
			// from. It is a real hostname already: the platform one is issued
			// with the environment, not with the first deploy. Future tense
			// on purpose, since nothing answers on it until it is built.
			if address := createdAddress(created); address != "" {
				env.Printer.Say("  the address it will answer on: %s", address)
			}

			if autoDeploy {
				env.Printer.Say("  once built, every push to %s will deploy", created.GitRef)
			}

			return nil
		},
	}
}

func envDeleteCommand() *Command {
	var yes bool

	return &Command{
		Name:    "delete",
		Summary: "remove an environment and its machines",
		Usage:   "env delete <env> [--yes]",
		Long: `Destroys the machines this environment alone runs on, removes its
hostnames from DNS, and deletes its environment-scoped variables. Project-scoped
variables are untouched.

**Its backups go with it, and cannot be recovered.** The key they were
encrypted with is on the record being deleted, so there is nothing left to
decrypt them with afterwards — not by you, and not by anybody here.

The subscription does not change. The slot stays bought and can be filled
again; reducing what you pay for is a separate decision.

Production cannot be deleted, at any role. Archive the project instead, which
destroys the machines and keeps the backups.`,
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&yes, "yes", false, "do not ask for confirmation")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			if len(args) == 0 {
				// Required rather than resolved from the branch. Every other
				// command defaulting to the current checkout is a convenience;
				// here it would delete an environment nobody named.
				return Usagef(nil, "name the environment to delete, in full")
			}

			target, err := env.ResolveEnvironment(ctx, args[0])
			if err != nil {
				return err
			}

			if err := ensureNoExtra(nil, args, 1); err != nil {
				return err
			}

			client, err := env.Client()
			if err != nil {
				return err
			}

			if !yes {
				if !env.Interactive() {
					// No prompt available, and this is the one command where
					// proceeding without one is not a reasonable default.
					return fmt.Errorf(
						"deleting %s destroys its machines and its backups, which cannot be recovered\n"+
							"  there is no terminal to confirm on, so pass --yes if you mean it",
						target.Environment.Name,
					)
				}

				// The backup sentence, every time, because the API cannot say
				// it and the console does. A CLI that deleted without printing
				// it would be strictly worse than the console.
				env.Printer.Warn("Deleting %s destroys its machines and its backups.", target.Environment.Name)
				env.Printer.Say("  the backups cannot be recovered: their key is on the record being deleted")
				env.Printer.Say("  the subscription does not change; the slot stays bought")

				if !env.confirm(fmt.Sprintf("Type-check: delete %s?", target.Environment.Name)) {
					return fmt.Errorf("cancelled")
				}
			}

			deleted, err := client.DeleteEnvironment(ctx, target.Environment.ID)
			if err != nil {
				return describeDeleteFailure(err, target)
			}

			if env.Printer.Structured() {
				return env.Printer.Value(deleted)
			}

			env.Printer.Good("%s deleted", deleted.Environment.Slug)

			return nil
		},
	}
}

func envSourceCommand() *Command {
	var (
		branch     string
		autoDeploy string
	)

	return &Command{
		Name:    "source",
		Summary: "change the branch an environment builds from",
		Usage:   "env source [<env>] [--branch <b>] [--auto-deploy true|false]",
		Long: `Repoints the environment, or turns deploy-on-push on and off.

Switching the branch does not move the running release. The next push, or the
next deploy you ask for, builds from the new branch.

With no flags it prints what the environment builds from now.

On a protected environment this needs an owner, while deploying to the same
environment needs only a developer. That asymmetry is intended: changing the
branch changes what the next deploy serves, which outlasts the deploy.`,
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&branch, "branch", "", "the branch to build from")
			fs.StringVar(&autoDeploy, "auto-deploy", "", "true or false")
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

			// Nothing asked for: report rather than send an empty patch, which
			// the control plane refuses. "Show me" is a reasonable thing to
			// mean by the command with no arguments.
			if branch == "" && autoDeploy == "" {
				detail, detailErr := env.Detail(ctx, target)
				if detailErr != nil {
					return detailErr
				}

				return env.Printer.Print(output.Table{
					Columns: []string{"field", "value"},
					Rows: [][]string{
						{"environment", detail.Name},
						{"branch", detail.GitRef},
						{"deploy on push", yesNo(detail.AutoDeploy)},
					},
				}, map[string]any{"environment": detail})
			}

			patch := api.EnvironmentPatch{}

			if branch != "" {
				patch.GitRef = &branch
			}

			if autoDeploy != "" {
				parsed, parseErr := parseBool(autoDeploy)
				if parseErr != nil {
					return parseErr
				}

				patch.AutoDeploy = &parsed
			}

			updated, err := client.PatchEnvironment(ctx, target.Environment.ID, patch)
			if err != nil {
				if api.Code(err) == "branch_taken" {
					return fmt.Errorf("%w\n  `vallic env list` shows which environment has it", err)
				}

				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(map[string]any{"environment": updated})
			}

			if patch.GitRef != nil {
				env.Printer.Good("%s now builds from %s", updated.Name, updated.GitRef)
				// The running release did not move. Somebody who repointed a
				// branch and reloaded the site would otherwise conclude it had.
				env.Printer.Say("  the next push, or the next deploy you ask for, builds from it")
			}

			if patch.AutoDeploy != nil {
				if updated.AutoDeploy {
					env.Printer.Good("every push to %s will deploy %s", updated.GitRef, updated.Name)
				} else {
					env.Printer.Good("%s no longer deploys on a push", updated.Name)
				}
			}

			return nil
		},
	}
}

// createdAddress is the address a new environment will answer on, or empty
// against a control plane that predates the hostnames block.
//
// Empty rather than assembled from the slug: the platform domain is
// configuration this binary does not hold, so a guess would be a confident
// wrong address. `vallic url` refuses outright for that reason; this command
// cannot, because the environment was created and saying so is the point.
func createdAddress(created *api.EnvironmentDetail) string {
	if created.Hostnames == nil {
		return ""
	}

	return created.Hostnames.URL
}

// parseBool reads a flag that has to be able to say false.
//
// A string rather than a bool flag, because `--auto-deploy` as a bool cannot
// express "turn it off": its absence and its false are the same thing, and a
// patch has to tell "leave this alone" from "set it to false".
func parseBool(value string) (bool, error) {
	switch strings.ToLower(value) {
	case "true", "yes", "on", "1":
		return true, nil
	case "false", "no", "off", "0":
		return false, nil
	default:
		return false, Usagef(nil, "--auto-deploy takes true or false, not %q", value)
	}
}

// describeCreateFailure adds the one thing the message cannot carry.
func describeCreateFailure(err error) error {
	switch api.Code(err) {
	case "shape_refused":
		// The shape a project may take, which costs nothing to fix: one
		// staging per project, and staging before development. Worth its own
		// line because the answer is a different kind of environment rather
		// than a purchase, and for a while this shared a code with the plan
		// ceiling and could only be printed bare.
		return fmt.Errorf("%w\n  %s", err, shapeAdvice(err))
	case "plan_limit":
		// The ceiling, which is fixed by buying something. The message names
		// the plan and where to change it, so there is nothing useful to add
		// that would not be guessing at a price.
		return err
	case "quota_exceeded":
		// Safe to gloss: this one means exactly one thing, and the contrast
		// with the plan is what somebody needs to know to go to the right
		// page.
		return fmt.Errorf("%w\n  this is the account's quota, not the project's plan", err)
	case "branch_taken":
		return fmt.Errorf("%w\n  `vallic env list` shows which environment has it", err)
	case "project_incomplete":
		return fmt.Errorf("%w\n  an environment on an unfinished project would fail on the first screen that needs a shape", err)
	default:
		return err
	}
}

// describeDeleteFailure explains the production refusal, which is absolute.
func describeDeleteFailure(err error, target *Target) error {
	if api.Code(err) == "forbidden" && target.Environment.Type == "production" {
		return fmt.Errorf(
			"%w\n  production is never deleted, at any role\n"+
				"  archiving the project destroys the machines and keeps the backups",
			err,
		)
	}

	return err
}

// typeForProject picks the kind of environment a project can actually take.
//
// The platform allows exactly one staging environment and refuses a
// development one before it exists, so which kind is wanted follows from what
// is already there rather than from a preference. `--type staging` used to be
// the default, which is right for the first environment of a project and
// wrong for every one after: `vallic env create feature/checkout` answered
// "already has a staging environment", a sentence about staging in reply to a
// question about a branch.
//
// One extra request, on a command that creates something. Worth it to avoid
// making somebody learn a rule the platform is perfectly able to apply.
func typeForProject(ctx context.Context, client *api.Client, projectID int) (string, error) {
	environments, err := client.Environments(ctx)
	if err != nil {
		return "", err
	}

	for _, environment := range environments {
		if environment.Project == projectID && environment.Type == "staging" {
			return "development", nil
		}
	}

	return "staging", nil
}

// whyThatType explains a choice the caller did not make.
func whyThatType(chosen string) string {
	if chosen == "staging" {
		return "this project has none yet, and staging comes before development"
	}

	return "this project already has its staging environment"
}

// shapeAdvice says which way out of a shape refusal applies.
//
// Read off the message, which is the one place the two are distinguishable
// and is exactly what a stable code is supposed to spare a client. Tolerated
// here and nowhere else: this is a hint appended to the control plane's own
// sentence, so a wrong guess costs a line of advice rather than a wrong
// action, and the fallback says the true thing for both.
func shapeAdvice(err error) string {
	if strings.Contains(err.Error(), "no staging environment yet") {
		return "create staging first: vallic env create <branch> --type staging"
	}

	if strings.Contains(err.Error(), "already has a staging") {
		return "a project has one staging; for another environment: vallic env create <branch> --type development"
	}

	return "`vallic env list` shows what this project already has"
}
