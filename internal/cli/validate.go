package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vallic/vallic-cli/internal/api"
	"github.com/vallic/vallic-cli/internal/output"
)

// validateCommand checks a manifest before it is committed.
func validateCommand() *Command {
	var (
		against string
		local   bool
	)

	return &Command{
		Name:    "validate",
		Summary: "check vallic.yaml before you commit it",
		Usage:   "validate [<file>] [--against <env>]",
		Long: `Checks the file in your working tree, not the one on the branch, so
it answers before anything is pushed.

There are two answers and they are not the same, which is why the command tells
you which one you got.

--local checks syntax and schema only. It reaches no environment and needs no
project, so it works on a file for a site that does not exist yet. A pass is
not a promise that a deploy succeeds: it cannot see a service the environment
does not run, a version the catalogue does not offer, or a variable the file
requires and nobody has set.

Without --local it is checked against an environment, which is the answer that
matters: a pass is the same test the deploy itself makes.

A file with nothing wrong is echoed back as the platform read it, which is
where to check that a key landed: a service missing from that list was not
read, whatever the file says. A file with problems gets the problems instead,
because that is what somebody with a broken file came for.

Exits 1 when the file has problems, so it drops into a pre-commit hook:

    vallic validate --local || exit 1`,
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&against, "against", "", "the environment to check against")
			fs.BoolVar(&local, "local", false, "syntax and schema only, with no environment")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			if local && against != "" {
				return Usagef(nil, "--local checks against nothing, so it cannot be given an environment")
			}

			path, contents, err := readManifest(first(args))
			if err != nil {
				return err
			}

			client, err := env.Client()
			if err != nil {
				return err
			}

			var validation *api.Validation

			if local {
				validation, err = client.Validate(ctx, contents)
			} else {
				target, resolveErr := env.ResolveEnvironment(ctx, against)
				if resolveErr != nil {
					return fmt.Errorf("%w\n  or check the file on its own: vallic validate --local", resolveErr)
				}

				validation, err = client.ValidateAgainst(ctx, target.Environment.ID, contents)
			}

			if err != nil {
				return err
			}

			if env.Printer.Structured() {
				if err := env.Printer.Value(map[string]any{"validation": validation}); err != nil {
					return err
				}

				return validationOutcome(validation)
			}

			return reportValidation(env, path, validation)
		},
	}
}

// readManifest reads the file to check, or finds it.
func readManifest(named string) (string, string, error) {
	path := named

	if path == "" {
		// The conventional name, in the working directory. Looked for rather
		// than required as an argument, because the overwhelmingly common case
		// is running this at the root of a checkout.
		path = "vallic.yaml"

		if _, err := os.Stat(path); err != nil {
			return "", "", fmt.Errorf(
				"no vallic.yaml here\n  name one: vallic validate path/to/vallic.yaml",
			)
		}
	}

	if path == "-" {
		return "", "", Usagef(nil, "name a file; reading a manifest from standard input is not supported")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("cannot read %s: %w", path, err)
	}

	if strings.TrimSpace(string(raw)) == "" {
		// Named as a client problem rather than sent. The control plane would
		// answer "this repository has no vallic.yaml", which is the wrong
		// sentence to read about a file you are looking at.
		return "", "", fmt.Errorf("%s is empty", path)
	}

	return filepath.Clean(path), string(raw), nil
}

