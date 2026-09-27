package cli

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/vallic/vallic-cli/internal/api"
	"github.com/vallic/vallic-cli/internal/output"
)

// backupCommand works an environment's backups.
func backupCommand() *Command {
	return &Command{
		Name:    "backup",
		Summary: "list, take, download and restore backups",
		Long: `A backup is not a file sitting somewhere you can reach. It lives in
an encrypted repository whose key the platform holds and whose storage you have
no credentials for, which is the point: a copy of a customer's database that
anybody with a link could fetch is not a backup, it is a leak waiting to
happen.

So ` + "`backup download`" + ` is two steps rather than one. The first asks a
machine to read the snapshot out and write it somewhere fetchable; the second,
once that has finished, hands back a signed link that lasts a day.

There is no refresh. Every backup and every removal queues a fresh listing
behind itself, because a list that is only current when somebody remembers to
press something is a list that is wrong the morning after a backup ran.`,
		Children: []*Command{
			backupListCommand(),
			backupCreateCommand(),
			backupDownloadCommand(),
			backupRestoreCommand(),
		},
	}
}

func backupListCommand() *Command {
	var kind string

	return &Command{
		Name:    "list",
		Summary: "list the backups that exist",
		Usage:   "backup list [<env>] [--kind database|files]",
		Long: `Newest first, as of the last listing. The LISTED line says when that
was: nothing here is read live, because reading a repository costs a machine
several minutes.

A backup shown as ` + "`unknown`" + ` was not taken by the platform, so it cannot
say whether it holds a database or files. Those cannot be downloaded or
restored, deliberately — guessing would risk unpacking a database dump over a
site's files.

A backup the platform recorded but no listing has seen is named under the table
rather than shown as a row. The rows are what the repository held when it was
last read, and adding a row for something nobody has found there would be
promising a snapshot on the platform's word alone.

There is no size column, and that is a limitation rather than a choice: the
platform does not collect snapshot sizes today.`,
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&kind, "kind", "", "database or files")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			if kind != "" {
				if _, err := backupKind(kind); err != nil {
					return err
				}
			}

			target, err := env.ResolveEnvironment(ctx, first(args))
			if err != nil {
				return err
			}

			client, err := env.Client()
			if err != nil {
				return err
			}

			listing, err := client.Backups(ctx, target.Environment.ID)
			if err != nil {
				return err
			}

			// Ordered here rather than trusted. The control plane hands the
			// listing through in whatever order the machine that read the
			// repository wrote it, and promises none, so the "newest first"
			// this command documents is only true if the client makes it
			// true. Printing them in arrival order was the alternative: it
			// puts an arbitrary night at the top of the list people read to
			// find the most recent backup.
			sortSnapshotsNewestFirst(listing.Snapshots)

			if env.Printer.Structured() {
				return env.Printer.Value(map[string]any{"backups": listing})
			}

			if !listing.BackedUp {
				env.Printer.Warn("%s is not being backed up.", listing.Environment)

				return nil
			}

			table := output.Table{
				Columns: []string{"id", "kind", "taken", "kept", "download"},
				Empty:   "No backups yet.",
			}

			for i := range listing.Snapshots {
				snapshot := &listing.Snapshots[i]

				if kind != "" && snapshot.Kind != kind {
					continue
				}

				table.Rows = append(table.Rows, []string{
					snapshot.ShortID,
					snapshot.Kind,
					ago(snapshot.Taken),
					keptFor(snapshot),
					exportState(snapshot),
				})
			}

			if err := env.Printer.Print(table, nil); err != nil {
				return err
			}

			for _, line := range listingAgeLines(listing) {
				env.Printer.Say("%s", line)
			}

			for _, inFlight := range []string{"database", "files"} {
				if id := listing.InFlight[inFlight]; id != nil {
					env.Printer.Say("A %s backup is running now: vallic activity log %d", inFlight, *id)
				}
			}

			repository := listing.Repository

			if repository == nil {
				// The two states somebody with no repository can be in, and
				// they are opposite answers: one is a backup that has not
				// happened yet, the other is one that cannot happen. This used
				// to print the first for both, which told a team with nowhere
				// to write that all they had to do was wait.
				if listing.PlatformFallback {
					env.Printer.Say("No backup has run here yet, and there is somewhere for one to be written when it does.")
				} else {
					env.Printer.Warn("No backup has run here, and the platform has no shared storage to write one to.")
					env.Printer.Say("  unless this team has brought storage of its own, waiting will not produce a backup")
				}

				return nil
			}

			// Only where there is a repository. Under "there is nowhere to
			// write a backup", how long backups are kept reads as a
			// reassurance about something that is not happening.
			if line := retentionLine(listing.Retention); line != "" {
				env.Printer.Say("%s", line)
			}

			if line, failed := verificationLine(repository); failed {
				env.Printer.Warn("%s", line)
			} else {
				env.Printer.Say("%s", line)
			}

			if line, gone := destinationLine(repository); gone {
				env.Printer.Warn("%s", line)
			} else {
				env.Printer.Say("%s", line)
			}

			return nil
		},
	}
}

