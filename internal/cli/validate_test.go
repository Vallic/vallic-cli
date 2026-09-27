package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vallic/vallic-cli/internal/api"
)

// The one line in this output that is not in the repository. A schedule the
// platform added because the file declared none reads exactly like one
// somebody wrote, and a client that could not tell offers to edit a line that
// does not exist: somebody then opens the file, cannot find it, and concludes
// the platform is running something it will not show them.
func TestCronLinesMarkTheScheduleThePlatformAdded(t *testing.T) {
	const marker = "the platform's own"

	cases := []struct {
		name       string
		jobs       []api.CronJob
		wantLines  int
		wantMarked []bool
	}{
		{
			name: "a file that schedules its own",
			jobs: []api.CronJob{
				{Name: "cron", Schedule: "*/15 * * * *", Command: "drush cron"},
				{Name: "prune", Schedule: "0 3 * * *", Command: "./prune.sh"},
			},
			wantLines:  2,
			wantMarked: []bool{false, false},
		},
		{
			name:       "a file that schedules nothing",
			jobs:       []api.CronJob{{Name: "cron", Schedule: "0 * * * *", Command: "drush cron", Default: true}},
			wantLines:  1,
			wantMarked: []bool{true},
		},
		{
			name:       "nothing scheduled at all",
			jobs:       nil,
			wantLines:  0,
			wantMarked: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := cronLines(tc.jobs)

			if len(lines) != tc.wantLines {
				t.Fatalf("cronLines() = %d lines, want %d", len(lines), tc.wantLines)
			}

			for i, line := range lines {
				if marked := strings.Contains(line, marker); marked != tc.wantMarked[i] {
					t.Errorf("line %d = %q, marked %v, want %v", i, line, marked, tc.wantMarked[i])
				}

				// The name, the schedule and the command are all on the line:
				// a marker on a row nobody can identify is no help.
				for _, want := range []string{tc.jobs[i].Name, tc.jobs[i].Schedule, tc.jobs[i].Command} {
					if !strings.Contains(line, want) {
						t.Errorf("line %d = %q, want it to carry %q", i, line, want)
					}
				}
			}
		})
	}
}