// reportValidation prints what the control plane made of the file.
func reportValidation(env *Env, path string, validation *api.Validation) error {
	// Said first, every time. "Valid" means two different things depending on
	// what it was checked against, and a reader who does not know which they
	// got has been told nothing.
	if validation.CheckedAgainst == nil {
		env.Printer.Say("%s, syntax and schema only (no environment).", path)
	} else {
		env.Printer.Say("%s, checked against %s.", path, validation.CheckedAgainst.Slug)
	}

	if base := filepath.Base(path); validation.Filename != "" && base != validation.Filename {
		// A pass on a file under some other name is a pass on something no
		// deploy will ever read, and this command is most often run on a file
		// somebody is about to commit. The name is the control plane's rather
		// than a constant here, so this stays true the day it changes.
		env.Printer.Say("  a deploy reads %s from the repository, and nothing else", validation.Filename)
	}

	for _, problem := range validation.Problems {
		env.Printer.Warn("  %s", problem)
	}

	if len(validation.Clamps) > 0 {
		// Not problems. The file is accepted and the platform will use
		// something else, which somebody reading their own file needs to know
		// before they wonder why the number they wrote is not the one running.
		env.Printer.Say("")
		env.Printer.Say("Asked for, and not honoured as written:")

		if err := env.Printer.Print(output.Table{
			Columns: []string{"field", "asked", "applied"},
			Rows:    clampRows(validation.Clamps),
		}, nil); err != nil {
			return err
		}
	}

	if validation.Adoptable != nil {
		if len(validation.Adoptable.Added) > 0 {
			env.Printer.Say("A deploy would switch on: %s", joinOrDash(validation.Adoptable.Added))
		}

		if len(validation.Adoptable.Refused) > 0 {
			env.Printer.Warn("A deploy would refuse: %s", joinOrDash(validation.Adoptable.Refused))
		}
	}

	if validation.Valid {
		// Only where there is nothing wrong, and the reason is the problems
		// rather than the echo. Printing it either way was the first shape
		// here, and a file with three problems answered with twenty lines of
		// manifest under them is a file whose problems have scrolled off the
		// top of the terminal. Somebody with a broken file came for the list;
		// somebody with a good one came to see which of their keys landed.
		reportManifest(env, validation)

		if validation.CheckedAgainst == nil {
			env.Printer.Good("Understood, and nothing in it is misread.")
			// The honest caveat. A pass from the cheap route is worth having
			// and is not the answer somebody is usually looking for.
			env.Printer.Say("  a deploy may still refuse it: run without --local to compare it against an environment")
		} else {
			env.Printer.Good("This deploys to %s.", validation.CheckedAgainst.Slug)
		}
	}

	return validationOutcome(validation)
}

// validationOutcome turns an invalid file into a non-zero exit.
//
// An error rather than a message, because this command's whole job in a
// pre-commit hook is its exit code.
func validationOutcome(validation *api.Validation) error {
	if validation.Valid {
		return nil
	}

	if len(validation.Problems) == 1 {
		return fmt.Errorf("1 problem")
	}

	return fmt.Errorf("%d problems", len(validation.Problems))
}

func clampRows(clamps []api.Clamp) [][]string {
	rows := make([][]string, 0, len(clamps))

	for _, clamp := range clamps {
		asked := clamp.Asked
		if asked == "" {
			asked = "—"
		}

		applied := clamp.Applied
		if applied == "" {
			// An empty applied means the asked value was ignored entirely
			// rather than replaced with something else.
			applied = "ignored"
		}

		rows = append(rows, []string{clamp.Field, asked, applied})
	}

	return rows
}

// reportManifest echoes what the parser understood, under the platform's own
// name for the file and its schema.
//
// The name and the schema are the control plane's to state, and are sent so
// that no client carries its own copy of either. A heading with "vallic.yaml"
// compiled into it would go on printing that the day the platform reads
// something else, to the one person who ran this command to find out.
func reportManifest(env *Env, validation *api.Validation) {
	entries := manifestEntries(&validation.Manifest)
	schedule := cronLines(validation.Manifest.Cron)

	if len(entries) == 0 && len(schedule) == 0 {
		return
	}

	env.Printer.Say("")
	env.Printer.Say("Read as %s, schema %d:", validation.Filename, validation.SchemaVersion)

	width := 0

	for _, entry := range entries {
		if len(entry.Key) > width {
			width = len(entry.Key)
		}
	}

	for _, entry := range entries {
		env.Printer.Say("  %-*s  %s", width+1, entry.Key+":", entry.Value)
	}

	if len(schedule) > 0 {
		env.Printer.Say("")
		env.Printer.Say("Scheduled:")

		for _, line := range schedule {
			env.Printer.Say("  %s", line)
		}
	}
}