func backupCreateCommand() *Command {
	var (
		kind string
		wait bool
	)

	return &Command{
		Name:    "create",
		Summary: "take a backup now",
		Usage:   "backup create [<env>] --kind database|files",
		Long: `--kind is required rather than defaulted, because the two are
genuinely different acts: a database dump is minutes and a files sync can be
hours.

A manual backup is kept differently from a scheduled one. It is held out of the
nightly expiry pass and expires by its age instead, so taking one before a risky
change does not quietly become a permanent copy of production. How many days
that is belongs to the environment rather than to this page, and
` + "`vallic backup list`" + ` says it in the environment's own numbers.

Not gated on confirming who you are, even on protected production. A backup is
protective, and a gate on the thing people do *before* something risky is a gate
in the wrong place.`,
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&kind, "kind", "", "database or files")
			fs.BoolVar(&wait, "wait", false, "block until it finishes")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			chosen, err := backupKind(kind)
			if err != nil {
				return err
			}

			target, err := env.ResolveEnvironment(ctx, first(args))
			if err != nil {
				return err
			}

			client, err := env.Client()
			if err != nil {
				return err
			}

			queued, err := client.TakeBackup(ctx, target.Environment.ID, chosen)
			if err != nil {
				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(queued)
			}

			env.Printer.Good("%s backup of %s queued (task %d)",
				queued.Backup.Kind, queued.Backup.Environment, queued.Backup.Task.ID)

			if !wait {
				env.Printer.Say("  follow it: vallic activity log %d", queued.Backup.Task.ID)

				return nil
			}

			return env.followTask(ctx, queued.Backup.Task.ID, 2*time.Hour)
		},
	}
}

func backupDownloadCommand() *Command {
	var (
		wait    bool
		timeout time.Duration
	)

	return &Command{
		Name:    "download",
		Summary: "get a fetchable copy of a backup",
		Usage:   "backup download <id> [<env>] [--wait]",
		Long: `Two steps, because a backup is not a file anybody can reach. The
first call asks a machine to read the snapshot out of the encrypted repository
and write it somewhere fetchable; run it again once that has finished and it
answers with a link.

--wait does both: it queues the copy, follows the work, and prints the link when
it is ready. A database export can take a while, so the default wait is
generous.

The link lasts a day and is signed when you ask for it. It is never stored on
the task, because a link on a row is readable by everyone who can see the row
and keeps working after their access has been taken away.

The command prints the URL and does not fetch it. A database export is tens of
gigabytes, and what to do with it is yours to decide:

    curl -o dump.sql "$(vallic backup download a1b2c3d4 --wait --quiet)"`,
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&wait, "wait", false, "queue the copy, follow it, and print the link")
			fs.DurationVar(&timeout, "timeout", 2*time.Hour, "how long --wait waits")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			if len(args) == 0 {
				return Usagef(nil, "name a backup, as `vallic backup list` shows it")
			}

			snapshot := args[0]

			target, err := env.ResolveEnvironment(ctx, firstOf(args[1:]))
			if err != nil {
				return err
			}

			client, err := env.Client()
			if err != nil {
				return err
			}

			snapshot, err = resolveSnapshot(ctx, client, target.Environment.ID, snapshot)
			if err != nil {
				return err
			}

			download, err := client.DownloadBackup(ctx, target.Environment.ID, snapshot)
			if err != nil {
				return err
			}

			if download.Ready() {
				return reportDownload(env, download)
			}

			if !wait {
				if env.Printer.Structured() {
					return env.Printer.Value(download)
				}

				env.Printer.Good("Preparing %s (task %d)", download.Download.Filename, download.Download.Task.ID)
				env.Printer.Say("  ask again when it has finished, or pass --wait next time")
				env.Printer.Say("  follow it: vallic activity log %d", download.Download.Task.ID)

				return nil
			}

			env.Printer.Say("Preparing %s…", download.Download.Filename)

			if err := env.followTask(ctx, download.Download.Task.ID, timeout); err != nil {
				return err
			}

			// Asked again, because the URL is minted on the ask and there was
			// nothing to sign the first time.
			download, err = client.DownloadBackup(ctx, target.Environment.ID, snapshot)
			if err != nil {
				return err
			}

			if !download.Ready() {
				return fmt.Errorf(
					"the export finished and no link came back\n  try again: vallic backup download %s",
					snapshot,
				)
			}

			return reportDownload(env, download)
		},
	}
}

