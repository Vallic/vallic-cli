package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/vallic/vallic-cli/internal/api"
	"github.com/vallic/vallic-cli/internal/output"
)

// The people of the current team: who is in it, inviting more, their roles,
// taking them off. Upsun's user:list, user:add, user:update and user:delete,
// under the noun the platform has them on.
//
// The rules are the control plane's: a role is granted only by somebody who
// holds it, and anybody may take themselves off. Nothing here second-guesses
// them; a refusal arrives in its words.

func teamPeopleCommand() *Command {
	return &Command{
		Name:    "people",
		Summary: "list the team's members and its invitations still waiting",
		Usage:   "team people",
		Long: `Everybody in the current team, their role and the projects it is narrowed
to, and the invitations nobody has accepted yet. The invitations are shown only
to somebody who may invite.`,
		Run: func(ctx context.Context, env *Env, args []string) error {
			if err := ensureNoExtra(nil, args, 0); err != nil {
				return err
			}

			team, client, err := env.currentTeam(ctx)
			if err != nil {
				return err
			}

			people, err := client.TeamPeople(ctx, team.ID)
			if err != nil {
				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(people)
			}

			projects := projectLabels(people)

			members := output.Table{Columns: []string{"name", "email", "role", "projects"}}
			for _, member := range people.Members {
				name := member.User.Name
				if member.IsSelf {
					name += " (you)"
				}

				email := ""
				if member.User.Email != nil {
					email = *member.User.Email
				}

				members.Rows = append(members.Rows, []string{name, email, member.Role, projectsOf(member.Projects, projects)})
			}

			if err := env.Printer.Print(members, nil); err != nil {
				return err
			}

			if len(people.Invitations) == 0 {
				return nil
			}

			invitations := output.Table{Columns: []string{"invited", "role", "projects", "expires"}}
			for _, invitation := range people.Invitations {
				invitations.Rows = append(invitations.Rows, []string{
					invitation.Email, invitation.Role, projectsOf(invitation.Projects, projects),
					time.Unix(invitation.Expires, 0).Format("2 Jan 2006"),
				})
			}

			env.Printer.Line("")

			return env.Printer.Print(invitations, nil)
		},
	}
}

func teamInviteCommand() *Command {
	var (
		role     string
		projects string
	)

	return &Command{
		Name:    "invite",
		Summary: "invite somebody to the team, and print the link to send them",
		Usage:   "team invite <email> --role viewer|developer|admin|owner [--projects <a,b>]",
		Long: `Makes an invitation and prints its link. No mail is sent: the link is
yours to pass on, and it is printed this once — only its digest is kept, so
there is no getting it back. Withdraw the invitation and make another if it is
lost.

--projects narrows the role to those projects, by the name ` + "`vallic project list`" + `
prints; without it the role reaches every project. Only some roles can be
narrowed, and only to a role you hold yourself can you invite.`,
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&role, "role", "", "viewer, developer, admin or owner")
			fs.StringVar(&projects, "projects", "", "the projects the role reaches, comma separated (default: every project)")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			if len(args) == 0 {
				return Usagef(nil, "name the email address to invite")
			}

			if err := ensureNoExtra(nil, args, 1); err != nil {
				return err
			}

			if role == "" {
				return Usagef(nil, "--role is required: viewer, developer, admin or owner")
			}

			team, client, err := env.currentTeam(ctx)
			if err != nil {
				return err
			}

			ids, err := projectIDs(ctx, client, projects)
			if err != nil {
				return err
			}

			// Wrapped: an invitation lets somebody into the team, so the
			// control plane asks for a recent sign-in first.
			var invited *api.Invited
			err = env.WithStepUp(ctx, func() error {
				var callErr error
				invited, callErr = client.Invite(ctx, team.ID, args[0], role, ids)

				return callErr
			})
			if err != nil {
				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(invited)
			}

			env.Printer.Good("Invited %s to %s as %s.", invited.Invitation.Email, team.MachineName, invited.Invitation.Role)
			env.Printer.Say("  no mail is sent; send them this link, which is not shown again:")

			// On stdout, alone: the one thing here worth capturing.
			env.Printer.Line("%s", invited.Link)

			return nil
		},
	}
}

