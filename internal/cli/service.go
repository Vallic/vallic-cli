package cli

import (
	"context"
	"flag"

	"github.com/vallic/vallic-cli/internal/api"
	"github.com/vallic/vallic-cli/internal/output"
)

// serviceCommand reads the containers an environment runs.
func serviceCommand() *Command {
	return &Command{
		Name:    "service",
		Summary: "list the services an environment runs",
		Children: []*Command{
			serviceListCommand(),
		},
	}
}

func serviceListCommand() *Command {
	var versions bool

	return &Command{
		Name:    "list",
		Summary: "list the stack, in the order it is built outwards",
		Usage:   "service list [<env>]",
		Long: `The services, in weight order, which runs from the application
outwards rather than alphabetically.

The VERSION column is what goes in ` + "`vallic.yaml`" + `. It is deliberately not
the whole image tag: the rest of the tag is the platform's own image build,
which is rebuilt whenever a security fix lands, and a manifest that pinned it
would break on the next rebuild. --images shows the full references instead.

This answers before an environment has been built. A draft environment lists
the same stack it will run once it is running, because the answer comes from
the project and the manifest rather than from a machine.

The two services named under the table are not the same service, and on a PHP
stack they never are. Commands and shells run in the application container,
which is where ` + "`drush`" + ` and the deploy steps belong; traffic arrives at the
entrypoint, which is the nginx in front of it. A Node or Go application is
both, because it serves its own HTTP.`,
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&versions, "images", false, "show full image references rather than versions")
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

			stack, err := client.Services(ctx, target.Environment.ID)
			if err != nil {
				return err
			}

			last := "version"
			if versions {
				last = "image"
			}

			table := output.Table{
				Columns: []string{"service", "group", "runs on", "from", last},
				Empty:   "This environment runs nothing, which should not be possible.",
			}

			for i := range stack.Services {
				service := &stack.Services[i]

				version := service.Version
				if versions {
					version = service.Image
				}

				table.Rows = append(table.Rows, []string{
					service.ID,
					service.Group,
					runsOn(service),
					service.Source,
					version,
				})
			}

			if env.Printer.Structured() {
				return env.Printer.Value(map[string]any{"stack": stack})
			}

			if err := env.Printer.Print(table, nil); err != nil {
				return err
			}

			// The runtime the stack was resolved for, which no row carries:
			// a PHP environment with a Node build step lists the same two
			// services as a Node environment, and only this says which of
			// them is the application.
			env.Printer.Say("Runtime: %s", stack.Runtime)

			// Both, separately, even where they name the same service. One
			// line for "the main service" would be right for Node, which
			// serves its own HTTP, and wrong for PHP, where a shell opens in
			// php-fpm and traffic arrives at the nginx in front of it.
			// Collapsing the two sends somebody to run a deploy step in the
			// web server, which is the confusion the control plane sends two
			// fields to prevent.
			env.Printer.Say("Commands and shells run in: %s", serviceOrNone(stack.Application))
			env.Printer.Say("Traffic arrives at: %s", serviceOrNone(stack.Entrypoint))

			// Both said afterwards rather than columned: they are answers to
			// questions somebody has about the list they just read, not
			// properties of a row.
			if len(stack.Added) > 0 {
				env.Printer.Say("Pulled in as dependencies:")

				// One gloss each, because this is the list nobody wrote and
				// "zookeeper" is not a word that explains itself. The rows
				// the repository did ask for are left plain: they were named
				// by the person reading them.
				for _, id := range stack.Added {
					env.Printer.Say("  %s", serviceGloss(stack, id))
				}
			}

			if len(stack.Rejected) > 0 {
				// A real problem, not a note. The environment is still asking
				// for something the platform no longer offers, and the next
				// deploy will not give it to them.
				env.Printer.Warn("No longer offered, and still asked for: %s", joinOrDash(stack.Rejected))
			}

			return nil
		},
	}
}

// serviceOrNone names a service the control plane singled out.
//
// Not a dash. These two lines answer "where does a shell open" and "where
// does traffic land", and a dash in an answer reads as a figure the CLI could
// not fetch rather than as a stack that genuinely has nowhere to open one.
func serviceOrNone(id *string) string {
	if id == nil || *id == "" {
		return "nothing in this stack"
	}

	return *id
}

// serviceGloss names a service the way somebody who never asked for it needs
// it named.
//
// Under the table rather than in a column: a description is two sentences of
// prose and would wrap every row it was put beside. Glossing every row here
// instead was the other shape, and on a stack where the repository declared
// nothing it buries the list under a page of prose nobody ran the command to
// read.
func serviceGloss(stack *api.Stack, id string) string {
	for i := range stack.Services {
		service := &stack.Services[i]

		if service.ID != id {
			continue
		}

		gloss := service.Label

		if service.Description != "" {
			if gloss != "" {
				gloss += ". "
			}

			gloss += service.Description
		}

		if gloss != "" {
			return id + ": " + gloss
		}
	}

	// A name with no row behind it, which the control plane does not send
	// today. The id on its own rather than nothing: what was pulled in is
	// worth saying even where there is no gloss for it.
	return id
}

// runsOn names the machine role a service runs on.
func runsOn(service *api.Service) string {
	if service.BuildOnly {
		// Not blank. A build-only service has no tier because it runs during
		// a build and then stops, which is a different thing from a tier
		// nobody worked out.
		return "build"
	}

	if service.Tier == nil {
		return "—"
	}

	return *service.Tier
}
