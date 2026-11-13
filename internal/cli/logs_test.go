package cli

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/vallic/vallic-cli/internal/api"
	vssh "github.com/vallic/vallic-cli/internal/ssh"
)

func TestLogsNamesTheDestinationAndTheMachinesCopy(t *testing.T) {
	url := "https://console.vallic.com/abc123/projects/acme/configuration"
	report := strings.Join(logsReport(&api.Logs{
		Environment: "acme-staging",
		Destination: &api.LogDestination{Kind: "loki", KindLabel: "Grafana Loki", Name: "Team Loki"},
		MachineDays: 7,
		SettingsURL: &url,
	}), "\n")

	for _, want := range []string{
		"Where acme-staging's logs are:",
		"Sent to:        Team Loki (Grafana Loki)",
		"the last 7 days — read them with `vallic logs`",
		"Change it:      " + url,
		"vallic activity",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the report lacks %q:\n%s", want, report)
		}
	}
}

func TestADestinationThatCannotSendIsNotReportedAsOne(t *testing.T) {
	report := strings.Join(logsReport(&api.Logs{
		Environment: "acme-staging",
		Unusable:    &api.LogDestination{Kind: "loki", KindLabel: "Grafana Loki", Name: "Team Loki"},
	}), "\n")

	if !strings.Contains(report, "nowhere — Team Loki (Grafana Loki) is chosen, but it is switched off") {
		t.Errorf("an unusable destination was not said to be one:\n%s", report)
	}
}

func TestNoDestinationSaysSo(t *testing.T) {
	report := strings.Join(logsReport(&api.Logs{Environment: "acme-staging"}), "\n")

	if !strings.Contains(report, "nowhere — this project has no log destination") {
		t.Errorf("no destination was not said:\n%s", report)
	}
	if strings.Contains(report, "Change it:") {
		t.Errorf("a settings link was printed where there is none:\n%s", report)
	}
}

func TestTheLogsAnswerDecodesAsTheControlPlaneSendsIt(t *testing.T) {
	var answer api.Logs
	body := `{"environment":"acme-staging","destination":{"kind":"datadog","kind_label":"Datadog","name":"DD"},"unusable":null,"machine_days":7,"settings_url":null}`

	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Destination == nil || answer.Destination.KindLabel != "Datadog" || answer.MachineDays != 7 || answer.SettingsURL != nil {
		t.Errorf("decoded as %+v", answer)
	}
}

// What was asked for travels as words the machine takes apart on spaces.
func TestLogsRequestIsSpelledAsTheMachineReadsIt(t *testing.T) {
	for want, request := range map[string]logsRequest{
		// Nothing said about how much: the end of it.
		"vallic-logs --tail 100":                         {Tail: -1},
		"vallic-logs --tail 10 --follow":                 {Tail: -1, Follow: true},
		"vallic-logs --since 1h":                         {Tail: -1, Since: "1h"},
		"vallic-logs --since 1h --tail 5 --follow":       {Tail: 5, Since: "1h", Follow: true},
		"vallic-logs --since 2d --until 1d --json":       {Tail: -1, Since: "2d", Until: "1d", JSON: true},
		"vallic-logs --tail 100 --source proxy,app":      {Tail: -1, Sources: []string{"proxy", "app"}},
		"vallic-logs --since 2026-11-13T10:00 --tail 50": {Tail: 50, Since: "2026-11-13T10:00"},
	} {
		if got := request.verb("vallic-logs"); got != want {
			t.Errorf("%+v is spelled %q, want %q", request, got, want)
		}
	}
}

// What cannot be sent as one word, or does not go together, is said here,
// naming the flag, before a connection is opened for it.
func TestLogsRequestThatCannotBeSentIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		request logsRequest
		output  string
		want    string
	}{
		"a kind there is not":   {logsRequest{Sources: []string{"database"}}, "", "--source takes"},
		"a moment in two words": {logsRequest{Since: "2026-11-13 10:00"}, "", "written as one word"},
		"a command in a moment": {logsRequest{Until: "1h;reboot"}, "", "written as one word"},
		"a tail with an end":    {logsRequest{Follow: true, Until: "1h"}, "", "does not go with --until"},
		"a tail into a file":    {logsRequest{Follow: true}, "out.log", "redirect it instead"},
	} {
		err := tc.request.check(tc.output)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: answered %v, want a refusal saying %q", name, err, tc.want)
		}
	}

	if err := (logsRequest{Since: "2026-11-13T10:00:00Z", Until: "15m", Sources: []string{"proxy-error"}}).check("out.log.gz"); err != nil {
		t.Errorf("a request that is fine was refused: %v", err)
	}
}

// An environment on several machines is read on all of them unless one is
// named, and one machine is read as it is.
func TestLogsAreReadOnEveryMachine(t *testing.T) {
	several := &vssh.Target{SSHTarget: api.SSHTarget{
		Host: "acme.vallic.cloud", Port: 2417, User: "vc-acme",
		Machines: []api.SSHMachine{{Name: "front", Role: "proxy"}, {Name: "web-1", Role: "web"}, {Name: "web-2", Role: "web"}},
	}}

	streams, err := logStreams(several, "", "vallic-logs --tail 100")
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) != 3 || streams[1].name != "web-1" {
		t.Fatalf("read %d machines: %+v", len(streams), streams)
	}
	for _, stream := range streams {
		command := vssh.Describe(stream.cmd)
		if !strings.Contains(command, stream.name+".acme.vallic.cloud") || !strings.Contains(command, "vallic-logs --tail 100") {
			t.Errorf("%s is read with %s", stream.name, command)
		}
	}

	one, err := logStreams(several, "web-2", "vallic-logs")
	if err != nil || len(one) != 1 || one[0].name != "" || !strings.Contains(vssh.Describe(one[0].cmd), "web-2.acme.vallic.cloud") {
		t.Errorf("naming a machine read %+v, %v", one, err)
	}

	if _, err := logStreams(several, "web-9", "vallic-logs"); err == nil {
		t.Error("a machine the environment does not have was read")
	}

	single := &vssh.Target{SSHTarget: api.SSHTarget{Host: "acme.vallic.cloud", Port: 2417, User: "vc-acme"}}
	if streams, err := logStreams(single, "", "vallic-logs"); err != nil || len(streams) != 1 || streams[0].name != "" {
		t.Errorf("one machine read as %+v, %v", streams, err)
	}
}

