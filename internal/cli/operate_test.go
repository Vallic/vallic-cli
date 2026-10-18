package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vallic/vallic-cli/internal/api"
)

// Upsun's spelling reaches the same command, so somebody coming from it can
// type what their fingers know.
func TestUpsunsSpellingIsReadAsTheWordsItStandsFor(t *testing.T) {
	root := Root()

	cases := map[string][]string{
		"db:dump":          {"db", "dump"},
		"environment:list": {"environment", "list"},
		"mount:upload":     {"mount", "upload"},
		"ssh-key:add":      {"ssh-key", "add"},
		"self:update":      {"self-update"},
		"db:sql":           {"sql"},
		"user:add":         {"team", "invite"},
		"route:list":       {"url", "--all"},
	}

	for typed, want := range cases {
		got, ok := colonSpelling(root, typed)
		if !ok || strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("colonSpelling(%q) = %q, %v; want %q", typed, got, ok, want)
		}
	}

	if got, ok := colonSpelling(root, "certificate:add"); ok {
		t.Errorf("colonSpelling() made %q of a command with no equivalent", got)
	}

	// Through the dispatcher: the colon form reaches the command, which
	// refuses what it is given in its own words.
	env, _, _ := followEnv(t)
	err := Execute(context.Background(), root, env, []string{"db:dump", "--stdout", "--file", "x.sql"})

	if err == nil || !strings.Contains(err.Error(), "--stdout writes no file") {
		t.Errorf("db:dump reached %v, want db dump's own refusal", err)
	}
}

// Now, the average and the peak, with a step that has no disk reading left
// out of the disk's figures rather than counted as an empty disk.
func TestMetricsAreSummarisedWithoutInventingReadings(t *testing.T) {
	disk := 2.0e9

	summary := summarise([]api.ResourcePoint{
		{CPUPercent: 10, MemoryBytes: 100, DiskBytes: &disk},
		{CPUPercent: 50, MemoryBytes: 300},
		{CPUPercent: 30, MemoryBytes: 200},
	})

	if summary.cpu.latest != 30 || summary.cpu.peak != 50 || summary.cpu.average != 30 {
		t.Errorf("cpu = %+v", summary.cpu)
	}

	if summary.memory.average != 200 || summary.memory.peak != 300 {
		t.Errorf("memory = %+v", summary.memory)
	}

	if summary.disk.count != 1 || summary.disk.latest != disk {
		t.Errorf("disk = %+v, want the one reading there was", summary.disk)
	}

	if diskOrNone(series{}, 0) != "-" {
		t.Error("no disk readings printed as a size")
	}
}

// An empty --secret is none of them; no --secret is the control plane's guess.
func TestSecretNamesKnowWhetherTheyWereGiven(t *testing.T) {
	var none namesFlag
	none.Set("")

	if !none.given || len(none.names) != 0 {
		t.Errorf("an empty --secret = %+v, want given and empty", none)
	}

	var some namesFlag
	some.Set("API_TOKEN, DB_PASSWORD")

	if strings.Join(some.names, " ") != "API_TOKEN DB_PASSWORD" {
		t.Errorf("--secret = %q", some.names)
	}
}

// Putting a site behind its offline page is what visitors see, so it is not
// done without a yes — and with no terminal, the refusal says how to give one.
func TestMaintenanceAsksBeforeTheOfflinePage(t *testing.T) {
	run := envMaintenanceCommand().Run

	for _, args := range [][]string{{}, {"maybe"}, {"on", "staging", "extra"}} {
		var usage *UsageError
		if err := run(context.Background(), nil, args); !errors.As(err, &usage) {
			t.Errorf("%q = %v, want a usage error before anything is asked", args, err)
		}
	}
}
