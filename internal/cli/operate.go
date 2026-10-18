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

// cdnCommand reads an environment's CDN settings and empties its cache.
//
// Read from the edge rather than from a copy, so what it prints is what the
// CDN is doing. Changing the settings stays in the console: the API takes
// every setting at once, as the console's form sends them, and a command that
// rewrote all of them to change one would be one copy away from undoing
// somebody else's change.
func cdnCommand() *Command {
	return &Command{
		Name:    "cdn",
		Summary: "show an environment's CDN settings, and clear its cache",
		Children: []*Command{
			{
				Name:    "show",
				Summary: "show what the CDN caches, and for how long",
				Usage:   "cdn show [<env>]",
				Run: func(ctx context.Context, env *Env, args []string) error {
					if err := ensureNoExtra(nil, args, 1); err != nil {
						return err
					}

					target, client, err := env.targetAndClient(ctx, first(args))
					if err != nil {
						return err
					}

					cdn, err := client.CDN(ctx, target.Environment.ID)
					if err != nil {
						return err
					}

					if env.Printer.Structured() {
						return env.Printer.Value(map[string]any{"cdn": cdn.Values})
					}

					values := cdn.Values

					return env.Printer.Print(output.Table{
						Columns: []string{"setting", "value"},
						Rows: [][]string{
							{"pages cached", yesNo(values.Dynamic)},
							{"browser cache, pages", cacheTime(values.BrowserCachePages)},
							{"browser cache, files", cacheTime(values.BrowserCacheFiles)},
							{"varies by", listOrNone(values.Vary)},
							{"cookies kept", listOrNone(values.Cookies)},
							{"headers kept", listOrNone(values.Headers)},
							{"countries refused", listOrNone(values.Blocked)},
							{"never cached", listOrNone(values.SkipPaths)},
							{"session cookies", listOrNone(values.SessionCookies)},
						},
					}, nil)
				},
			},
			{
				Name:    "clear",
				Aliases: []string{"purge"},
				Summary: "empty the CDN's cache for an environment",
				Usage:   "cdn clear [<env>]",
				Long: `Asks the CDN to drop everything it holds for the environment. The edge
serves fresh copies from the site within a minute or so, and until then answers
from what it has.

Nothing on the site changes: its own caches are its framework's, and drush cr
or its equivalent clears those.`,
				Run: func(ctx context.Context, env *Env, args []string) error {
					if err := ensureNoExtra(nil, args, 1); err != nil {
						return err
					}

					target, client, err := env.targetAndClient(ctx, first(args))
					if err != nil {
						return err
					}

					said, err := client.ClearCDN(ctx, target.Environment.ID)
					if err != nil {
						return err
					}

					if env.Printer.Structured() {
						return env.Printer.Value(map[string]any{"cleared": true, "message": said.Message})
					}

					env.Printer.Good("%s", firstNonEmpty(said.Message, "The CDN cache is being cleared."))

					return nil
				},
			},
		},
	}
}

// envMaintenanceCommand puts an environment behind its offline page.
func envMaintenanceCommand() *Command {
	var (
		note string
		yes  bool
	)

	return &Command{
		Name:    "maintenance",
		Summary: "put an environment behind its offline page, or take it out",
		Usage:   "env maintenance on|off [<env>] [--note <line>] [--yes]",
		Long: `With on, visitors get the offline page from the next reconcile, within
the minute, and the site keeps running behind it: deploys, cron and a shell
all still work. off serves it again.

--note is the one line the page shows, at most 200 characters; without it the
page keeps the note it had.

Turning it on asks first, because it is visitors who see it. With no terminal
to ask on, pass --yes.`,
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&note, "note", "", "the line the offline page shows")
			fs.BoolVar(&yes, "yes", false, "do not ask for confirmation")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			if len(args) == 0 {
				return Usagef(nil, "say on or off")
			}

			if err := ensureNoExtra(nil, args, 2); err != nil {
				return err
			}

			on, err := parseBool(args[0])
			if err != nil {
				return Usagef(nil, "say on or off, not %q", args[0])
			}

			target, client, err := env.targetAndClient(ctx, firstOf(args[1:]))
			if err != nil {
				return err
			}

			if on && !yes {
				if !env.Interactive() {
					return fmt.Errorf("this puts %s behind its offline page\n  there is no terminal to confirm on, so pass --yes if you mean it", target.Named())
				}

				if !env.confirm(fmt.Sprintf("Put %s behind its offline page?", target.Named())) {
					return fmt.Errorf("cancelled")
				}
			}

			var notePtr *string
			if note != "" {
				notePtr = &note
			}

			result, err := client.SetMaintenance(ctx, target.Environment.ID, on, notePtr)
			if err != nil {
				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(map[string]any{"maintenance": result})
			}

			env.Printer.Good("%s", result.Message)

			if result.Maintenance && result.Note != "" {
				env.Printer.Say("  the page says: %s", result.Note)
			}

			return nil
		},
	}
}

