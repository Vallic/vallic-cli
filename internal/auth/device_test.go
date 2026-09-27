package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The poll loop is where a device flow goes wrong, because three of the
// responses it gets are not failures and one of them changes the cadence.
// These drive it against a real HTTP server so the form encoding, the status
// codes and the JSON are all exercised together.

func TestPollWaitsThroughAuthorizationPending(t *testing.T) {
	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != tokenPath {
			t.Errorf("polled %s, want %s", r.URL.Path, tokenPath)
		}

		if got := r.FormValue("grant_type"); got != grantDeviceCode {
			t.Errorf("grant_type = %q, want %q", got, grantDeviceCode)
		}

		if got := r.FormValue("device_code"); got != "dev-code" {
			t.Errorf("device_code = %q, want dev-code", got)
		}

		// Twice pending, then the token. The pending answers carry 400,
		// which is what the RFC specifies and what a client that only looked
		// at the status would treat as fatal.
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": errAuthorizationPending})

			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "at-1",
			"token_type":    "Bearer",
			"expires_in":    900,
			"refresh_token": "rt-1",
			"auth_time":     1750000000,
		})
	}))
	defer server.Close()

	client := NewClient(server.URL)

	creds, err := client.Poll(context.Background(), fastDevice(), nil)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	if creds.AccessToken != "at-1" || creds.RefreshToken != "rt-1" {
		t.Errorf("got %+v, want the token and refresh token from the last response", creds)
	}

	if creds.Kind != KindOAuth {
		t.Errorf("Kind = %q, want %q", creds.Kind, KindOAuth)
	}

	// The claim that makes a step-up answerable. Without it the CLI cannot
	// tell a freshly authenticated token from a refreshed one.
	if creds.AuthTime.Unix() != 1750000000 {
		t.Errorf("AuthTime = %v, want the auth_time from the response", creds.AuthTime)
	}

	if creds.Expiry.IsZero() {
		t.Error("Expiry is zero although expires_in was sent")
	}

	if calls.Load() != 3 {
		t.Errorf("polled %d times, want 3", calls.Load())
	}
}

func TestPollBacksOffOnSlowDown(t *testing.T) {
	var calls atomic.Int32
	var gaps []time.Duration
	last := time.Now()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gaps = append(gaps, time.Since(last))
		last = time.Now()

		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": errSlowDown})

			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer"})
	}))
	defer server.Close()

	client := NewClient(server.URL)

	device := fastDevice()

	if _, err := client.Poll(context.Background(), device, nil); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	if len(gaps) < 2 {
		t.Fatalf("polled %d times, want at least 2", len(gaps))
	}

	// slow_down adds five seconds to the interval, which is the RFC's own
	// instruction. Asserted as "longer than the first gap" rather than an
	// absolute figure, so the test is about the behaviour and not the clock.
	if gaps[1] <= gaps[0] {
		t.Errorf("interval did not grow after slow_down: %v then %v", gaps[0], gaps[1])
	}
}

func TestPollStopsWhenDeclined(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": errAccessDenied})
	}))
	defer server.Close()

	_, err := NewClient(server.URL).Poll(context.Background(), fastDevice(), nil)
	if err == nil {
		t.Fatal("Poll succeeded although the person declined")
	}

	if !strings.Contains(err.Error(), "declined") {
		t.Errorf("error = %q, want it to say the sign-in was declined", err)
	}
}

func TestPollStopsWhenCodeExpires(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": errExpiredToken})
	}))
	defer server.Close()

	_, err := NewClient(server.URL).Poll(context.Background(), fastDevice(), nil)
	if err == nil {
		t.Fatal("Poll succeeded on an expired device code")
	}

	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("error = %q, want it to say the code expired", err)
	}
}

// A transport failure mid-poll is not a failed sign-in: the device code is
// good until its deadline, so the loop is the retry. A client that gave up
// here would fail every sign-in that crossed a dropped connection.
func TestPollSurvivesATransportFailure(t *testing.T) {
	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// Hijacked and closed, so the client sees a broken connection
			// rather than any HTTP status.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}

			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer"})
	}))
	defer server.Close()

	creds, err := NewClient(server.URL).Poll(context.Background(), fastDevice(), nil)
	if err != nil {
		t.Fatalf("Poll gave up on a dropped connection: %v", err)
	}

	if creds.AccessToken != "at" {
		t.Errorf("AccessToken = %q, want at", creds.AccessToken)
	}
}

// An unexpected OAuth code stops the loop rather than spinning on it.
func TestPollStopsOnAnUnknownError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":             "invalid_client",
			"error_description": "no such client",
		})
	}))
	defer server.Close()

	_, err := NewClient(server.URL).Poll(context.Background(), fastDevice(), nil)

	var oauthErr *OAuthError
	if !errors.As(err, &oauthErr) {
		t.Fatalf("error = %v, want an *OAuthError", err)
	}

	if oauthErr.Code != "invalid_client" {
		t.Errorf("Code = %q, want invalid_client", oauthErr.Code)
	}

	if !strings.Contains(oauthErr.Error(), "no such client") {
		t.Errorf("Error() = %q, want the description in it", oauthErr.Error())
	}
}

