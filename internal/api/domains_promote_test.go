package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// primary sends canonical false as well, so a canonical hostname made primary
// stops redirecting the others; canonical sends canonical alone, which makes
// it primary too.
func TestPromoteDomainSaysWhetherTheOthersRedirect(t *testing.T) {
	for _, tc := range []struct {
		name      string
		canonical bool
		want      map[string]any
	}{
		{"primary", false, map[string]any{"primary": true, "canonical": false}},
		{"canonical", true, map[string]any{"canonical": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var method, path string
			var body map[string]any

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				method, path = r.Method, r.URL.Path
				_ = json.NewDecoder(r.Body).Decode(&body)

				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"domain":{"id":5,"hostname":"shop.example.com","is_primary":true},"message":"shop.example.com is now primary."}`))
			}))
			defer server.Close()

			client := New(server.URL, anonymous{}, "test")

			updated, err := client.PromoteDomain(context.Background(), 5, tc.canonical)
			if err != nil {
				t.Fatalf("PromoteDomain: %v", err)
			}

			if method != http.MethodPatch || path != Prefix+"/domains/5" {
				t.Errorf("sent %s %s, want PATCH %s/domains/5", method, path, Prefix)
			}

			if !reflect.DeepEqual(body, tc.want) {
				t.Errorf("body = %v, want %v", body, tc.want)
			}

			if !updated.Domain.IsPrimary || updated.Message == "" {
				t.Errorf("answer = %+v, want the domain and the message", updated)
			}
		})
	}
}