// metricsCommand summarises an environment's CPU, memory and disk.
//
// Upsun's metrics:all, as a summary rather than a chart: what it is using now,
// on average, and at worst, over the window the console's Resources tab shows.
func metricsCommand() *Command {
	var window string

	return &Command{
		Name:    "metrics",
		Aliases: []string{"resources"},
		Summary: "show an environment's CPU, memory and disk use",
		Usage:   "metrics [<env>] [--window 1h|24h|7d|30d|90d]",
		Long: `The latest reading, the average and the peak over the window — the last
day unless --window says otherwise. Every reading is the average of its step,
a minute for an hour and longer for longer windows, so a peak shorter than a
step reads lower than it was.

--format json prints every reading.`,
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&window, "window", "24h", "how far back: 1h, 24h, 7d, 30d or 90d")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			if err := ensureNoExtra(nil, args, 1); err != nil {
				return err
			}

			target, client, err := env.targetAndClient(ctx, first(args))
			if err != nil {
				return err
			}

			resources, err := client.Resources(ctx, target.Environment.ID, window)
			if err != nil {
				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(map[string]any{"resources": resources})
			}

			if len(resources.Points) == 0 {
				env.Printer.Say("No readings for %s in the last %s.", target.Named(), resources.Window)

				return nil
			}

			summary := summarise(resources.Points)

			env.Printer.Line("%s, the last %s, a reading every %s:", target.Named(), resources.Window, (time.Duration(resources.Step) * time.Second).Round(time.Second))
			env.Printer.Line("")

			return env.Printer.Print(output.Table{
				Columns: []string{"", "now", "average", "peak"},
				Rows: [][]string{
					{"cpu", percent(summary.cpu.latest), percent(summary.cpu.average), percent(summary.cpu.peak)},
					{"memory", humanBytes(int64(summary.memory.latest)), humanBytes(int64(summary.memory.average)), humanBytes(int64(summary.memory.peak))},
					{"disk", diskOrNone(summary.disk, summary.disk.latest), diskOrNone(summary.disk, summary.disk.average), diskOrNone(summary.disk, summary.disk.peak)},
				},
			}, nil)
		},
	}
}

// series is one measure over a window.
type series struct {
	latest, average, peak float64
	count                 int
}

func (s *series) add(value float64) {
	s.latest = value
	s.average += value
	s.count++

	if value > s.peak {
		s.peak = value
	}
}

func (s *series) finish() {
	if s.count > 0 {
		s.average /= float64(s.count)
	}
}

type resourceSummary struct {
	cpu, memory, disk series
}

// summarise reduces the readings, oldest first, to now, average and peak. A
// step with no disk reading is left out of the disk's figures, not counted as
// an empty disk.
func summarise(points []api.ResourcePoint) resourceSummary {
	var summary resourceSummary

	for _, point := range points {
		summary.cpu.add(point.CPUPercent)
		summary.memory.add(point.MemoryBytes)

		if point.DiskBytes != nil {
			summary.disk.add(*point.DiskBytes)
		}
	}

	summary.cpu.finish()
	summary.memory.finish()
	summary.disk.finish()

	return summary
}

func percent(value float64) string {
	return fmt.Sprintf("%.0f%%", value)
}

func diskOrNone(s series, value float64) string {
	if s.count == 0 {
		return "-"
	}

	return humanBytes(int64(value))
}

// cacheTime is a browser cache time as somebody reads one, or what happens
// without one.
func cacheTime(seconds *int) string {
	if seconds == nil {
		return "the site's own headers"
	}

	return humanSeconds(*seconds)
}

// humanSeconds is a span as the console's own choices name it: 5 minutes,
// 1 hour, 30 days.
func humanSeconds(seconds int) string {
	unit, size := "second", 1

	switch {
	case seconds >= 86400 && seconds%86400 == 0:
		unit, size = "day", 86400
	case seconds >= 3600 && seconds%3600 == 0:
		unit, size = "hour", 3600
	case seconds >= 60 && seconds%60 == 0:
		unit, size = "minute", 60
	}

	n := seconds / size
	if n != 1 {
		unit += "s"
	}

	return fmt.Sprintf("%d %s", n, unit)
}

func listOrNone(values []string) string {
	if len(values) == 0 {
		return "none"
	}

	return strings.Join(values, ", ")
}

// targetAndClient resolves an environment and hands back the client that
// resolved it, for the commands that make one request about it.
func (e *Env) targetAndClient(ctx context.Context, positional string) (*Target, *api.Client, error) {
	target, err := e.ResolveEnvironment(ctx, positional)
	if err != nil {
		return nil, nil, err
	}

	client, err := e.Client()
	if err != nil {
		return nil, nil, err
	}

	return target, client, nil
}