// What is absent from the echo is what the control plane did not read, which
// is the question somebody echoing their own file is asking. If a key with
// nothing behind it starts printing again, the four keys that landed are
// answered with eleven dashes around them and the answer is lost in them.
func TestManifestEntriesLeaveOutWhatWasNotRead(t *testing.T) {
	cases := []struct {
		name     string
		manifest api.Manifest
		want     []manifestEntry
	}{
		{
			// The parser's own defaults, which is what every key of a file
			// that sets almost nothing comes back as.
			name:     "a file with nothing in it but its type",
			manifest: api.Manifest{Type: "drupal", Runtime: map[string]string{}, Services: []api.ManifestService{}},
			want:     []manifestEntry{{Key: "type", Value: "drupal"}},
		},
		{
			name: "the keys a file usually has",
			manifest: api.Manifest{
				Type: "drupal",
				// Out of order on purpose: a map answers in whatever order Go
				// felt like, and two runs over one file have to agree.
				Runtime:  map[string]string{"php": "8.3", "memory_limit": "512M"},
				Port:     8080,
				Services: []api.ManifestService{{Name: "mariadb", Version: "10.11"}, {Name: "solr", Version: "9", Environment: map[string]string{"SOLR_HEAP": "1024m"}}},
				Workers:  []api.Worker{{Name: "queue", Replicas: 2}, {Name: "mail", Replicas: 1}},
				BuildSteps: []api.BuildStep{
					{Name: "composer"},
					{Name: "assets", Image: "node"},
				},
				BuildCache:        []string{"vendor"},
				DeploySteps:       []string{"drush updb -y"},
				RollbackOnFailure: true,
				HealthPath:        "/up",
				HealthTimeout:     30,
				RequiredEnv:       []string{"STRIPE_KEY"},
				Mounts:            []string{"public/files"},
			},
			want: []manifestEntry{
				{Key: "type", Value: "drupal"},
				{Key: "runtime", Value: "memory_limit 512M, php 8.3"},
				{Key: "port", Value: "8080"},
				{Key: "services", Value: "mariadb 10.11, solr 9 (SOLR_HEAP)"},
				{Key: "workers", Value: "queue ×2, mail"},
				{Key: "build steps", Value: "composer, assets (in node)"},
				{Key: "build cache", Value: "vendor"},
				{Key: "deploy steps", Value: "drush updb -y"},
				{Key: "rollback", Value: "on a failed deploy"},
				{Key: "health", Value: "/up, 30s"},
				{Key: "required env", Value: "STRIPE_KEY"},
				{Key: "mounts", Value: "public/files"},
			},
		},
		{
			// Off is what the platform does with a file that never mentioned
			// it, so there is no line to print. "rollback: no" would read as
			// something somebody chose.
			name:     "a deploy that is not rolled back",
			manifest: api.Manifest{Type: "nextjs", Start: "node server.js", RollbackOnFailure: false},
			want: []manifestEntry{
				{Key: "type", Value: "nextjs"},
				{Key: "start", Value: "node server.js"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := manifestEntries(&tc.manifest)

			if len(got) != len(tc.want) {
				t.Fatalf("manifestEntries() = %v, want %v", got, tc.want)
			}

			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("entry %d = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// Naming a build step is optional and most files skip it, so rendering the
// name alone made the whole build line empty, and an empty value is dropped
// from the summary entirely. A manifest that appeared to declare no build is
// the single worst thing this output could get wrong: a deploy without
// `composer install` is a site missing its dependencies.
func TestAnUnnamedBuildStepIsShownByItsCommand(t *testing.T) {
	steps := []api.BuildStep{{Command: "composer install --no-dev"}}

	if got := manifestBuildSteps(steps); got != "composer install --no-dev" {
		t.Errorf("manifestBuildSteps() = %q, want the command", got)
	}

	// A name still wins where there is one, because somebody wrote it to be
	// read.
	named := []api.BuildStep{{Name: "PHP dependencies", Command: "composer install"}}

	if got := manifestBuildSteps(named); got != "PHP dependencies" {
		t.Errorf("manifestBuildSteps() = %q, want the name", got)
	}

	// And the image still travels, because a step running somewhere the
	// repository's tools are not is a build failure with no obvious cause.
	elsewhere := []api.BuildStep{{Command: "npm ci", Image: "node:22"}}

	if got := manifestBuildSteps(elsewhere); got != "npm ci (in node:22)" {
		t.Errorf("manifestBuildSteps() = %q, want the image named", got)
	}
}

// An extra that landed must appear, because the line above this output says
// every key that landed is in it.
//
// It did not until [2026-09-24]: the control plane parsed `extra:`, accepted
// it, and echoed a manifest with no mention of it, so a file declaring a
// machine was validated as though it had declared nothing. Asserted on the
// rendering rather than the field so that adding the field without printing it
// still fails -- which is the shape the build-step bug took.
func TestAnExtraThatLandedIsPrinted(t *testing.T) {
	entries := manifestEntries(&api.Manifest{
		Type: "drupal",
		Extras: []api.ManifestExtra{{
			Name: "pioneer",
			Expose: api.ExposeMap{
				"thumbnailer": {{Listen: 8000, Container: 8000}},
				"metrics":     {{Listen: 8001, Container: 9090}},
			},
			InternetEgress: true,
		}},
	})

	var got string

	for _, entry := range entries {
		if entry.Key == "extras" {
			got = entry.Value
		}
	}

	if got == "" {
		t.Fatal("the manifest declared an extra and nothing was printed")
	}

	// Sorted, so the order is the same in every run and in every diff.
	want := "pioneer (metrics 8001→9090, thumbnailer 8000), internet egress"

	if got != want {
		t.Errorf("extras = %q, want %q", got, want)
	}
}

// An extra exposing nothing is a reported problem, not a rejected document, so
// the manifest is still echoed -- and PHP renders that empty map as `[]`. The
// whole response used to fail to parse on exactly this shape, which is worse
// than the mistake it describes: the reader loses the problem that explains it.
func TestAnExtraExposingNothingStillParses(t *testing.T) {
	var manifest api.Manifest

	body := `{"type":"drupal","extras":[{"name":"pioneer","expose":[],"internet_egress":false}]}`

	if err := json.Unmarshal([]byte(body), &manifest); err != nil {
		t.Fatalf("unmarshal: %v, want PHP's empty array to be tolerated", err)
	}

	if len(manifest.Extras) != 1 || manifest.Extras[0].Name != "pioneer" {
		t.Fatalf("extras = %+v, want the one slot", manifest.Extras)
	}

	if manifest.Extras[0].Expose == nil {
		t.Error("expose is nil, want an empty map that ranges without a check")
	}

	entries := manifestEntries(&manifest)

	for _, entry := range entries {
		if entry.Key == "extras" && entry.Value != "pioneer" {
			t.Errorf("extras = %q, want the slot named with no port list", entry.Value)
		}
	}
}