func backupRestoreCommand() *Command {
	var (
		kind string
		yes  bool
		wait bool
	)

	return &Command{
		Name:    "restore",
		Summary: "put a backup back over the environment it came from",
		Usage:   "backup restore <id|latest> [<env>] --kind database|files",
		Long: `Overwrites the environment with its own backup. This is the most
destructive thing in the whole CLI, and it is recovery rather than a copy: the
snapshot goes back over the environment it was taken from, and nowhere else.

Restoring production's data into staging is not reachable from here, on purpose.
It is a different act with a different threat model — real customer records in an
environment developers can reach is not what that data was collected under — and
the platform scrubs on the way in for environments that are not production.

` + "`latest`" + ` restores the newest backup of that kind.

On a protected environment this needs an owner, and it has to be confirmed by a
person at a browser. A personal access token cannot be asked to confirm, so a
pipeline cannot restore over protected production.

A restore gets one attempt. It rewrites the very thing a deploy would be
reading, and one that failed halfway has left the environment somewhere nobody
planned, so repeating it blindly makes that worse.`,
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&kind, "kind", "", "database or files")
			fs.BoolVar(&yes, "yes", false, "do not ask for confirmation")
			fs.BoolVar(&wait, "wait", false, "block until it finishes")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			if len(args) == 0 {
				return Usagef(nil, "name a backup, or `latest`")
			}

			chosen, err := backupKind(kind)
			if err != nil {
				return err
			}

			snapshot := args[0]

			// Whether the environment was named here, or worked out from the
			// branch. It decides whether --yes is enough on its own; see below.
			named := firstOf(args[1:])

			target, err := env.ResolveEnvironment(ctx, named)
			if err != nil {
				return err
			}

			client, err := env.Client()
			if err != nil {
				return err
			}

			if snapshot != "latest" {
				snapshot, err = resolveSnapshot(ctx, client, target.Environment.ID, snapshot)
				if err != nil {
					return err
				}
			}

			detail, err := env.Detail(ctx, target)
			if err != nil {
				return err
			}

			if needsAnExplicitTarget(yes, named, env.Interactive()) {
				return fmt.Errorf(
					"a restore run unattended has to name the environment it overwrites\n"+
						"  this one was worked out from the branch, which is not good enough for something that cannot be undone\n"+
						"  say it: vallic backup restore %s %s --kind %s --yes",
					shortOf(snapshot), detail.Name, chosen,
				)
			}

			if !yes {
				if !env.Interactive() {
					return fmt.Errorf(
						"restoring over %s overwrites its %s, and cannot be undone\n"+
							"  there is no terminal to confirm on, so pass --yes if you mean it",
						detail.Name, chosen,
					)
				}

				env.Printer.Warn("This overwrites %s's %s with backup %s.", detail.Name, chosen, shortOf(snapshot))
				env.Printer.Say("  it cannot be undone, and it gets one attempt")

				if detail.Type == "production" {
					env.Printer.Say("  %s is production", detail.Name)
				}

				if !env.confirm(fmt.Sprintf("Restore over %s?", detail.Name)) {
					return fmt.Errorf("cancelled")
				}
			}

			var queued *api.RestoreQueued

			// Wrapped, because a protected production environment demands that
			// the person asking prove who they are now. A token cannot, and
			// the error says so.
			err = env.WithStepUp(ctx, func() error {
				var callErr error
				queued, callErr = client.RestoreBackup(ctx, target.Environment.ID, snapshot, chosen)

				return callErr
			})
			if err != nil {
				if api.Code(err) == "reauthentication_impossible" {
					// The server says a person has to confirm. It cannot know
					// that the caller is a token, so the actionable half is
					// ours to add.
					return fmt.Errorf(
						"%w\n  run it from a terminal where you can sign in: vallic login",
						err,
					)
				}

				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(queued)
			}

			env.Printer.Good("Restoring %s over %s (task %d)",
				shortOf(queued.Restore.Snapshot), queued.Restore.Environment, queued.Restore.Task.ID)

			reportSanitisation(env, queued, detail)

			if !wait {
				env.Printer.Say("  follow it: vallic activity log %d", queued.Restore.Task.ID)

				return nil
			}

			return env.followTask(ctx, queued.Restore.Task.ID, 4*time.Hour)
		},
	}
}