// Several machines' lines come out in order of time, each named for its
// machine; one machine's come out as they are.
func TestSeveralMachinesAreReadInOrderOfTime(t *testing.T) {
	script := func(lines ...string) *exec.Cmd {
		return exec.Command("printf", "%s\n", strings.Join(lines, "\n"))
	}

	streams := []logStream{
		{name: "front", cmd: script(
			"2026-11-13T10:00:01.000Z [proxy acme] one",
			"2026-11-13T10:00:04.000Z [proxy acme] four",
		)},
		{name: "web-1", cmd: script(
			"2026-11-13T10:00:02.000Z [container php] two",
			"2026-11-13T10:00:03.000Z [container php] three",
			"2026-11-13T10:00:05.000Z [container php] five",
		)},
	}

	var out, errOut strings.Builder
	lines, err := runLogStreams(context.Background(), streams, &out, &errOut, logsRequest{})
	if err != nil || lines != 5 {
		t.Fatalf("read %d lines, %v; %s", lines, err, errOut.String())
	}

	want := "front | 2026-11-13T10:00:01.000Z [proxy acme] one\n" +
		"web-1 | 2026-11-13T10:00:02.000Z [container php] two\n" +
		"web-1 | 2026-11-13T10:00:03.000Z [container php] three\n" +
		"front | 2026-11-13T10:00:04.000Z [proxy acme] four\n" +
		"web-1 | 2026-11-13T10:00:05.000Z [container php] five\n"
	if out.String() != want {
		t.Errorf("the lines came out as\n%s", out.String())
	}

	out.Reset()
	lines, err = runLogStreams(context.Background(), []logStream{{cmd: script("a line", "another")}}, &out, &errOut, logsRequest{})
	if err != nil || lines != 2 || out.String() != "a line\nanother\n" {
		t.Errorf("one machine read %q, %d lines, %v", out.String(), lines, err)
	}
}

// As objects, a line says which machine it is from inside the object, and
// is ordered by the time it holds.
func TestLinesAsObjectsNameTheirMachine(t *testing.T) {
	line := `{"time":"2026-11-13T10:00:00Z","source":"container","name":"php","message":"hello"}`

	got := named("web-1", line, true)
	var entry map[string]string
	if err := json.Unmarshal([]byte(got), &entry); err != nil {
		t.Fatalf("%q is not an object: %v", got, err)
	}
	if entry["machine"] != "web-1" || entry["message"] != "hello" {
		t.Errorf("the object reads %v", entry)
	}

	if stampOf(line) != "2026-11-13T10:00:00Z" || stampOf("2026-11-13T10:00:00.000Z [cron] ran") != "2026-11-13T10:00:00.000Z" || stampOf("- [cron] ran") != "" {
		t.Error("the time a line starts with was not read")
	}
	if named("", line, true) != line || named("", "text", false) != "text" {
		t.Error("one machine's line was changed")
	}
}

// A machine that refuses, or whose agent is older than the verb, is said in
// a sentence; and where another machine answered, beside its lines.
func TestMachineThatCannotBeReadIsSaid(t *testing.T) {
	fails := func(stderr string, code int) *exec.Cmd {
		return exec.Command("sh", "-c", "echo '"+stderr+"' >&2; exit "+strconv.Itoa(code))
	}

	var out, errOut strings.Builder
	_, err := runLogStreams(context.Background(), []logStream{{cmd: fails("sh: 1: vallic-logs: not found", 127)}}, &out, &errOut, logsRequest{})
	if err == nil || !strings.Contains(err.Error(), "cannot be read for logs yet") {
		t.Errorf("an older agent was answered %v", err)
	}

	_, err = runLogStreams(context.Background(), []logStream{{cmd: fails("vallic: logs: this account does not run acme on this machine", 255)}}, &out, &errOut, logsRequest{})
	if err == nil || err.Error() != "logs: this account does not run acme on this machine" {
		t.Errorf("a refusal was answered %v", err)
	}

	// One of two answered: its lines are printed and the other is named.
	out.Reset()
	lines, err := runLogStreams(context.Background(), []logStream{
		{name: "front", cmd: exec.Command("printf", "%s\n", "2026-11-13T10:00:01.000Z [proxy acme] one")},
		{name: "web-1", cmd: fails("ssh: connect to host web-1 port 2417: Connection timed out", 255)},
	}, &out, &errOut, logsRequest{})
	if err != nil || lines != 1 || !strings.Contains(out.String(), "front | ") {
		t.Errorf("one machine of two read %q, %d lines, %v", out.String(), lines, err)
	}
	if !strings.Contains(errOut.String(), "warning: web-1: ssh: connect to host web-1") {
		t.Errorf("the machine that did not answer was not named: %q", errOut.String())
	}
}