func teamRoleCommand() *Command {
	var projects string

	return &Command{
		Name:    "role",
		Summary: "give a member another role",
		Usage:   "team role <member> <role> [--projects <a,b>]",
		Long: `The member is named by email address or by name, as ` + "`vallic team people`" + `
prints them. --projects narrows the role, as for team invite; without it the
role reaches every project.`,
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&projects, "projects", "", "the projects the role reaches, comma separated (default: every project)")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			if len(args) < 2 {
				return Usagef(nil, "name the member and the role, as `vallic team role ana@example.com developer`")
			}

			if err := ensureNoExtra(nil, args, 2); err != nil {
				return err
			}

			team, client, err := env.currentTeam(ctx)
			if err != nil {
				return err
			}

			member, err := findMember(ctx, client, team, args[0])
			if err != nil {
				return err
			}

			ids, err := projectIDs(ctx, client, projects)
			if err != nil {
				return err
			}

			// Wrapped, for the same reason as an invitation.
			var changed *api.MembershipChanged
			err = env.WithStepUp(ctx, func() error {
				var callErr error
				changed, callErr = client.ChangeMembership(ctx, member.ID, args[1], ids)

				return callErr
			})
			if err != nil {
				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(changed)
			}

			env.Printer.Good("%s", firstNonEmpty(changed.Message, member.User.Name+" is now "+changed.Membership.Role+"."))

			return nil
		},
	}
}

func teamRemoveCommand() *Command {
	var yes bool

	return &Command{
		Name:    "remove",
		Summary: "take a member off the team, or leave it",
		Usage:   "team remove <member> [--yes]",
		Long: `Takes the member off the current team; named as for team role. Their
keys stop opening the team's environments at the next reconcile. Naming
yourself leaves the team.

It asks first. With no terminal to ask on, pass --yes.`,
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&yes, "yes", false, "do not ask for confirmation")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			if len(args) == 0 {
				return Usagef(nil, "name the member to take off, by email or name")
			}

			if err := ensureNoExtra(nil, args, 1); err != nil {
				return err
			}

			team, client, err := env.currentTeam(ctx)
			if err != nil {
				return err
			}

			member, err := findMember(ctx, client, team, args[0])
			if err != nil {
				return err
			}

			question := fmt.Sprintf("Take %s off %s?", member.User.Name, team.MachineName)
			if member.IsSelf {
				question = fmt.Sprintf("Leave %s?", team.MachineName)
			}

			if !yes {
				if !env.Interactive() {
					return fmt.Errorf("%s\n  there is no terminal to confirm on, so pass --yes if you mean it", strings.TrimSuffix(question, "?"))
				}

				if !env.confirm(question) {
					return fmt.Errorf("cancelled")
				}
			}

			if err := client.RemoveMembership(ctx, member.ID); err != nil {
				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(map[string]any{"removed": member})
			}

			if member.IsSelf {
				env.Printer.Good("You have left %s.", team.MachineName)
			} else {
				env.Printer.Good("%s is no longer in %s.", member.User.Name, team.MachineName)
			}

			return nil
		},
	}
}

