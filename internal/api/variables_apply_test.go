package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// apply is a POST to the scope's own route, and the answer names what
// restarted and what could not be reached.
func TestApplyPostsToTheScopesRoute(t *testing.T) {
	var method, path string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"applied":["acme-staging"],"unreachable":["acme-production"]}`))
	}))
	defer server.Close()

	client := New(server.URL, anonymous{}, "test")

	result, err := client.ApplyProjectVariables(context.Background(), 7)
	if err != nil {
		t.Fatalf("ApplyProjectVariables: %v", err)
	}

	if method != http.MethodPost || path != Prefix+"/projects/7/variables/apply" {
		t.Errorf("request = %s %s, want POST %s/projects/7/variables/apply", method, path, Prefix)
	}

	if len(result.Applied) != 1 || result.Applied[0] != "acme-staging" {
		t.Errorf("applied = %v", result.Applied)
	}

	if len(result.Unreachable) != 1 || result.Unreachable[0] != "acme-production" {
		t.Errorf("unreachable = %v", result.Unreachable)
	}

	if _, err := client.ApplyEnvironmentVariables(context.Background(), 12); err != nil {
		t.Fatalf("ApplyEnvironmentVariables: %v", err)
	}

	if path != Prefix+"/environments/12/variables/apply" {
		t.Errorf("path = %s, want %s/environments/12/variables/apply", path, Prefix)
	}
}

// A control plane that says nothing about pending is not one that says
// "nothing pending". The three answers have to stay three.
func TestPendingTellsUnknownFromFalse(t *testing.T) {
	cases := []struct {
		name string
		body string
		want *bool
	}{
		{name: "an older control plane says nothing", body: `{"variables":[]}`, want: nil},
		{name: "up to date", body: `{"pending":false,"variables":[]}`, want: ptr(false)},
		{name: "behind", body: `{"pending":true,"variables":[]}`, want: ptr(true)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			list, err := New(server.URL, anonymous{}, "test").EnvironmentVariables(context.Background(), 1)
			if err != nil {
				t.Fatalf("EnvironmentVariables: %v", err)
			}

			switch {
			case tc.want == nil && list.Pending != nil:
				t.Errorf("pending = %v, want nil", *list.Pending)
			case tc.want != nil && (list.Pending == nil || *list.Pending != *tc.want):
				t.Errorf("pending = %v, want %v", list.Pending, *tc.want)
			}
		})
	}
}
