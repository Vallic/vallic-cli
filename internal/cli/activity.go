package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/vallic/vallic-cli/internal/api"
	"github.com/vallic/vallic-cli/internal/output"
)

func activityListCommand() *Command {
	var (
		kinds       string
		limit       int
		before      int
		environment string
		everywhere  bool
	)

	return &Command{
		Name:    "list",
		Summary: "list recent tasks, newest first",
		Usage:   "activity list [<env>] [--type deploy,build] [--limit <n>]",
		Long: `Newest first, narrowed to the current environment. ` + "`--all`" + ` shows
everything the credential can see instead, across every project and machine.

QUEUED is when the platform was asked for the work, and TOOK is the run. A row
can be an hour old and have taken nine seconds, and a queued row's age is how
long it has been waiting.

--type takes several kinds at once, comma separated, because what somebody
thinks of as "a deploy" is more than one task type on the platform:

    vallic activity list --type deploy,build

A kind this team has never run is not an error; it lists nothing.

Paging is by cursor, not by page number. --before takes the ` + "`next_before`" + `
from the previous page, which ` + "`--format json`" + ` carries. That matters more
than it sounds: the list is newest-first on a table that grows at the head, so
with page numbers a deploy queued between two pages shifts every later row by
one, and the row a client skips is the failure it went looking for.`,
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&kinds, "type", "", "task kinds, comma separated")
			fs.IntVar(&limit, "limit", 0, "how many to show (default 25, most 100)")
			fs.IntVar(&before, "before", 0, "only tasks older than this id")
			fs.StringVar(&environment, "for", "", "an environment, by name")
			fs.BoolVar(&everywhere, "all", false, "every environment this credential can see")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			named := firstNonEmpty(first(args), environment)

			if everywhere && named != "" {
				return Usagef(nil, "--all is every environment, so it cannot be given one")
			}

			client, err := env.Client()
			if err != nil {
				return err
			}

			filter := api.TaskFilter{
				Types:  splitKinds(kinds),
				Before: before,
				Limit:  limit,
			}

			scope := "everything this credential can see"

			if !everywhere {
				target, resolveErr := env.ResolveEnvironment(ctx, named)
				if resolveErr != nil {
					return resolveErr
				}

				filter.Environment = target.Environment.ID
				scope = target.Environment.Name
			}

			page, err := client.Tasks(ctx, filter)
			if err != nil {
				return err
			}

			table := output.Table{
				Columns: []string{"id", "type", "state", "where", "queued", "took", "outcome"},
				Empty:   "Nothing has happened here yet.",
			}

			for i := range page.Tasks {
				row := &page.Tasks[i]

				table.Rows = append(table.Rows, []string{
					fmt.Sprint(row.ID),
					row.Type,
					taskState(row),
					taskWhere(row),
					// A column, not a note under the table: every row has an
					// age, and without one a deploy from five minutes ago and
					// a deploy from five weeks ago read as the same row.
					//
					// Queued, not started. A task that has not run has no
					// start, so a started column would be blank exactly where
					// somebody is asking how long a thing has been waiting.
					// `took` is the other half and measures the run.
					ago(&row.Created),
					took(row),
					row.Message,
				})
			}

			if env.Printer.Structured() {
				return env.Printer.Value(map[string]any{
					"scope":       scope,
					"tasks":       page.Tasks,
					"next_before": page.NextBefore,
				})
			}

			if err := env.Printer.Print(table, nil); err != nil {
				return err
			}

			if page.NextBefore != nil {
				// Said rather than left to be discovered. A list that quietly
				// stopped at 25 rows looks like a platform that did 25 things.
				env.Printer.Say("More: vallic activity list --before %d", *page.NextBefore)
			}

			return nil
		},
	}
}

// splitKinds turns a comma list into the kinds to ask for.
//
// An entry that is only whitespace is dropped rather than sent: the control
// plane matches an empty kind against nothing, so a trailing comma would
// silently narrow the list to zero rows.
func splitKinds(value string) []string {
	var kinds []string

	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			kinds = append(kinds, trimmed)
		}
	}

	return kinds
}

// taskState renders a state, with the attempt count where it is carrying one.
//
// "queued 1/3" says the platform failed once and is going to try again, which
// a bare "queued" does not, and somebody deciding whether to intervene needs
// the difference. Never "failed 2/3": a retryable failure goes back to queued
// on the control plane, and failed is where the attempts ran out.
//
// The threshold differs by state because the count means two things. It is
// raised on the claim, so a running task's figure is the attempt it is on and
// a 1 there is nothing to report, while a queued task's figure is what it has
// already spent and a 1 there is a failure it is backing off from. One `> 1`
// for both was the first shape here, and it printed a bare "queued" for the
// row that matters most: the one about to be tried a second time.
func taskState(row *api.TaskRow) string {
	spent := row.Attempts > 1
	if row.State == "queued" {
		spent = row.Attempts > 0
	}

	if spent && !row.Terminal() {
		return fmt.Sprintf("%s %d/%d", row.State, row.Attempts, row.MaxAttempts)
	}

	if row.State == "failed" && row.ExitCode != nil && *row.ExitCode != 0 {
		return fmt.Sprintf("failed (%d)", *row.ExitCode)
	}

	return row.State
}

// taskWhere names what a task ran against.
func taskWhere(row *api.TaskRow) string {
	switch {
	case row.Environment != nil:
		return row.Environment.Slug
	case row.Server != nil:
		return row.Server.Label
	default:
		// Platform work, which a tenant's credential should not be seeing at
		// all. Named rather than blanked so it is visible if it ever does.
		return "—"
	}
}

// took renders how long a task ran, or how long it has been running.
func took(row *api.TaskRow) string {
	if row.Started == nil || *row.Started == 0 {
		return "—"
	}

	end := row.Finished
	if end == nil || *end == 0 {
		if row.State != "running" {
			// Started but not finished and not running is a task waiting out
			// the backoff after a failure. The control plane sets `started`
			// on each attempt and never clears it, so measuring it against
			// now here would report a row that is doing nothing as one that
			// has been running since its last try began, and the figure would
			// then jump backwards the moment it was picked up again.
			return "—"
		}

		// Still going. Measured against now, so a stuck task's duration grows
		// while somebody watches it rather than reading as zero.
		return elapsedSince(*row.Started) + "…"
	}

	return duration(*end - *row.Started)
}

// duration renders a span of seconds the way somebody reads a build time.
//
// Rounded, because the exact second a task took is in `--format json` for
// anything that needs it, and a column of "1m 43.271s" is a column nobody
// compares across rows.
func duration(seconds int64) string {
	switch {
	case seconds < 0:
		// A finish before a start. Reported rather than rendered as a
		// negative: it means a clock moved, and pretending otherwise hides it.
		return "?"
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm %ds", seconds/60, seconds%60)
	default:
		return fmt.Sprintf("%dh %dm", seconds/3600, (seconds%3600)/60)
	}
}

// elapsedSince renders how long ago a timestamp was.
func elapsedSince(at int64) string {
	return duration(time.Now().Unix() - at)
}
