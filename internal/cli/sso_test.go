package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/vallic/vallic-cli/internal/api"
	"github.com/vallic/vallic-cli/internal/config"
)

// What the control plane answers a member of a team that signs in through
// its identity provider, before they have.
func ssoRequired(address string) *api.Error {
	return &api.Error{
		Code:    api.CodeSSORequired,
		Message: "Acme signs in through its identity provider. Sign in there in a browser, then try again: " + address,
		Status:  403,
	}
}

// With no terminal there is nobody to send to the browser: the refusal is
// the answer, with the address in it to open by hand.
func TestSignInAtProviderWithoutATerminal(t *testing.T) {
	env := &Env{In: bytes.NewBuffer(nil), config: &config.Config{API: "https://console.example.test"}}
	refusal := ssoRequired("https://console.example.test/sso/abc/start")

	if err := env.signInAtProvider(context.Background(), refusal); !errors.Is(err, refusal) {
		t.Fatalf("signInAtProvider() = %v, want the refusal itself", err)
	}
}

// Only an address on the control plane this CLI talks to is ever opened: no
// refusal of ours names another, and one that did is not followed.
func TestOnlyTheControlPlanesOwnAddressIsOpened(t *testing.T) {
	env := &Env{config: &config.Config{API: "https://console.example.test"}}

	cases := map[string]bool{
		"https://console.example.test/sso/abc/start": true,
		"https://CONSOLE.example.test/sso/abc/start": true,
		"https://evil.example.test/sso/abc/start":    false,
		"http://console.example.test/sso/abc/start":  false,
		"javascript:alert(1)":                        false,
	}

	for address, want := range cases {
		if got := env.onControlPlane(address); got != want {
			t.Errorf("onControlPlane(%q) = %v, want %v", address, got, want)
		}
	}
}
