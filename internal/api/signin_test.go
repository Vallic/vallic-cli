package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// What the control plane answers a member of a team that signs in through
// its identity provider, before they have.
const ssoRefusal = `{"error":{"code":"sso_required","message":"Acme signs in through its identity provider. Sign in there in a browser, then try again: https://console.example.test/sso/k8ohjy/start?return=/k8ohjy"}}`

// A control plane that refuses the first `refused` requests, then answers.
func provider(t *testing.T, refused int) (*httptest.Server, *int) {
	t.Helper()

	requests := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++

		w.Header().Set("Content-Type", "application/json")

		if requests <= refused {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(ssoRefusal))

			return
		}

		_, _ = w.Write([]byte(`{"applied":[],"unreachable":[]}`))
	}))
	t.Cleanup(server.Close)

	return server, &requests
}

// Signed in at the provider, the same request goes again and goes through.
func TestSignInSendsTheRequestAgain(t *testing.T) {
	server, requests := provider(t, 1)
	client := New(server.URL, anonymous{}, "test")

	var asked []string

	client.SignIn = func(_ context.Context, refusal *Error) error {
		asked = append(asked, refusal.SignInURL())

		return nil
	}

	if _, err := client.ApplyProjectVariables(context.Background(), 7); err != nil {
		t.Fatalf("ApplyProjectVariables() = %v, want it through after signing in", err)
	}

	if *requests != 2 {
		t.Errorf("requests = %d, want the refused one and the one after signing in", *requests)
	}

	want := "https://console.example.test/sso/k8ohjy/start?return=/k8ohjy"
	if len(asked) != 1 || asked[0] != want {
		t.Errorf("SignIn was asked %v, want once for %s", asked, want)
	}
}

// A provider still refusing after the person said they had signed in is not
// one to send them back to in a loop: the second refusal is the answer.
func TestSignInIsAskedOnce(t *testing.T) {
	server, requests := provider(t, 10)
	client := New(server.URL, anonymous{}, "test")
	calls := 0

	client.SignIn = func(context.Context, *Error) error {
		calls++

		return nil
	}

	_, err := client.ApplyProjectVariables(context.Background(), 7)

	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Code != CodeSSORequired {
		t.Fatalf("ApplyProjectVariables() = %v, want the second refusal", err)
	}

	if calls != 1 || *requests != 2 {
		t.Errorf("SignIn asked %d times over %d requests, want once over two", calls, *requests)
	}
}

// Without a way to sign in, or when signing in did not happen, the refusal
// or the reason is the answer and nothing is sent again.
func TestNoSignInLeavesTheRefusal(t *testing.T) {
	server, requests := provider(t, 10)
	client := New(server.URL, anonymous{}, "test")

	_, err := client.ApplyProjectVariables(context.Background(), 7)

	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Code != CodeSSORequired {
		t.Fatalf("ApplyProjectVariables() = %v, want the refusal", err)
	}

	stopped := errors.New("stopped")
	client.SignIn = func(context.Context, *Error) error { return stopped }

	if _, err := client.ApplyProjectVariables(context.Background(), 7); !errors.Is(err, stopped) {
		t.Fatalf("ApplyProjectVariables() = %v, want the reason signing in stopped", err)
	}

	if *requests != 2 {
		t.Errorf("requests = %d, want one each and none sent again", *requests)
	}
}

// The address is the last one in the message, and only a refusal of this
// kind has one.
func TestSignInURL(t *testing.T) {
	cases := []struct {
		name string
		err  Error
		want string
	}{
		{"at the end", Error{Code: CodeSSORequired, Message: "Acme signs in through its identity provider. Sign in there in a browser, then try again: https://console.example.test/sso/abc/start"}, "https://console.example.test/sso/abc/start"},
		{"none given", Error{Code: CodeSSORequired, Message: "Acme signs in through its identity provider."}, ""},
		{"another refusal", Error{Code: "forbidden", Message: "Not yours: https://console.example.test/x"}, ""},
	}

	for _, c := range cases {
		if got := c.err.SignInURL(); got != c.want {
			t.Errorf("%s: SignInURL() = %q, want %q", c.name, got, c.want)
		}
	}
}
