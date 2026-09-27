package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/vallic/vallic-cli/internal/api"
)

func snapshots() []api.Snapshot {
	return []api.Snapshot{
		{ID: "a1b2c3d4e5f6", ShortID: "a1b2c3d4", Kind: "database"},
		{ID: "a1b2ffffffff", ShortID: "a1b2ffff", Kind: "files"},
		{ID: "9988776655", ShortID: "99887766", Kind: "database"},
	}
}

// The rule that stops `backup restore a1` picking a night at random. Restoring
// the wrong night's database is not a mistake anybody recovers from by trying
// again, so an ambiguous prefix has to be refused rather than resolved.
func TestMatchSnapshotRefusesAnAmbiguousPrefix(t *testing.T) {
	_, err := matchSnapshot(snapshots(), "a1b2")
	if err == nil {
		t.Fatal(`matchSnapshot("a1b2") = nil error, want a refusal`)
	}

	// Both candidates named, with their kind, so somebody can pick.
	for _, want := range []string{"a1b2c3d4", "a1b2ffff", "database", "files"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

func TestMatchSnapshotResolvesAnUnambiguousPrefix(t *testing.T) {
	got, err := matchSnapshot(snapshots(), "a1b2c3")
	if err != nil {
		t.Fatal(err)
	}

	if got != "a1b2c3d4e5f6" {
		t.Errorf("matchSnapshot() = %q, want the full id", got)
	}
}

// A full id must resolve even where another id extends it, or somebody who
// pasted a complete id would be told it was ambiguous.
func TestMatchSnapshotPrefersAnExactIdOverAPrefix(t *testing.T) {
	list := []api.Snapshot{
		{ID: "abc", ShortID: "abc", Kind: "database"},
		{ID: "abcdef", ShortID: "abcdef", Kind: "files"},
	}

	got, err := matchSnapshot(list, "abc")
	if err != nil {
		t.Fatalf("matchSnapshot() = %v, want the exact match to win", err)
	}

	if got != "abc" {
		t.Errorf("matchSnapshot() = %q, want %q", got, "abc")
	}
}

func TestMatchSnapshotSaysWhenNothingMatches(t *testing.T) {
	_, err := matchSnapshot(snapshots(), "ffff")
	if err == nil {
		t.Fatal("matchSnapshot() = nil error, want a refusal")
	}

	if !strings.Contains(err.Error(), "backup list") {
		t.Errorf("error = %q, want it to say how to see what there is", err)
	}
}

// Neither kind is defaulted: a dump is minutes and a files sync can be hours,
// so picking one for somebody would sometimes pick the expensive one.
func TestBackupKindIsNeverGuessed(t *testing.T) {
	for _, kind := range []string{"database", "files"} {
		if got, err := backupKind(kind); err != nil || got != kind {
			t.Errorf("backupKind(%q) = %q, %v", kind, got, err)
		}
	}

	for _, bad := range []string{"", "Database", "everything"} {
		if _, err := backupKind(bad); err == nil {
			t.Errorf("backupKind(%q) = nil error, want a refusal", bad)
		}
	}
}

// --auto-deploy has to be able to say false. A bool flag cannot: its absence
// and its false are the same thing, and a patch must tell "leave this alone"
// from "turn it off".
func TestParseBoolCanSayFalse(t *testing.T) {
	for _, yes := range []string{"true", "TRUE", "yes", "on", "1"} {
		got, err := parseBool(yes)
		if err != nil || !got {
			t.Errorf("parseBool(%q) = %v, %v", yes, got, err)
		}
	}

	for _, no := range []string{"false", "FALSE", "no", "off", "0"} {
		got, err := parseBool(no)
		if err != nil || got {
			t.Errorf("parseBool(%q) = %v, %v", no, got, err)
		}
	}

	if _, err := parseBool("maybe"); err == nil {
		t.Error(`parseBool("maybe") = nil error, want a refusal`)
	}
}

// A restore cannot be undone, so an unattended one has to say what it is
// overwriting. Inheriting the environment from the branch is fine for a
// deploy and not for this.
func TestAnUnattendedRestoreMustNameItsEnvironment(t *testing.T) {
	cases := []struct {
		name        string
		yes         bool
		named       string
		interactive bool
		want        bool
	}{
		{
			name: "unattended, --yes, target inherited from the branch",
			yes:  true, named: "", interactive: false,
			want: true,
		},
		{
			name: "unattended, --yes, environment named on the line",
			yes:  true, named: "staging", interactive: false,
			want: false,
		},
		{
			// A person at a terminal is asked to confirm, and the prompt says
			// which environment. Nothing is inherited silently.
			name: "a terminal, so there is a prompt naming it",
			yes:  true, named: "", interactive: true,
			want: false,
		},
		{
			// Without --yes the ordinary confirmation path runs, which refuses
			// on its own when there is no terminal.
			name: "unattended without --yes falls to the confirmation refusal",
			yes:  false, named: "", interactive: false,
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsAnExplicitTarget(tc.yes, tc.named, tc.interactive); got != tc.want {
				t.Errorf("needsAnExplicitTarget(%v, %q, %v) = %v, want %v",
					tc.yes, tc.named, tc.interactive, got, tc.want)
			}
		})
	}
}

// backupMinutesAgo is a timestamp that far back, for the lines that compare
// one against another.
func backupMinutesAgo(minutes int64) *int64 {
	at := time.Now().Unix() - minutes*60

	return &at
}

// The numbers come from the control plane per environment, and `backup create
// --help` used to state thirty days as a fact. If this stops printing them,
// the only place the CLI says how long a backup lives is a sentence that is
// wrong the day a plan changes the number.
func TestRetentionLineUsesTheEnvironmentsOwnNumbers(t *testing.T) {
	cases := []struct {
		name      string
		retention map[string]int
		want      string
	}{
		{
			name:      "production, nothing bought",
			retention: map[string]int{"daily": 7, "weekly": 4, "monthly": 12, "manual_days": 30},
			want:      "Kept: 7 daily, 4 weekly, 12 monthly. One you take by hand is kept 30 days, whatever those counts say.",
		},
		{
			// An intraday cadence buys hourly buckets. They are counted in
			// runs, so a six-a-day schedule is forty-two of them for a week.
			name:      "production, four-hourly database",
			retention: map[string]int{"hourly": 42, "daily": 7, "weekly": 4, "monthly": 12, "manual_days": 30},
			want:      "Kept: 42 hourly, 7 daily, 4 weekly, 12 monthly. One you take by hand is kept 30 days, whatever those counts say.",
		},
		{
			name:      "staging",
			retention: map[string]int{"daily": 3, "manual_days": 30},
			want:      "Kept: 3 daily. One you take by hand is kept 30 days, whatever those counts say.",
		},
		{
			// A window and a floor rather than counts. Nothing is taken by
			// hand here, so the manual sentence has to go rather than say zero.
			name:      "measured rather than counted",
			retention: map[string]int{"within_hours": 48, "last": 3},
			want:      "Kept: everything from the last 48h, the last 3 whatever their age.",
		},
		{
			name:      "one day by hand",
			retention: map[string]int{"daily": 1, "manual_days": 1},
			want:      "Kept: 1 daily. One you take by hand is kept a day, whatever those counts say.",
		},
		{
			// Nothing at all, rather than "kept: nothing" beside a list of
			// backups: that is how the backup tool is told to delete them.
			name:      "a policy that keeps nothing says nothing",
			retention: map[string]int{},
			want:      "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retentionLine(tc.retention); got != tc.want {
				t.Errorf("retentionLine() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The control plane sends when it believes a backup ran and when the
// repository was last read, and the gap between them is what somebody is
// checking for. If this stops naming that gap, a scheduled backup nothing has
// found in the repository looks exactly like one that never ran.
func TestUnconfirmedBackupsAreNamedWhenNoListingHasSeenThem(t *testing.T) {
	cases := []struct {
		name       string
		repository *api.BackupRepository
		listedAt   *int64
		want       []string
	}{
		{
			name:       "the listing is newer than both backups",
			repository: &api.BackupRepository{LastDatabase: backupMinutesAgo(120), LastFiles: backupMinutesAgo(180)},
			listedAt:   backupMinutesAgo(60),
			want:       nil,
		},
		{
			name:       "a database backup ran after the last listing",
			repository: &api.BackupRepository{LastDatabase: backupMinutesAgo(30), LastFiles: backupMinutesAgo(180)},
			listedAt:   backupMinutesAgo(60),
			want:       []string{"database"},
		},
		{
			name:       "both ran after the last listing",
			repository: &api.BackupRepository{LastDatabase: backupMinutesAgo(30), LastFiles: backupMinutesAgo(40)},
			listedAt:   backupMinutesAgo(60),
			want:       []string{"database", "files"},
		},
		{
			// Nothing has ever listed, so nothing has confirmed either of them.
			name:       "no listing at all",
			repository: &api.BackupRepository{LastDatabase: backupMinutesAgo(30)},
			listedAt:   nil,
			want:       []string{"database"},
		},
		{
			name:       "nothing has ever run",
			repository: &api.BackupRepository{},
			listedAt:   nil,
			want:       nil,
		},
		{
			name:       "no repository to have run anything",
			repository: nil,
			listedAt:   nil,
			want:       nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unconfirmedBackupLines(tc.repository, tc.listedAt)

			if len(got) != len(tc.want) {
				t.Fatalf("unconfirmedBackupLines() = %q, want %d line(s)", got, len(tc.want))
			}

			for i, kind := range tc.want {
				if !strings.Contains(got[i], kind) {
					t.Errorf("line %d = %q, want it to name the %s backup", i, got[i], kind)
				}

				if !strings.Contains(got[i], "no listing has confirmed it") {
					t.Errorf("line %d = %q, want it to say the listing has not confirmed it", i, got[i])
				}
			}
		})
	}
}

// An environment whose repository is NULL is in one of two states, and they
// are opposite answers. If this collapses them again, a team with nowhere to
// write a backup is told to wait for one that cannot arrive.
func TestListingAgeSaysWhatANeverListedRepositoryMeans(t *testing.T) {
	cases := []struct {
		name    string
		listing *api.BackupListing
		want    []string
	}{
		{
			name: "listed, and nothing has run since",
			listing: &api.BackupListing{
				Repository: &api.BackupRepository{LastDatabase: backupMinutesAgo(180)},
				ListedAt:   backupMinutesAgo(60),
			},
			want: []string{"Listed "},
		},
		{
			// The sentence that was the only one printed for every case.
			name: "a repository nothing has listed and nothing has run against",
			listing: &api.BackupListing{
				Repository: &api.BackupRepository{},
			},
			want: []string{"no backup has been recorded since it was set up"},
		},
		{
			// Same repository, and the sentence above would be false: the
			// platform recorded a backup, and only the listing is missing.
			name: "a repository nothing has listed, with a backup recorded",
			listing: &api.BackupListing{
				Repository: &api.BackupRepository{LastFiles: backupMinutesAgo(30)},
			},
			want: []string{"Nothing has listed this repository yet.", "files"},
		},
		{
			// No repository at all. Staleness is not the answer, and saying
			// anything about listings here would talk past what is wrong.
			name:    "no repository",
			listing: &api.BackupListing{},
			want:    nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := listingAgeLines(tc.listing)

			if len(got) != len(tc.want) {
				t.Fatalf("listingAgeLines() = %q, want %d line(s)", got, len(tc.want))
			}

			for i, want := range tc.want {
				if !strings.Contains(got[i], want) {
					t.Errorf("line %d = %q, want it to contain %q", i, got[i], want)
				}
			}
		})
	}
}

// "Nobody has ever proved these restore" and "we proved them and it failed"
// are different answers. If the first one goes quiet again, silence means both
// "fine" and "nobody has looked", and an untested backup is a hypothesis.
func TestVerificationSeparatesNeverCheckedFromFailed(t *testing.T) {
	cases := []struct {
		name       string
		repository *api.BackupRepository
		want       string
		wantWarn   bool
	}{
		{
			name:       "nobody has ever read it back",
			repository: &api.BackupRepository{},
			want:       "Nothing has read this repository back yet",
			wantWarn:   false,
		},
		{
			name:       "read back, and it failed",
			repository: &api.BackupRepository{LastCheck: backupMinutesAgo(60), CheckFailed: true},
			want:       "failed",
			wantWarn:   true,
		},
		{
			name:       "read back, and it passed",
			repository: &api.BackupRepository{LastCheck: backupMinutesAgo(60)},
			want:       "it passed",
			wantWarn:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warn := verificationLine(tc.repository)

			if !strings.Contains(got, tc.want) {
				t.Errorf("verificationLine() = %q, want it to contain %q", got, tc.want)
			}

			if warn != tc.wantWarn {
				t.Errorf("verificationLine() warn = %v, want %v", warn, tc.wantWarn)
			}
		})
	}
}

// The key is refused outright for a team that has brought no storage, so
// somebody looking for it there is looking for something that does not exist
// for them. If this stops saying so, they find that out from a third person.
func TestDestinationSaysWhetherTheKeyCanEverBeShown(t *testing.T) {
	s3 := "s3"

	cases := []struct {
		name       string
		repository *api.BackupRepository
		want       []string
		wantWarn   bool
	}{
		{
			name:       "the team brought the storage",
			repository: &api.BackupRepository{DestinationKind: &s3, OwnDestination: true},
			want:       []string{"s3", "can be read in the console"},
			wantWarn:   false,
		},
		{
			name:       "the platform's shared storage",
			repository: &api.BackupRepository{DestinationKind: &s3},
			want:       []string{"s3", "can never be shown"},
			wantWarn:   false,
		},
		{
			// The destination row has been removed from under the repository,
			// which is a state worth being able to see rather than a blank.
			name:       "the destination has gone",
			repository: &api.BackupRepository{OwnDestination: true},
			want:       []string{"has been removed"},
			wantWarn:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warn := destinationLine(tc.repository)

			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("destinationLine() = %q, want it to contain %q", got, want)
				}
			}

			if warn != tc.wantWarn {
				t.Errorf("destinationLine() warn = %v, want %v", warn, tc.wantWarn)
			}
		})
	}
}
