package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/vallic/vallic-cli/internal/api"
	"github.com/vallic/vallic-cli/internal/config"
	"github.com/vallic/vallic-cli/internal/resolve"
)

// linkCommand records which project a checkout is.
//
// For the one case internal/resolve cannot answer on its own: a checkout
// whose remote matches no project this credential can see, where the
// resolution stops and lists the candidates rather than picking one. A fork,
// a mirror, a repository added under a different host, or no git at all.
//
// What it records is not a property of the checkout, and that is the whole
// caveat. It goes in the configuration this machine shares between every
// directory, so the resolver reads it only *after* the git remote: a
// checkout that names its own project goes on resolving to it. Read first,
// a project linked once answered for every checkout on the machine, and a
// command run in one project's checkout acted on another project.
func linkCommand() *Command {
	var forget bool

	return &Command{
		Name:    "link",
		Summary: "record which project this checkout is",
		Usage:   "link [<project>] [--clear]",
		Long: `Most checkouts need this and never run it. A project carries its
repository's full name, so the git remote is matched against it and the
project is worked out with nothing recorded anywhere.

This is for the checkouts where that fails, which is what "cannot tell which
project you mean" is saying: a fork, a mirror, a repository the project was
added under another name, or a directory that is not a checkout at all.

With no argument it says what is recorded now, or what commands resolve to
here without it.

What it records is machine-wide, not per checkout, so it answers only where
a checkout cannot: a checkout whose git remote names a project you can see
goes on resolving to that project. --clear forgets it.

It records no environment, and that is deliberate. The environment comes from
the branch, which changes while you work. An environment recorded once would
answer for every branch after it, which is the case resolution refuses on
purpose:
a branch matching no environment is told so, rather than deploying to whatever
was recorded. Name one for the command that needs it, with --environment or
VALLIC_ENVIRONMENT.`,
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&forget, "clear", false, "forget the recorded project")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			if err := ensureNoExtra(nil, args, 1); err != nil {
				return err
			}

			cfg, err := env.Config()
			if err != nil {
				return err
			}

			if forget {
				if len(args) > 0 {
					return Usagef(nil, "--clear forgets the recorded project, so it takes none")
				}

				return clearLink(env, cfg)
			}

			// The API, for both halves: a name has to be checked against
			// something, and a name that matches nothing is only useful if the
			// answer lists what would have worked.
			client, err := env.Client()
			if err != nil {
				return err
			}

			projects, err := client.Projects(ctx)
			if err != nil {
				return err
			}

			if len(args) == 0 {
				return reportLink(ctx, env, cfg, projects)
			}

			chosen, err := chooseProject(projects, args[0])
			if err != nil {
				return err
			}

			cfg.Project = chosen.MachineName

			if err := cfg.Save(); err != nil {
				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(map[string]any{"project": chosen})
			}

			env.Printer.Good("Commands act on %s where a checkout cannot say.", chosen.MachineName)

			if dir, dirErr := config.Dir(); dirErr == nil {
				env.Printer.Say("  recorded in %s, for the checkouts on this machine that cannot say", dir)
			}

			warnOverridden(ctx, env, projects, chosen)

			return nil
		},
	}
}

// reportLink answers `vallic link` with nothing after it.
//
// Somebody who types that is asking which project they are on, and the two
// answers are not the same thing: one is recorded and outlives this
// directory, the other is worked out here and now. So the name goes to stdout
// for a `$(…)` to capture, and which of the two it is goes to stderr.
func reportLink(ctx context.Context, env *Env, cfg *config.Config, projects []api.Project) error {
	recorded := cfg.Project
	inferred := inferProject(ctx, projects)

	if env.Printer.Structured() {
		name := ""
		if inferred != nil {
			name = inferred.MachineName
		}

		return env.Printer.Value(map[string]any{
			"recorded": nullableString(recorded),
			"resolved": nullableString(name),
		})
	}

	// VALLIC_PROJECT, which every command reads before the checkout and the
	// file. Worth separating, because --clear removes the file's and not it.
	if fromEnv := os.Getenv("VALLIC_PROJECT"); fromEnv != "" {
		env.Printer.Line("%s", fromEnv)
		env.Printer.Say("  from VALLIC_PROJECT, which every command reads first")

		return nil
	}

	// The checkout, which answers ahead of anything recorded.
	if inferred != nil {
		env.Printer.Line("%s", inferred.MachineName)
		env.Printer.Say("  worked out from this checkout")

		if recorded != "" && recorded != inferred.MachineName {
			env.Printer.Say("  %s is recorded too, and answers only where a checkout cannot say", recorded)
		}

		return nil
	}

	if recorded == "" {
		if len(projects) == 0 {
			return fmt.Errorf("this credential can see no projects, so there is nothing to link to")
		}

		// The state this command exists for. Named as usage, because the
		// fix is an argument and the candidates are the useful half of the
		// answer.
		return Usagef(nil,
			"nothing is recorded, and this checkout does not say which project it is\n"+
				"  record one: vallic link <project>\n  it can be: %s",
			strings.Join(projectNames(projects), ", "),
		)
	}

	env.Printer.Line("%s", recorded)

	if dir, err := config.Dir(); err == nil {
		env.Printer.Say("  recorded in %s, for the checkouts on this machine that cannot say", dir)
	}

	return nil
}

