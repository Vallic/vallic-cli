package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vallic/vallic-cli/internal/api"
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
		"Logs for acme-staging are not kept by Vallic Cloud",
		"Sent to:        Team Loki (Grafana Loki)",
		"the last 7 days",
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
