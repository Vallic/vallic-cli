package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Static is a credential with nothing to refresh.
//
// A personal access token, whether it came off the disk or out of
// VALLIC_TOKEN. It is the whole implementation because that is the whole
// truth about a PAT: it works until it does not, and there is no step the
// client can take to renew it.
type Static struct {
	token string
}

// NewStatic wraps a bearer string.
func NewStatic(token string) *Static {
	return &Static{token: token}
}

// Token implements Source.
func (s *Static) Token() (string, error) {
	if s.token == "" {
		return "", ErrNoCredential
	}

	return s.token, nil
}

// Refreshing is an OAuth credential that renews itself.
//
// The only place in the CLI that knows a token can expire. It holds the
// credential, refreshes it when it is about to lapse, and writes the result
// back — so a long-running `vallic deploy --wait` does not fail twenty
// minutes in, and so the next command starts from the renewed token rather
// than refreshing again.
type Refreshing struct {
	client *Client

	// mu guards creds and serialises refreshes. Commands are single-threaded
	// today; a `--wait` that polls while streaming a log will not be, and a
	// double refresh is two rotations of a rotating refresh token, which
	// looks exactly like a stolen one to a server that is watching for that.
	mu    sync.Mutex
	creds *Credentials

	// persist is what writes the renewed credential back. Injected so tests
	// do not touch a real home directory, and so a `--no-save` mode has
	// somewhere to plug in.
	persist func(*Credentials) error
}

// NewRefreshing wraps a stored OAuth credential.
func NewRefreshing(client *Client, creds *Credentials) *Refreshing {
	return &Refreshing{
		client:  client,
		creds:   creds,
		persist: func(c *Credentials) error { return c.Save() },
	}
}

// Token implements Source, renewing first if it has to.
func (r *Refreshing) Token() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.creds.Expired() {
		return r.creds.AccessToken, nil
	}

	if r.creds.RefreshToken == "" {
		// An OAuth credential with no refresh token and an expired access
		// token is spent. Saying so plainly is better than sending it and
		// reporting the 401 the server will answer with.
		return "", fmt.Errorf("%w: the session has expired; run: vallic login", ErrNoCredential)
	}

	renewed, err := r.client.Refresh(context.Background(), r.creds.RefreshToken)
	if err != nil {
		var oauthErr *OAuthError
		if errors.As(err, &oauthErr) && oauthErr.Code == "invalid_grant" {
			// The refresh token has been revoked, rotated out from under us,
			// or expired. All three mean the same thing to the person, and
			// naming the OAuth code at them would explain nothing.
			return "", fmt.Errorf("%w: the session is no longer valid; run: vallic login", ErrNoCredential)
		}

		return "", err
	}

	// Carried across rather than taken from the response. The server has no
	// reason to repeat who this is or what it is called on a refresh, and
	// losing the label would silently rename the device in the console's
	// session list. AuthTime is deliberately *not* carried across when the
	// server sent a new one: a refresh is not an authentication, and if the
	// server says when the person last authenticated, it is right and this
	// copy is stale.
	renewed.Account = r.creds.Account
	renewed.Label = r.creds.Label
	if renewed.AuthTime.IsZero() {
		renewed.AuthTime = r.creds.AuthTime
	}
	if renewed.RefreshToken == "" {
		// A server that does not rotate refresh tokens sends none back. The
		// one in hand stays valid; dropping it would log the person out on
		// the next expiry for no reason.
		renewed.RefreshToken = r.creds.RefreshToken
	}

	r.creds = renewed

	if err := r.persist(renewed); err != nil {
		// Not fatal. The token in memory is good and the command should run;
		// the cost of a failed write is one extra refresh next time, and
		// failing a deploy because a config directory is read-only would be
		// a worse trade.
		return renewed.AccessToken, nil
	}

	return renewed.AccessToken, nil
}

// Credentials exposes what is held, for `whoami` and for a step-up that needs
// to know which account it is re-authenticating.
func (r *Refreshing) Credentials() *Credentials {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.creds
}

// SourceFor builds the right Source for a stored credential.
//
// This function is the whole reason the interface exists: it is the one place
// that branches on Kind, and adding a third kind would change this file and
// no command.
func SourceFor(client *Client, creds *Credentials) Source {
	if creds.Kind == KindOAuth {
		return NewRefreshing(client, creds)
	}

	return NewStatic(creds.AccessToken)
}