// backupKind settles which half of a backup is meant.
func backupKind(value string) (string, error) {
	switch value {
	case "database", "files":
		return value, nil
	case "":
		// Not defaulted. A dump is minutes and a files sync is hours, so
		// picking one for somebody would sometimes pick the expensive one.
		return "", Usagef(nil, "--kind takes database or files")
	default:
		return "", Usagef(nil, "--kind takes database or files, not %q", value)
	}
}

// resolveSnapshot turns what somebody typed into a full snapshot id.
//
// The list prints short ids because a full one is sixty-four hex characters
// nobody retypes, so a prefix has to be accepted. An ambiguous prefix is
// refused rather than resolved to the first match: restoring the wrong night's
// database is not a mistake anybody recovers from by trying again.
func resolveSnapshot(ctx context.Context, client *api.Client, environmentID int, wanted string) (string, error) {
	listing, err := client.Backups(ctx, environmentID)
	if err != nil {
		return "", err
	}

	return matchSnapshot(listing.Snapshots, wanted)
}

// matchSnapshot finds the one snapshot a prefix names.
//
// Split out from the request so the ambiguity rule can be tested directly: it
// is the rule that stops `restore a1` picking a night at random, and a rule
// that is only exercised through an HTTP fake is a rule nobody checks.
func matchSnapshot(snapshots []api.Snapshot, wanted string) (string, error) {
	wanted = strings.ToLower(strings.TrimSpace(wanted))

	if wanted == "" {
		return "", fmt.Errorf("name a backup, as `vallic backup list` shows it")
	}

	var matches []api.Snapshot

	for _, snapshot := range snapshots {
		if snapshot.ID == wanted {
			// An exact id beats every prefix, so a full id is never ambiguous
			// even where another id happens to extend it.
			return snapshot.ID, nil
		}

		if strings.HasPrefix(snapshot.ID, wanted) {
			matches = append(matches, snapshot)
		}
	}

	switch len(matches) {
	case 1:
		return matches[0].ID, nil
	case 0:
		return "", fmt.Errorf(
			"no backup here starts with %s\n  `vallic backup list` shows what there is",
			wanted,
		)
	default:
		var ids []string
		for _, match := range matches {
			ids = append(ids, fmt.Sprintf("%s (%s, %s)", match.ShortID, match.Kind, ago(match.Taken)))
		}

		return "", fmt.Errorf(
			"%s matches %d backups, and picking one for you is not a mistake you could undo\n  %s",
			wanted, len(matches), strings.Join(ids, "\n  "),
		)
	}
}

// reportDownload prints the link, and only the link on stdout.
func reportDownload(env *Env, download *api.BackupDownload) error {
	if env.Printer.Structured() {
		return env.Printer.Value(download)
	}

	env.Printer.Say("%s, ready until %s", download.Download.Filename, until(download.Download.Expires))

	// Alone on stdout, so it can be captured into curl. Everything else this
	// command says goes to stderr.
	env.Printer.Line("%s", *download.Download.URL)

	return nil
}

// reportSanitisation says whether the data is scrubbed on the way in.
func reportSanitisation(env *Env, queued *api.RestoreQueued, detail *api.EnvironmentDetail) {
	if detail.Type == "production" {
		// Into production, the data goes back as it was. Nothing to scrub: it
		// is already where it was collected.
		return
	}

	count := queued.Restore.SanitisationCommands

	if count == 0 {
		// The case worth warning about. Real customer records are about to
		// land in an environment developers can reach, unscrubbed, because the
		// application declared nothing to scrub them with.
		env.Printer.Warn("  nothing will be scrubbed: this application declares no sanitisation commands")
		env.Printer.Say("  real data will land in %s as it is", detail.Name)

		return
	}

	env.Printer.Say("  %d sanitisation command(s) will run after the import", count)
}

