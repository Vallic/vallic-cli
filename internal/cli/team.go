package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/vallic/vallic-cli/internal/api"
	"github.com/vallic/vallic-cli/internal/auth"
	"github.com/vallic/vallic-cli/internal/output"
)

// teamCommand lists the teams a credential reaches, and picks one.
//
// What `switch` means depends on the credential, and that is not a wart — it
// is the difference between the two showing through. A personal access token
// is minted for one team and narrows every request to it, so with a token
// there is nothing to switch: reaching another team means holding another
// token. A browser sign-in reaches every team its owner belongs to, and then
// the choice is real and lives in the config file.
//
// The command exists under both and says which it is, because a `switch` that
// silently did nothing would be worse than one that explains itself.
func teamCommand() *Command {
	return &Command{
		Name:    "team",
		Summary: "show which teams this credential reaches",
		Children: []*Command{
			teamListCommand(),
			teamSwitchCommand(),
		},
	}
}

func teamListCommand() *Command {
	return &Command{
		Name:    "list",
		Summary: "list the teams this credential reaches",
		Usage:   "team list",
		Run: func(ctx context.Context, env *Env, args []string) error {
			if err := ensureNoExtra(nil, args, 0); err != nil {
				return err
			}

			teams, current, err := env.teams(ctx)
			if err != nil {
				return err
			}

			table := output.Table{
				Columns: []string{"", "name", "label", "role"},
				Empty:   "This credential reaches no teams. Ask to be invited to one.",
			}

			for _, team := range teams {
				marker := ""
				if team.MachineName == current {
					marker = "*"
				}

				table.Rows = append(table.Rows, []string{
					marker, team.MachineName, team.Label, team.Role,
				})
			}

			return env.Printer.Print(table, map[string]any{
				"teams":   teams,
				"current": nullableString(current),
			})
		},
	}
}

func teamSwitchCommand() *Command {
	return &Command{
		Name:    "switch",
		Summary: "choose the team commands act in",
		Usage:   "team switch <name>",
		Long: `Only meaningful for a credential that reaches more than one team.

A personal access token is minted for one team and narrows every request to
it, so there is nothing for this to change — reaching another team means
holding another token, which is minted in the console. A browser sign-in
reaches every team you belong to, and this records which of them to act in.`,
		Run: func(ctx context.Context, env *Env, args []string) error {
			if len(args) == 0 {
				return Usagef(nil, "name a team; `vallic team list` shows which")
			}

			if err := ensureNoExtra(nil, args, 1); err != nil {
				return err
			}

			wanted := args[0]

			teams, _, err := env.teams(ctx)
			if err != nil {
				return err
			}

			var chosen *api.Team
			for i := range teams {
				if strings.EqualFold(teams[i].MachineName, wanted) || strings.EqualFold(teams[i].Label, wanted) {
					chosen = &teams[i]
					break
				}
			}

			if chosen == nil {
				names := make([]string, 0, len(teams))
				for _, team := range teams {
					names = append(names, team.MachineName)
				}

				if len(names) == 0 {
					return fmt.Errorf("this credential reaches no teams, so there is nothing to switch to")
				}

				return Usagef(nil, "this credential does not reach a team called %q\n  it reaches: %s", wanted, strings.Join(names, ", "))
			}

			creds, err := env.Credentials()
			if err != nil {
				return err
			}

			cfg, err := env.Config()
			if err != nil {
				return err
			}

			cfg.Team = chosen.MachineName
			if err := cfg.Save(); err != nil {
				return err
			}

			env.Printer.Good("Acting in %s as %s.", chosen.MachineName, chosen.Role)

			// Said plainly rather than left to be discovered. A token already
			// narrows every request, so recording a team changes nothing
			// about what the next command can reach — and somebody who ran
			// this expecting it to should hear so now rather than wonder why
			// the team never changed.
			if creds.Kind != auth.KindOAuth && len(teams) <= 1 {
				env.Printer.Say("  this token was minted for that team, so nothing else was reachable anyway")
			}

			return nil
		},
	}
}

// teams reads the reachable teams and which one is current.
func (e *Env) teams(ctx context.Context) ([]api.Team, string, error) {
	client, err := e.Client()
	if err != nil {
		return nil, "", err
	}

	teams, err := client.Teams(ctx)
	if err != nil {
		return nil, "", err
	}

	cfg, err := e.Config()
	if err != nil {
		return nil, "", err
	}

	current := cfg.Team

	// With one reachable team there is nothing to choose, so it is the
	// current one whether or not anything was ever recorded — which is the
	// state every personal access token is in.
	if current == "" && len(teams) == 1 {
		current = teams[0].MachineName
	}

	return teams, current, nil
}

// nullableString keeps an unset value out of the JSON as null rather than as
// an empty string, which a script would have to test for separately.
func nullableString(s string) any {
	if s == "" {
		return nil
	}

	return s
}
