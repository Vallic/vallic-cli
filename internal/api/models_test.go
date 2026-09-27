package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

// The one thing a client must never get wrong about a variable: a withheld
// value and an empty value are different answers, and a cell that showed
// nothing for both would report a configured secret as unset.
func TestVariableDisplayTellsWithheldFromEmpty(t *testing.T) {
	cases := []struct {
		name     string
		variable Variable
		want     string
	}{
		{
			name:     "a secret is named as one",
			variable: Variable{Secret: true},
			want:     "(secret)",
		},
		{
			name:     "a null value from an older control plane reads the same",
			variable: Variable{Secret: false, Value: nil},
			want:     "(secret)",
		},
		{
			name:     "an empty value is visibly empty, not blank",
			variable: Variable{Value: ptr("")},
			want:     `""`,
		},
		{
			name:     "an ordinary value is itself",
			variable: Variable{Value: ptr("production")},
			want:     "production",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.variable.Display(); got != tc.want {
				t.Errorf("Display() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A certificate pasted into a variable must not take the table's header with
// it, and a truncation must be visible as one.
func TestVariableDisplayFlattensAndTruncates(t *testing.T) {
	long := Variable{Value: ptr(strings.Repeat("x", 200))}

	got := long.Display()

	if len([]rune(got)) != 48 {
		t.Errorf("Display() is %d runes, want 48", len([]rune(got)))
	}

	if !strings.HasSuffix(got, "…") {
		t.Errorf("Display() = %q, want a visible truncation", got)
	}

	multi := Variable{Value: ptr("-----BEGIN KEY-----\nabc\r\ndef")}

	if got := multi.Display(); strings.ContainsAny(got, "\n\r") {
		t.Errorf("Display() = %q, want no line breaks", got)
	}
}

// Whether a value is overriding another is the difference between editing the
// record in effect and editing one that changes every other environment.
func TestVariableScopeLabelSaysWhatItHid(t *testing.T) {
	overriding := Variable{Scope: "environment", Overridden: true, InheritedFrom: ptr("project")}

	if got, want := overriding.ScopeLabel(), "environment (overrides project)"; got != want {
		t.Errorf("ScopeLabel() = %q, want %q", got, want)
	}

	plain := Variable{Scope: "project"}

	if got, want := plain.ScopeLabel(), "project"; got != want {
		t.Errorf("ScopeLabel() = %q, want %q", got, want)
	}

	// Overridden with nothing named to have been overridden is a control
	// plane contradicting itself; the scope alone is the only true half.
	half := Variable{Scope: "environment", Overridden: true}

	if got, want := half.ScopeLabel(), "environment"; got != want {
		t.Errorf("ScopeLabel() = %q, want %q", got, want)
	}
}

// Omitting `secret` is what lets an existing secret stay secret. Sending
// `false` would turn a password into a readable variable, so the field has to
// leave the struct entirely when it was not asked for.
func TestVariableWriteOmitsSecretUnlessAsked(t *testing.T) {
	raw, err := json.Marshal(VariableWrite{Name: "API_TOKEN", Value: "new"})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(raw), "secret") {
		t.Errorf("body = %s, want no secret key at all", raw)
	}

	// An empty value is sent, because the control plane tells an omitted key
	// from an empty one and refuses the first.
	if !strings.Contains(string(raw), `"value":"new"`) {
		t.Errorf("body = %s, want the value", raw)
	}

	raw, err = json.Marshal(VariableWrite{Name: "API_TOKEN", Value: "", Secret: ptr(true)})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(raw), `"secret":true`) {
		t.Errorf("body = %s, want secret true", raw)
	}

	if !strings.Contains(string(raw), `"value":""`) {
		t.Errorf("body = %s, want an empty value sent rather than omitted", raw)
	}
}

// `value: null` beside `secret: true` is the whole contract for a script, so
// a client that decoded one into the other's absence would break it.
func TestVariableDecodesAWithheldValueAsNil(t *testing.T) {
	var v Variable

	if err := json.Unmarshal([]byte(`{"name":"API_TOKEN","value":null,"secret":true}`), &v); err != nil {
		t.Fatal(err)
	}

	if v.Value != nil {
		t.Errorf("Value = %v, want nil", v.Value)
	}

	if err := json.Unmarshal([]byte(`{"name":"DEBUG","value":"","secret":false}`), &v); err != nil {
		t.Fatal(err)
	}

	if v.Value == nil || *v.Value != "" {
		t.Errorf("Value = %v, want a pointer to the empty string", v.Value)
	}
}

// PHP has one array type for lists and maps, so `json_encode` renders an empty
// map as `[]`. Decoding that into a Go map fails and takes the whole response
// with it — which is exactly how typing the manifest broke `vallic validate`
// for every file that declared a service, since `environment` is empty far
// more often than not.
func TestAnEmptyMapArrivesFromPhpAsAList(t *testing.T) {
	var v Validation

	raw := `{"filename":"vallic.yaml","schema_version":1,"manifest":{` +
		`"runtime":[],"services":[{"name":"valkey","version":"8","environment":[]}]}}`

	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("decode = %v, want the empty list read as an empty map", err)
	}

	if len(v.Manifest.Runtime) != 0 {
		t.Errorf("Runtime = %v, want empty", v.Manifest.Runtime)
	}

	if len(v.Manifest.Services) != 1 {
		t.Fatalf("Services = %d, want 1", len(v.Manifest.Services))
	}

	if v.Manifest.Services[0].Environment == nil {
		t.Error("Environment = nil, want an empty map so a caller can range over it")
	}
}

// A populated object still decodes, and a list with anything in it is a real
// disagreement about the contract rather than PHP's empty-array quirk, so it
// must still fail rather than be silently swallowed.
func TestAStringMapStillReadsObjectsAndRefusesRealLists(t *testing.T) {
	var m StringMap

	if err := json.Unmarshal([]byte(`{"APP_ENV":"prod"}`), &m); err != nil {
		t.Fatalf("decode = %v", err)
	}

	if m["APP_ENV"] != "prod" {
		t.Errorf("m = %v, want APP_ENV=prod", m)
	}

	if err := json.Unmarshal([]byte(`["nonsense"]`), &m); err == nil {
		t.Error("decode of a non-empty list = nil error, want it to fail loudly")
	}
}

// The channel reaches the URL as a query parameter, and only when asked for.
//
// Asserted against a real server rather than by reading the string back,
// because what matters is the request the control plane receives: the ordinary
// call must arrive with no `channel` at all, so it is one cache entry there and
// is byte-identical to what every client built before channels existed sent.
func TestLatestCLIAsksForAChannelOnlyWhenGivenOne(t *testing.T) {
	for _, tc := range []struct {
		name    string
		channel string
		want    string
	}{
		{"nothing asked", "", ""},
		{"dev asked", ChannelDev, ChannelDev},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw string

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw = r.URL.RawQuery

				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"channel":"dev","published":true,"version":"0.4.1-dev-202609241830","downloads":{}}`))
			}))
			defer server.Close()

			client := New(server.URL, anonymous{}, "test")

			release, err := client.LatestCLI(context.Background(), tc.channel)
			if err != nil {
				t.Fatalf("LatestCLI: %v", err)
			}

			if got := query(raw, "channel"); got != tc.want {
				t.Errorf("channel parameter = %q, want %q (raw %q)", got, tc.want, raw)
			}

			// The answer's own channel, not the request's: an installation
			// serving dev says dev to a client that asked for nothing.
			if release.Channel != ChannelDev {
				t.Errorf("Channel = %q, want it read from the response", release.Channel)
			}
		})
	}
}

// query is one parameter's value, or empty where it was not sent at all.
func query(raw, key string) string {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return ""
	}

	return values.Get(key)
}

// anonymous is a credential source with no credential, which is what this route
// is asked with.
type anonymous struct{}

func (anonymous) Token() (string, error) { return "", nil }

// Retry-After is read in both forms RFC 9110 allows, and nothing else.
//
// Worth a table because the two forms come from different places — an origin
// server tends to send delta-seconds and a CDN an HTTP-date — and the CLI sees
// whichever sits in front of the control plane that day. The control plane's
// own limiter sends neither, on purpose: its flood backend cannot say when a
// window frees.
func TestRetryAfterReadsBothFormsAndNothingElse(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"delta seconds", "120", 2 * time.Minute},
		{"delta seconds with spaces", "  45 ", 45 * time.Second},
		{"http date", "Sun, 27 Sep 2026 12:00:30 GMT", 30 * time.Second},
		{"absent", "", 0},

		// Each of these would be a wait, if it were trusted. Zero instead, so
		// the caller falls back to its own schedule rather than sleeping for a
		// negative duration or for whatever an unparseable string coerces to.
		{"zero", "0", 0},
		{"negative", "-30", 0},
		{"a date already past", "Sun, 27 Sep 2026 11:59:00 GMT", 0},
		{"nonsense", "soon", 0},
		{"a float, which the grammar does not allow", "1.5", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryAfter(tc.header, now); got != tc.want {
				t.Errorf("retryAfter(%q) = %v, want %v", tc.header, got, tc.want)
			}
		})
	}
}

// And it reaches the error a caller branches on, from a real response.
//
// Asserted through a server rather than by calling retryAfter directly,
// because the half that broke in every previous version of this kind of bug
// was the wiring, not the parser.
func TestARateLimitCarriesWhatTheResponseAskedFor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"too many"}}`))
	}))
	defer server.Close()

	client := New(server.URL, anonymous{}, "test")

	_, err := client.Me(context.Background())

	if !IsRateLimited(err) {
		t.Fatalf("err = %v, want it recognised as a rate limit", err)
	}

	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an *Error", err)
	}

	if apiErr.RetryAfter != 42*time.Second {
		t.Errorf("RetryAfter = %v, want 42s", apiErr.RetryAfter)
	}
}

// A body with no envelope carries it too — which is the case that matters
// most, because that is what something in front of the control plane answers,
// and those limits are not the ones this client reasoned about.
func TestARetryAfterSurvivesAResponseWithNoEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("Too Many Requests"))
	}))
	defer server.Close()

	client := New(server.URL, anonymous{}, "test")

	_, err := client.Me(context.Background())

	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an *Error", err)
	}

	if apiErr.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %v, want 7s", apiErr.RetryAfter)
	}
}
