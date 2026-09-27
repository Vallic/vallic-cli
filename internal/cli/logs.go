package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/vallic/vallic-cli/internal/api"
)

// logsCommand says where an environment's logs are.
//
// Not a tail. A site's request and error logs go from its machine's journal
// to the team's own destination and never to the control plane, so there is
// nothing here to stream — and a command that pretended to would show
// something other than what the team's destination holds. What the platform
// can say honestly is where they are. See the control plane's docs/cli.md,
// "Open decisions", for what reading them from here would take.
func logsCommand() *Command {
	return &Command{
		Name:    "logs",
		Summary: "say where an environment's logs are",
		Usage:   "logs [<env>]",
		Long: `Where this environment's request and error logs go: the destination the
project chose (Grafana Loki, Datadog, …), and the copy each machine keeps for
itself. They are read there — Vallic Cloud does not keep them.

What the platform did to the environment — deploys, provisioning, backups —
is a different log, and ` + "`vallic activity`" + ` reads it.`,
		Run: func(ctx context.Context, env *Env, args []string) error {
			target, err := env.ResolveEnvironment(ctx, first(args))
			if err != nil {
				return err
			}

			client, err := env.Client()
			if err != nil {
				return err
			}

			answer, err := client.Logs(ctx, target.Environment.ID)

			var refusal *api.Error
			if errors.As(err, &refusal) && refusal.Status == http.StatusNotFound {
				// An older control plane. Said plainly: the console's
				// configuration page has the same answer.
				return fmt.Errorf("this control plane does not say where logs go yet\n  the project's configuration page in the console has it")
			}
			if err != nil {
				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(answer)
			}

			for _, line := range logsReport(answer) {
				env.Printer.Line("%s", line)
			}

			return nil
		},
	}
}

// logsReport is what `vallic logs` prints, one line each.
func logsReport(answer *api.Logs) []string {
	lines := []string{fmt.Sprintf("Logs for %s are not kept by Vallic Cloud; they are read where they are sent.", answer.Environment)}

	switch {
	case answer.Destination != nil:
		lines = append(lines, fmt.Sprintf("  Sent to:        %s (%s)", answer.Destination.Name, answer.Destination.KindLabel))
	case answer.Unusable != nil:
		// The case worth the most words: somebody believes their logs go
		// somewhere, and they do not.
		lines = append(lines,
			fmt.Sprintf("  Sent to:        nowhere — %s (%s) is chosen, but it is switched off or", answer.Unusable.Name, answer.Unusable.KindLabel),
			"                  missing what it needs, so nothing reaches it.",
		)
	default:
		lines = append(lines, "  Sent to:        nowhere — this project has no log destination.")
	}

	if answer.MachineDays > 0 {
		lines = append(lines, fmt.Sprintf("  On the machine: the last %d days, which support can read for you.", answer.MachineDays))
	}

	if answer.SettingsURL != nil {
		lines = append(lines, fmt.Sprintf("  Change it:      %s", *answer.SettingsURL))
	}

	return append(lines, "", "What the platform did to it (deploys, provisioning, backups): vallic activity")
}