// sortSnapshotsNewestFirst puts the most recent backup at the top.
//
// On `taken` rather than on `taken_at`, because the second is the repository's
// own string with an offset and nanoseconds in it, and comparing those as text
// orders two time zones wrongly. A snapshot whose timestamp the control plane
// could not read sorts last rather than first: it has no place in a
// chronology, and giving it one would push a real backup down the list.
func sortSnapshotsNewestFirst(snapshots []api.Snapshot) {
	sort.SliceStable(snapshots, func(i, j int) bool {
		left, right := snapshots[i].Taken, snapshots[j].Taken

		if left == nil || right == nil {
			return right == nil && left != nil
		}

		return *left > *right
	})
}

// listingAgeLines says how current the rows above are, and what the platform
// believes has happened since.
//
// Two questions rather than one, kept apart because the control plane keeps
// them apart: listed_at is when the repository was last read and the rows are
// only evidence of what it held then, while last_database and last_files are
// when a backup task reported success. Printing the first half alone was the
// bug. A scheduled backup the platform thinks ran and no listing has found is
// then invisible, and the gap between the two is what somebody comparing them
// came here for.
func listingAgeLines(listing *api.BackupListing) []string {
	unconfirmed := unconfirmedBackupLines(listing.Repository, listing.ListedAt)

	var lines []string

	switch {
	case listing.ListedAt != nil:
		lines = append(lines, fmt.Sprintf("Listed %s.", ago(listing.ListedAt)))
	case listing.Repository == nil:
		// Nothing has listed because there is nothing to list. Which of the
		// two states that is gets its own answer below, and neither of them is
		// about a stale listing.
	case len(unconfirmed) == 0:
		lines = append(lines, "Nothing has listed this repository yet, and no backup has been recorded since it was set up.")
	default:
		lines = append(lines, "Nothing has listed this repository yet.")
	}

	return append(lines, unconfirmed...)
}

// unconfirmedBackupLines names backups the platform recorded that no listing
// has found.
//
// Worded as a record rather than as a backup, because that is all it is: the
// timestamp is written when a backup task reports success, and only a listing
// proves there is a snapshot in the repository to restore from. Saying "a
// database backup ran 1h ago" would collapse the two, which is the confusion
// the control plane sends both halves to prevent.
func unconfirmedBackupLines(repository *api.BackupRepository, listedAt *int64) []string {
	if repository == nil {
		return nil
	}

	var lines []string

	for _, ran := range []struct {
		kind string
		at   *int64
	}{
		{"database", repository.LastDatabase},
		{"files", repository.LastFiles},
	} {
		if ran.at == nil || *ran.at == 0 {
			continue
		}

		// A listing taken in the same second as the backup it followed is a
		// listing that saw it, so equal is confirmed rather than suspect.
		if listedAt != nil && *ran.at <= *listedAt {
			continue
		}

		lines = append(lines, fmt.Sprintf(
			"The platform recorded a %s backup %s, and no listing has confirmed it.",
			ran.kind, ago(ran.at),
		))
	}

	return lines
}

// retentionLine says how long these backups live, in this environment's own
// numbers.
//
// Said here because this is where the numbers are. `backup create --help`
// stated thirty days as a fact, and the control plane sends manual_days per
// environment: the day a plan changes it, static help is a lie and nothing in
// a build would catch it. A line under the table rather than a column, because
// it is one answer for every row.
func retentionLine(retention map[string]int) string {
	var parts []string

	for _, unit := range []string{"hourly", "daily", "weekly", "monthly"} {
		if count := retention[unit]; count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count, unit))
		}
	}

	if hours := retention["within_hours"]; hours > 0 {
		parts = append(parts, fmt.Sprintf("everything from the last %dh", hours))
	}

	if last := retention["last"]; last > 0 {
		parts = append(parts, fmt.Sprintf("the last %d whatever their age", last))
	}

	manual := retention["manual_days"]

	byHand := fmt.Sprintf("%d days", manual)
	if manual == 1 {
		byHand = "a day"
	}

	switch {
	case len(parts) == 0 && manual <= 0:
		// Nothing, rather than "kept: nothing". A policy that keeps nothing is
		// how the backup tool is told to delete everything, and printing those
		// words beside a list of backups would read as exactly that.
		return ""
	case len(parts) == 0:
		return fmt.Sprintf("Kept: only what you take by hand, for %s.", byHand)
	case manual <= 0:
		return fmt.Sprintf("Kept: %s.", strings.Join(parts, ", "))
	default:
		return fmt.Sprintf("Kept: %s. One you take by hand is kept %s, whatever those counts say.",
			strings.Join(parts, ", "), byHand)
	}
}

