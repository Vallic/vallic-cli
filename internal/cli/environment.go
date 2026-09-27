package cli

import (
	"context"
	"flag"
	"fmt"
	"sort"

	"github.com/vallic/vallic-cli/internal/output"
)

// projectCommand groups what can be asked about projects.
func projectCommand() *Command {
	return &Command{
		Name:    "project",
		Summary: "list and inspect projects",
		Children: []*Command{
			{
				Name:    "list",
				Summary: "list the projects this credential can see",
				Usage:   "project list",
				Run: func(ctx context.Context, env *Env, args []string) error {
					client, err := env.Client()
					if err != nil {
						return err
					}

					projects, err := client.Projects(ctx)
					if err != nil {
						return err
					}

					table := output.Table{
						Columns: []string{"name", "label", "branch", "repository"},
						Empty:   "No projects.",
					}

					for _, p := range projects {
						repo := "—"
						if p.Repository != nil {
							repo = p.Repository.FullName
						}

						table.Rows = append(table.Rows, []string{
							p.MachineName,
							p.Label,
							p.DefaultBranch,
							repo,
						})
					}

					return env.Printer.Print(table, map[string]any{"projects": projects})
				},
			},
			{
				Name:    "info",
				Summary: "show one project",
				Usage:   "project info [--project <name>]",
				Run: func(ctx context.Context, env *Env, args []string) error {
					project, err := env.ResolveProject(ctx)
					if err != nil {
						return err
					}

					if env.Printer.Structured() {
						return env.Printer.Value(map[string]any{"project": project})
					}

					env.Printer.Line("%s", project.MachineName)
					env.Printer.Say("  label:    %s", project.Label)
					env.Printer.Say("  id:       %d", project.ID)
					env.Printer.Say("  branch:   %s", project.DefaultBranch)

					if project.Repository != nil {
						env.Printer.Say("  repo:     %s (%s)", project.Repository.FullName, project.Repository.Provider)
					}

					return nil
				},
			},
		},
	}
}

// envCommand groups what can be asked about environments.
func envCommand() *Command {
	return &Command{
		Name:    "env",
		Aliases: []string{"environment"},
		Summary: "list and inspect environments",
		Children: []*Command{
			envListCommand(),
			envInfoCommand(),
			envCreateCommand(),
			envSourceCommand(),
			envDeleteCommand(),
		},
	}
}

func envListCommand() *Command {
	var allProjects bool

	return &Command{
		Name:    "list",
		Summary: "list environments",
		Usage:   "env list [--all]",
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&allProjects, "all", false, "every project, not just this checkout's")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}

			environments, err := client.Environments(ctx)
			if err != nil {
				return err
			}

			// Narrowed to the checkout's project unless asked otherwise. A
			// bare `env list` inside a repository means "this project's",
			// which is what somebody standing in it is asking.
			if !allProjects {
				project, projectErr := env.ResolveProject(ctx)
				if projectErr == nil {
					filtered := environments[:0:0]
					for _, e := range environments {
						if e.Project == project.ID {
							filtered = append(filtered, e)
						}
					}
					environments = filtered
				}
				// An unresolvable project is not an error here: listing every
				// environment is a reasonable answer to `env list` run
				// outside a checkout, and refusing would be worse than
				// showing more than was asked for.
			}

			sort.Slice(environments, func(i, j int) bool {
				return environments[i].Slug < environments[j].Slug
			})

			table := output.Table{
				Columns: []string{"name", "type", "state", "branch", "slug"},
				Empty:   "No environments.",
			}

			for _, e := range environments {
				table.Rows = append(table.Rows, []string{
					e.Name,
					e.Type,
					e.State,
					e.GitRef,
					e.Slug,
				})
			}

			return env.Printer.Print(table, map[string]any{"environments": environments})
		},
	}
}

