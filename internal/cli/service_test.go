package cli

import (
	"testing"

	"github.com/vallic/vallic-cli/internal/api"
)

func stackOf(services ...api.Service) *api.Stack {
	return &api.Stack{Services: services}
}

// The application and the entrypoint are two services, and a stack that has
// neither must say so in words. A dash here would read as a figure the CLI
// failed to fetch, and somebody would go looking for the service it did not
// print rather than for the reason there is none.
func TestServiceOrNoneNamesAnAbsentServiceInWords(t *testing.T) {
	php := "php"
	empty := ""

	cases := []struct {
		name string
		id   *string
		want string
	}{
		{name: "a service the stack has", id: &php, want: "php"},
		{name: "a stack with none", id: nil, want: "nothing in this stack"},
		{name: "an empty id, which is the same answer", id: &empty, want: "nothing in this stack"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := serviceOrNone(tc.id); got != tc.want {
				t.Errorf("serviceOrNone() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The dependency list is the one nobody wrote, so its ids are the ones that
// explain themselves least. If this stops naming what the control plane sent,
// `zookeeper` goes back to being a word in a list with nothing to say why it
// is there.
func TestServiceGlossExplainsAServiceNobodyAskedFor(t *testing.T) {
	cases := []struct {
		name  string
		stack *api.Stack
		id    string
		want  string
	}{
		{
			name: "a label and a description",
			stack: stackOf(api.Service{
				ID:          "zookeeper",
				Label:       "ZooKeeper",
				Description: "Coordination for Solr. Never selected on its own; Solr pulls it in.",
			}),
			id:   "zookeeper",
			want: "zookeeper: ZooKeeper. Coordination for Solr. Never selected on its own; Solr pulls it in.",
		},
		{
			name:  "a label and nothing else",
			stack: stackOf(api.Service{ID: "solr", Label: "Solr"}),
			id:    "solr",
			want:  "solr: Solr",
		},
		{
			name:  "a description and nothing else",
			stack: stackOf(api.Service{ID: "solr", Description: "Search."}),
			id:    "solr",
			want:  "solr: Search.",
		},
		{
			// Neither of these happens against this control plane. The id is
			// still worth printing: what was pulled in is the answer, and the
			// gloss was only ever the help with it.
			name:  "a service with nothing to say about itself",
			stack: stackOf(api.Service{ID: "solr"}),
			id:    "solr",
			want:  "solr",
		},
		{
			name:  "a name with no row behind it",
			stack: stackOf(api.Service{ID: "php", Label: "PHP-FPM"}),
			id:    "zookeeper",
			want:  "zookeeper",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := serviceGloss(tc.stack, tc.id); got != tc.want {
				t.Errorf("serviceGloss() = %q, want %q", got, tc.want)
			}
		})
	}
}