// verificationLine answers "would these restore", and reports whether that is
// bad news.
//
// Three answers where there were two. Warning on a failed check and saying
// nothing when there has never been one leaves silence meaning both "fine" and
// "nobody has ever looked", and an untested backup is a hypothesis. The state
// that has never been checked is not a warning: nothing is known to be wrong,
// and a repository that has not come round to its first read-back is the
// ordinary state of a new one.
func verificationLine(repository *api.BackupRepository) (string, bool) {
	if repository.LastCheck == nil {
		return "Nothing has read this repository back yet, so nothing above is proved to restore.", false
	}

	if repository.CheckFailed {
		// The one of the three that is shouted, because every row above is
		// then a backup somebody tried to prove readable and could not.
		return fmt.Sprintf("The last integrity check on this repository failed, %s.", ago(repository.LastCheck)), true
	}

	return fmt.Sprintf("Read back %s, and it passed.", ago(repository.LastCheck)), false
}

// destinationLine says what kind of storage these sit in and whether the key
// that decrypts them can ever be handed over, and reports whether the
// destination has gone.
//
// The key half is the part worth printing. It is refused outright for a team
// that has brought no storage of its own, so somebody hunting for a key there
// is hunting for something that does not exist for them, and nothing else the
// CLI prints would say so. The location stays out of it: where the bucket is
// and what it is called is the platform's business, while the kind is what
// tells a customer their data is in object storage rather than on a disk.
func destinationLine(repository *api.BackupRepository) (string, bool) {
	if repository.DestinationKind == nil {
		return "The destination this repository was created against has been removed, so the platform cannot say where these sit.", true
	}

	if repository.OwnDestination {
		return fmt.Sprintf("Written to %s. This team has storage of its own, so the repository key can be read in the console.",
			*repository.DestinationKind), false
	}

	return fmt.Sprintf("Written to %s. This team has no storage of its own, so the repository key can never be shown.",
		*repository.DestinationKind), false
}

// keptFor says how a backup's retention is decided.
func keptFor(snapshot *api.Snapshot) string {
	if snapshot.Manual {
		// Held out of the nightly expiry pass, then expired on its own, so a
		// backup taken before a risky change does not become permanent.
		return "manual"
	}

	return "scheduled"
}

// exportState says whether a copy of a backup can be fetched.
func exportState(snapshot *api.Snapshot) string {
	if snapshot.Kind == "unknown" {
		// Not "no". The platform did not take this one, so it cannot say what
		// is in it, and it will refuse rather than guess.
		return "unavailable"
	}

	if snapshot.ExportTask != nil {
		return "prepared"
	}

	return "on request"
}

// shortOf abbreviates a snapshot id for a sentence.
func shortOf(snapshot string) string {
	if snapshot == "latest" {
		return snapshot
	}

	if len(snapshot) > 8 {
		return snapshot[:8]
	}

	return snapshot
}

// until renders when a link stops working.
func until(at *int64) string {
	if at == nil || *at == 0 {
		return "an unstated time"
	}

	left := time.Until(time.Unix(*at, 0))

	if left <= 0 {
		return "now (it has expired)"
	}

	return duration(int64(left.Seconds())) + " from now"
}

// needsAnExplicitTarget reports whether an unattended restore must name its
// environment rather than inherit it.
//
// Inheriting the environment from the current branch is right for a deploy,
// where the worst case is a release that can be deployed again. It is wrong
// here: a restore cannot be undone, and a pipeline that had checked out a
// different branch would overwrite a different site. So --yes on its own is
// not enough when nobody is watching and nobody said which site.
func needsAnExplicitTarget(yes bool, named string, interactive bool) bool {
	return yes && named == "" && !interactive
}