// manifestEntry is one line of that echo: a key of the file, and what the
// control plane read it as.
type manifestEntry struct {
	Key   string
	Value string
}

// manifestEntries is the keys that landed, in the order the control plane
// describes them in.
//
// A key with nothing behind it is left out rather than printed as a dash.
// Every key is always present in the response, deliberately, so that no
// script has to test for one. A person echoing their own file is asking
// which of their keys landed, and answering that with fifteen lines, eleven
// of them dashes, hides the four that did. What is missing here is what was
// not read, which is the whole of the question.
func manifestEntries(manifest *api.Manifest) []manifestEntry {
	entries := make([]manifestEntry, 0, 12)

	add := func(key, value string) {
		if value != "" {
			entries = append(entries, manifestEntry{Key: key, Value: value})
		}
	}

	add("type", manifest.Type)
	add("runtime", settingsLine(manifest.Runtime))
	add("start", manifest.Start)

	if manifest.Port != 0 {
		add("port", fmt.Sprint(manifest.Port))
	}

	add("services", manifestServices(manifest.Services))
	add("workers", manifestWorkers(manifest.Workers))
	add("build steps", manifestBuildSteps(manifest.BuildSteps))
	add("build cache", strings.Join(manifest.BuildCache, ", "))
	add("deploy steps", strings.Join(manifest.DeploySteps, ", "))

	if manifest.RollbackOnFailure {
		// Said only where it is on. Off is the platform's behaviour for a
		// file that never mentioned it, and a line reading "rollback: no"
		// would be read as a setting somebody chose.
		add("rollback", "on a failed deploy")
	}

	add("health", healthLine(manifest.HealthPath, manifest.HealthTimeout))
	add("required env", strings.Join(manifest.RequiredEnv, ", "))
	add("mounts", strings.Join(manifest.Mounts, ", "))
	add("extras", manifestExtras(manifest.Extras))

	return entries
}

// cronLines renders the schedule, marking what the platform put there.
//
// The marker goes beside the line it concerns rather than in a column of its
// own: it is true of every line in a file that schedules nothing and of none
// of them in a file that schedules its own, so a column would be a word
// repeated down the page or a column of dashes. What it must never be is
// left off. A client that could not tell offers to edit a line that is not in
// the repository, and somebody then goes looking for it.
func cronLines(jobs []api.CronJob) []string {
	names, schedules := 0, 0

	for i := range jobs {
		if len(jobs[i].Name) > names {
			names = len(jobs[i].Name)
		}

		if len(jobs[i].Schedule) > schedules {
			schedules = len(jobs[i].Schedule)
		}
	}

	lines := make([]string, 0, len(jobs))

	for _, job := range jobs {
		line := fmt.Sprintf("%-*s  %-*s  %s", names, job.Name, schedules, job.Schedule, job.Command)

		if job.Default {
			line += "  (the platform's own, because the file schedules nothing)"
		}

		lines = append(lines, line)
	}

	return lines
}

// manifestServices renders the services a repository asked to run beside it.
func manifestServices(services []api.ManifestService) string {
	parts := make([]string, 0, len(services))

	for _, service := range services {
		part := service.Name

		if service.Version != "" {
			part += " " + service.Version
		}

		if keys := sortedMapKeys(service.Environment); len(keys) > 0 {
			// The names of what the file sets on that service, never the
			// values: a repository is not where a credential belongs, and the
			// question here is which keys landed rather than what is in them.
			part += " (" + strings.Join(keys, ", ") + ")"
		}

		parts = append(parts, part)
	}

	return strings.Join(parts, ", ")
}

// manifestWorkers renders the long-running processes beside the application.
func manifestWorkers(workers []api.Worker) string {
	parts := make([]string, 0, len(workers))

	for _, worker := range workers {
		part := worker.Name

		if worker.Replicas > 1 {
			part += fmt.Sprintf(" ×%d", worker.Replicas)
		}

		parts = append(parts, part)
	}

	return strings.Join(parts, ", ")
}