func teamWithdrawCommand() *Command {
	return &Command{
		Name:    "withdraw",
		Summary: "take back an invitation nobody has accepted",
		Usage:   "team withdraw <email>",
		Long:    `The invitation's link stops working. The address can be invited again.`,
		Run: func(ctx context.Context, env *Env, args []string) error {
			if len(args) == 0 {
				return Usagef(nil, "name the email address the invitation went to")
			}

			if err := ensureNoExtra(nil, args, 1); err != nil {
				return err
			}

			team, client, err := env.currentTeam(ctx)
			if err != nil {
				return err
			}

			people, err := client.TeamPeople(ctx, team.ID)
			if err != nil {
				return err
			}

			var found *api.TeamInvitation
			for i := range people.Invitations {
				if strings.EqualFold(people.Invitations[i].Email, args[0]) {
					found = &people.Invitations[i]
					break
				}
			}

			if found == nil {
				return fmt.Errorf("no invitation to %s is waiting in %s\n  `vallic team people` lists the ones that are", args[0], team.MachineName)
			}

			if err := client.WithdrawInvitation(ctx, found.ID); err != nil {
				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(map[string]any{"withdrawn": found})
			}

			env.Printer.Good("The invitation to %s is withdrawn; its link no longer works.", found.Email)

			return nil
		},
	}
}

// currentTeam is the team commands act in, as `team switch` recorded it, or
// the one team the credential reaches.
func (e *Env) currentTeam(ctx context.Context) (*api.Team, *api.Client, error) {
	teams, current, err := e.teams(ctx)
	if err != nil {
		return nil, nil, err
	}

	for i := range teams {
		if teams[i].MachineName == current {
			client, err := e.Client()

			return &teams[i], client, err
		}
	}

	if current != "" {
		return nil, nil, fmt.Errorf("this credential does not reach %s, the team recorded as current\n  choose another: vallic team switch <name>", current)
	}

	return nil, nil, Usagef(nil, "this credential reaches several teams; choose one with `vallic team switch <name>`, or pass --team")
}

// findMember finds a member by email or name, which is how somebody knows
// them; the membership id is the API's.
func findMember(ctx context.Context, client *api.Client, team *api.Team, who string) (*api.TeamMember, error) {
	people, err := client.TeamPeople(ctx, team.ID)
	if err != nil {
		return nil, err
	}

	var matches []api.TeamMember

	for _, member := range people.Members {
		email := ""
		if member.User.Email != nil {
			email = *member.User.Email
		}

		if strings.EqualFold(email, who) || strings.EqualFold(member.User.Name, who) {
			matches = append(matches, member)
		}
	}

	switch len(matches) {
	case 1:
		return &matches[0], nil
	case 0:
		return nil, fmt.Errorf("nobody called %s is in %s\n  `vallic team people` lists who is", who, team.MachineName)
	default:
		return nil, errors.New("more than one member is called " + who + "; name them by email address")
	}
}

// projectIDs turns --projects, machine names or labels, into the ids the API
// takes. Empty is every project, which is nil.
func projectIDs(ctx context.Context, client *api.Client, names string) ([]int, error) {
	if strings.TrimSpace(names) == "" {
		return nil, nil
	}

	projects, err := client.Projects(ctx)
	if err != nil {
		return nil, err
	}

	ids := []int{}

	for _, name := range strings.Split(names, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}

		found := false
		for _, project := range projects {
			if strings.EqualFold(project.MachineName, name) || strings.EqualFold(project.Label, name) {
				ids = append(ids, project.ID)
				found = true

				break
			}
		}

		if !found {
			return nil, Usagef(nil, "no project called %q; `vallic project list` shows their names", name)
		}
	}

	return ids, nil
}

// projectLabels are the team's projects by id, as the roster names them.
func projectLabels(people *api.TeamPeople) map[int]string {
	labels := make(map[int]string, len(people.Projects))
	for _, project := range people.Projects {
		labels[project.ID] = project.Label
	}

	return labels
}

// projectsOf is a role's projects as somebody reads them; none is every one.
func projectsOf(ids []int, labels map[int]string) string {
	if len(ids) == 0 {
		return "all"
	}

	names := make([]string, 0, len(ids))
	for _, id := range ids {
		names = append(names, firstNonEmpty(labels[id], fmt.Sprintf("#%d", id)))
	}

	return strings.Join(names, ", ")
}