func envInfoCommand() *Command {
	return &Command{
		Name:    "info",
		Summary: "show one environment in full",
		Usage:   "env info [<env>]",
		Long: `Includes the SSH target: the host, the port and the account to
connect as.

Every one of those is the control plane's to say. The port especially — it is
chosen at random per project in the range 2000-2999 when the project is
created, so there is no default to fall back on and nothing to guess.`,
		Run: func(ctx context.Context, env *Env, args []string) error {
			target, err := env.ResolveEnvironment(ctx, first(args))
			if err != nil {
				return err
			}

			detail, err := env.Detail(ctx, target)
			if err != nil {
				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(map[string]any{"environment": detail})
			}

			env.Printer.Line("%s", detail.Slug)
			env.Printer.Say("  project:   %s", target.Project.MachineName)
			env.Printer.Say("  name:      %s", detail.Name)
			env.Printer.Say("  type:      %s", detail.Type)
			env.Printer.Say("  state:     %s", detail.State)
			env.Printer.Say("  branch:    %s", detail.GitRef)
			env.Printer.Say("  protected: %s", yesNo(detail.Protected))
			env.Printer.Say("  auto-deploy: %s", yesNo(detail.AutoDeploy))

			if detail.CurrentRelease != nil {
				env.Printer.Say("  release:   #%d (%s)", detail.CurrentRelease.Number, shortSHA(detail.CurrentRelease.GitSHA))
			} else {
				env.Printer.Say("  release:   nothing deployed")
			}

			if detail.SSH == nil {
				env.Printer.Say("  shell:     not available to you, or not built yet")

				return nil
			}

			env.Printer.Say("  shell:     ssh -p %d %s@%s", detail.SSH.Port, detail.SSH.User, detail.SSH.Host)
			env.Printer.Say("  files:     %s", detail.SSH.FilesPath)

			if !detail.SSH.HasKeys {
				env.Printer.Warn("you have no SSH key on file, so that connection will be refused")
				cfg, _ := env.Config()
				env.Printer.Say("  add one at %s", keysPage(cfg.API))
			}

			return nil
		},
	}
}

// statusCommand is the current project and environment at a glance.
func statusCommand() *Command {
	return &Command{
		Name:    "status",
		Summary: "show what this checkout points at",
		Usage:   "status [<env>]",
		Run: func(ctx context.Context, env *Env, args []string) error {
			target, err := env.ResolveEnvironment(ctx, first(args))
			if err != nil {
				return err
			}

			detail, err := env.Detail(ctx, target)
			if err != nil {
				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(map[string]any{
					"project":     target.Project,
					"environment": detail,
				})
			}

			release := "nothing deployed"
			if detail.CurrentRelease != nil {
				release = fmt.Sprintf("release #%d, %s", detail.CurrentRelease.Number, shortSHA(detail.CurrentRelease.GitSHA))
			}

			env.Printer.Line("%s → %s", target.Project.MachineName, detail.Name)
			env.Printer.Say("  %s, tracking %s", detail.State, detail.GitRef)
			env.Printer.Say("  %s", release)

			if detail.Protected {
				env.Printer.Say("  protected — reshaping it needs an owner")
			}

			return nil
		},
	}
}

// serverCommand lists machines.
func serverCommand() *Command {
	return &Command{
		Name:    "server",
		Aliases: []string{"machine"},
		Summary: "list the machines this credential can see",
		Children: []*Command{
			{
				Name:    "list",
				Summary: "list machines",
				Usage:   "server list",
				Run: func(ctx context.Context, env *Env, args []string) error {
					client, err := env.Client()
					if err != nil {
						return err
					}

					servers, err := client.Servers(ctx)
					if err != nil {
						return err
					}

					table := output.Table{
						Columns: []string{"label", "state", "provider", "region", "size", "address"},
						Empty:   "No machines.",
					}

					for _, s := range servers {
						address := s.IPv4
						if s.Shared {
							// Worth saying on the row rather than in a
							// legend: a shared machine is one whose
							// neighbours matter, and it is also the one that
							// gets no tenant host key.
							address += " (shared)"
						}

						table.Rows = append(table.Rows, []string{
							s.Label, s.State, s.Provider, s.Region, s.Size, address,
						})
					}

					return env.Printer.Print(table, map[string]any{"servers": servers})
				},
			},
		},
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}

	return "no"
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}

	return sha
}

func first(args []string) string {
	if len(args) > 0 {
		return args[0]
	}

	return ""
}

// ensureNoExtra refuses arguments a command does not take.
//
// Said rather than ignored: a mistyped flag arrives as a positional
// argument, and a command that silently discarded it would appear to have
// honoured it.
func ensureNoExtra(cmd *Command, args []string, taken int) error {
	if len(args) > taken {
		return Usagef(cmd, "unexpected argument %q", args[taken])
	}

	return nil
}