// clearLink forgets the recorded project.
func clearLink(env *Env, cfg *config.Config) error {
	was := cfg.Project
	fromEnv := os.Getenv("VALLIC_PROJECT")

	// Only where there is something to remove. Saving regardless would create
	// a configuration file for somebody who has never had one, on a command
	// whose whole job was to leave nothing behind.
	if was != "" {
		cfg.Project = ""

		if err := cfg.Save(); err != nil {
			return err
		}
	}

	if env.Printer.Structured() {
		return env.Printer.Value(map[string]any{"project": nil})
	}

	switch {
	case was == "":
		env.Printer.Say("Nothing was recorded.")
	case was == fromEnv:
		// The value came from the environment, so claiming to have forgotten
		// it would be a report of something that has not happened: the next
		// command reads the same variable again.
		env.Printer.Good("The configuration file records no project now.")
	default:
		env.Printer.Good("Forgot %s. A checkout that cannot say which project it is will be asked.", was)
	}

	if fromEnv != "" {
		env.Printer.Warn("VALLIC_PROJECT is set to %s, and every command reads it first", fromEnv)
	}

	return nil
}

// warnOverridden says where the link will not answer.
//
// A link is silent by nature: it changes what later commands resolve to and
// prints nothing at the time. Somebody who has just linked a project here
// while this checkout names another expects the link to answer for it, and
// it does not: the checkout comes first.
func warnOverridden(ctx context.Context, env *Env, projects []api.Project, chosen *api.Project) {
	if fromEnv := os.Getenv("VALLIC_PROJECT"); fromEnv != "" && fromEnv != chosen.MachineName {
		env.Printer.Warn("VALLIC_PROJECT is set to %s, every command reads it first, and it has not changed", fromEnv)
	}

	inferred := inferProject(ctx, projects)

	if inferred != nil && inferred.ID != chosen.ID {
		env.Printer.Warn("this checkout goes on resolving to %s, which its git remote names; the link answers only where a checkout cannot say", inferred.MachineName)
	}
}

// inferProject asks what the checkout says on its own.
//
// The resolver is built here rather than through env.Resolver, which layers
// the flags and the config file over it. Telling those two apart is this
// command's whole job, and a hint would answer with what was recorded a
// moment ago.
//
// A failure is not one: it means the checkout cannot say, which is the state
// this command is for.
func inferProject(ctx context.Context, projects []api.Project) *api.Project {
	dir, err := os.Getwd()
	if err != nil {
		return nil
	}

	project, err := (&resolve.Resolver{Client: known(projects), Dir: dir}).Project(ctx)
	if err != nil {
		return nil
	}

	return project
}

// known answers a resolver out of the list this command already read, so
// asking what the checkout says on its own costs no second request.
type known []api.Project

func (k known) Projects(context.Context) ([]api.Project, error) {
	return k, nil
}

// Never called. Resolver.Project settles a project without looking at any
// environment, and this command stops there.
func (k known) Environments(context.Context) ([]api.Environment, error) {
	return nil, nil
}

// chooseProject finds the project somebody named.
//
// Machine name first and label second, the two things `vallic project list`
// prints, in that order so an exact machine name is never lost to another
// project's label.
func chooseProject(projects []api.Project, want string) (*api.Project, error) {
	for i := range projects {
		if strings.EqualFold(projects[i].MachineName, want) {
			return &projects[i], nil
		}
	}

	for i := range projects {
		if strings.EqualFold(projects[i].Label, want) {
			return &projects[i], nil
		}
	}

	if len(projects) == 0 {
		return nil, fmt.Errorf("this credential can see no projects, so there is nothing to link to")
	}

	return nil, Usagef(nil,
		"this credential can see no project called %q\n  it can see: %s",
		want, strings.Join(projectNames(projects), ", "),
	)
}

// projectNames is the machine names, which are what this command takes.
func projectNames(projects []api.Project) []string {
	names := make([]string, 0, len(projects))

	for _, project := range projects {
		names = append(names, project.MachineName)
	}

	return names
}