// manifestExtras renders the customer's own machines and how to reach them.
//
// The ports are the point. An extra is a machine running software this platform
// did not write, and the only thing the manifest settles about it is which
// addresses the application may call -- so a line that named the slot and
// stopped would confirm the least useful half. Somebody checking this file is
// checking that the port they wrote is the port that will answer.
//
// The compose fragment is not shown because it is not sent: it is a document
// with its own schema that nothing here validates, and echoing half of it
// would imply otherwise.
func manifestExtras(extras []api.ManifestExtra) string {
	parts := make([]string, 0, len(extras))

	for _, extra := range extras {
		part := extra.Name

		if ports := exposedPorts(extra.Expose); ports != "" {
			part += " (" + ports + ")"
		}

		if extra.InternetEgress {
			// Said only where it is on, like rollback above: off is what an
			// extra gets for not asking, and printing that would read as a
			// choice somebody made.
			part += ", internet egress"
		}

		parts = append(parts, part)
	}

	return strings.Join(parts, ", ")
}

// exposedPorts renders one extra's ports, keyed by the service inside it.
//
// `8000` where the two numbers agree and `8001→9090` where they do not, which
// is the case worth seeing: the left number is the one the application dials
// and the right one is the service's own, and a renderer that showed only one
// of them would hide whichever was wrong.
func exposedPorts(expose api.ExposeMap) string {
	services := make([]string, 0, len(expose))

	for service := range expose {
		services = append(services, service)
	}

	// An order that does not move between runs, for the same reason
	// settingsLine sorts: this output is read in diffs.
	sort.Strings(services)

	parts := make([]string, 0, len(services))

	for _, service := range services {
		ports := make([]string, 0, len(expose[service]))

		for _, port := range expose[service] {
			if port.Listen == port.Container {
				ports = append(ports, fmt.Sprint(port.Listen))

				continue
			}

			ports = append(ports, fmt.Sprintf("%d→%d", port.Listen, port.Container))
		}

		parts = append(parts, service+" "+strings.Join(ports, " "))
	}

	return strings.Join(parts, ", ")
}

// manifestBuildSteps renders the build, naming a step that runs somewhere
// else.
//
// The image is worth the words: a step with one runs in a container the
// application is not in, so a step that expects the repository's own tools to
// be on its path is a build failure with no obvious cause.
func manifestBuildSteps(steps []api.BuildStep) string {
	parts := make([]string, 0, len(steps))

	for _, step := range steps {
		// The command where the step has no name, which is what the control
		// plane's own BuildStep::label() does. Naming a step is optional and
		// most files skip it, so keying off the name alone rendered the whole
		// build as empty and dropped the line: `vallic init` writes exactly
		// one unnamed step, and a manifest that silently showed no build is
		// the one mistake this output exists to catch.
		part := step.Name
		if part == "" {
			part = step.Command
		}

		if step.Image != "" {
			part += " (in " + step.Image + ")"
		}

		parts = append(parts, part)
	}

	return strings.Join(parts, ", ")
}

// healthLine renders the health check, with the wait where there is one.
func healthLine(path string, timeout int) string {
	if path == "" {
		return ""
	}

	if timeout == 0 {
		return path
	}

	return fmt.Sprintf("%s, %ds", path, timeout)
}

// settingsLine renders a map of settings, sorted by key.
//
// Sorted because Go randomises map order, and a command whose output moves
// between two runs over the same file is one nobody can diff.
func settingsLine(settings map[string]string) string {
	keys := sortedMapKeys(settings)
	parts := make([]string, 0, len(keys))

	for _, key := range keys {
		parts = append(parts, key+" "+settings[key])
	}

	return strings.Join(parts, ", ")
}

// sortedMapKeys is a map's keys in an order that does not move.
func sortedMapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))

	for key := range values {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}