// A step-up asks for authentication to happen now, which is what max_age=0
// means. Without it the server has no way to tell a first sign-in from a
// demand to re-prove one, and the whole 2FA-per-deploy gate is unenforceable.
func TestAuthorizeSendsMaxAgeForAStepUp(t *testing.T) {
	var maxAge string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		maxAge = r.FormValue("max_age")

		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":      "dc",
			"user_code":        "ABCD-EFGH",
			"verification_uri": "https://example.test/device",
			"expires_in":       600,
		})
	}))
	defer server.Close()

	client := NewClient(server.URL)

	if _, err := client.Authorize(context.Background(), nil, nil, ""); err != nil {
		t.Fatalf("Authorize: %v", err)
	}

	if maxAge != "" {
		t.Errorf("max_age = %q on an ordinary sign-in, want it absent", maxAge)
	}

	if _, err := client.Authorize(context.Background(), nil, &StepUp{Reason: "production"}, ""); err != nil {
		t.Fatalf("Authorize: %v", err)
	}

	if maxAge != "0" {
		t.Errorf("max_age = %q on a step-up, want 0", maxAge)
	}
}

// A step-up carries the credential being re-proved, so the server can bind
// the challenge to one account. Without it the challenge could be answered by
// whoever happens to be signed in on whatever browser the code reached.
func TestAuthorizeBindsAStepUpToTheCurrentCredential(t *testing.T) {
	var authorization, deviceName string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		deviceName = r.FormValue("device_name")

		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":      "dc",
			"user_code":        "BCDF-GHJ-KLM",
			"verification_uri": "https://example.test/device",
			"expires_in":       600,
		})
	}))
	defer server.Close()

	client := NewClient(server.URL)

	if _, err := client.Authorize(
		context.Background(),
		nil,
		&StepUp{Reason: "production", AccessToken: "vco_live"},
		"alice-laptop",
	); err != nil {
		t.Fatalf("Authorize: %v", err)
	}

	if authorization != "Bearer vco_live" {
		t.Errorf("Authorization = %q, want the current access token", authorization)
	}

	if deviceName != "alice-laptop" {
		t.Errorf("device_name = %q, want alice-laptop", deviceName)
	}

	// An ordinary sign-in has nothing to bind and must not send a header.
	if _, err := client.Authorize(context.Background(), nil, nil, "alice-laptop"); err != nil {
		t.Fatalf("Authorize: %v", err)
	}

	if authorization != "" {
		t.Errorf("Authorization = %q on a first sign-in, want none", authorization)
	}
}

// The subject travels so a step-up can tell who answered it.
func TestExchangeReadsTheSubjectAndStrength(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at",
			"token_type":   "Bearer",
			"auth_time":    1750000000,
			"acr":          "vallic:pwd",
			"sub":          "42",
		})
	}))
	defer server.Close()

	creds, err := NewClient(server.URL).Refresh(context.Background(), "rt")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if creds.Subject != "42" {
		t.Errorf("Subject = %q, want 42", creds.Subject)
	}

	if creds.Acr != "vallic:pwd" {
		t.Errorf("Acr = %q, want vallic:pwd", creds.Acr)
	}
}

// A response missing a required field is refused here rather than producing a
// confusing failure later in the poll loop.
func TestAuthorizeRefusesAnIncompleteResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "dc"})
	}))
	defer server.Close()

	if _, err := NewClient(server.URL).Authorize(context.Background(), nil, nil, ""); err == nil {
		t.Fatal("Authorize accepted a response with no user code or verification URI")
	}
}

// A token type this client cannot use is caught at exchange, not turned into
// 401s later that look like an expired credential.
func TestExchangeRefusesANonBearerToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at",
			"token_type":   "mac",
		})
	}))
	defer server.Close()

	_, err := NewClient(server.URL).Refresh(context.Background(), "rt")
	if err == nil {
		t.Fatal("Refresh accepted a mac token")
	}

	if !strings.Contains(err.Error(), "mac") {
		t.Errorf("error = %q, want the token type named", err)
	}
}

// An HTML error page from a proxy is not printed into the terminal, and the
// status is reported instead.
func TestPostReportsAStatusWhenTheBodyIsNotOAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
	}))
	defer server.Close()

	_, err := NewClient(server.URL).Authorize(context.Background(), nil, nil, "")
	if err == nil {
		t.Fatal("Authorize succeeded against a 502")
	}

	var oauthErr *OAuthError
	if errors.As(err, &oauthErr) {
		t.Errorf("error = %v, want a plain error rather than an *OAuthError", err)
	}

	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error = %q, want the status in it", err)
	}
}

func TestExpiredLeavesSlack(t *testing.T) {
	cases := []struct {
		name  string
		creds Credentials
		want  bool
	}{
		{
			// A PAT states no expiry. Guessing one would log somebody out of
			// a working credential.
			name:  "no stated expiry is never expired",
			creds: Credentials{},
			want:  false,
		},
		{
			name:  "past",
			creds: Credentials{Expiry: time.Now().Add(-time.Second)},
			want:  true,
		},
		{
			// Inside the minute of slack. Treated as expired so a token is
			// not sent that will lapse in flight, turning a refresh into a
			// failed command.
			name:  "about to lapse",
			creds: Credentials{Expiry: time.Now().Add(30 * time.Second)},
			want:  true,
		},
		{
			name:  "well in the future",
			creds: Credentials{Expiry: time.Now().Add(time.Hour)},
			want:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.creds.Expired(); got != tc.want {
				t.Errorf("Expired() = %v, want %v", got, tc.want)
			}
		})
	}
}

// fastDevice is a device code with a one-millisecond poll interval, so the
// loop's behaviour is tested without its cadence.
func fastDevice() *DeviceAuth {
	return &DeviceAuth{
		DeviceCode:      "dev-code",
		UserCode:        "ABCD-EFGH",
		VerificationURI: "https://example.test/device",
		ExpiresIn:       600,
		Interval:        0,
	}
}

func init() {
	// Interval 0 would otherwise mean the RFC's five seconds, and eight
	// tests at five seconds a poll is a suite nobody runs. The override is
	// here rather than in each test so the intent is stated once.
	defaultPollInterval = time.Millisecond
}
