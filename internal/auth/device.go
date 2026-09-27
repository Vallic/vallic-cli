package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ClientID is what this binary calls itself to the authorization server.
//
// Public, and there is no secret beside it. A secret compiled into something
// every customer downloads is not a secret, which RFC 8252 says in more words
// — so the device grant authenticates the *person* in the browser and treats
// the client as untrusted. Nothing here is weakened by this identifier being
// known; it selects a client configuration, it does not prove anything.
const ClientID = "vallic-cli"

// Endpoint paths on the control plane.
//
// Separate from the API's own /api/vc/v1 prefix: the API surface is versioned
// because its payloads will change, and OAuth's are fixed by the RFCs.
const (
	deviceAuthorizationPath = "/oauth/device_authorization"
	tokenPath               = "/oauth/token"
	revokePath              = "/oauth/revoke"
)

// Grant types, spelled as the RFCs spell them.
const (
	grantDeviceCode   = "urn:ietf:params:oauth:grant-type:device_code"
	grantRefreshToken = "refresh_token"
)

// DeviceAuth is what the server answers a device authorization request with.
//
// RFC 8628 §3.2.
type DeviceAuth struct {
	DeviceCode string `json:"device_code"`
	UserCode   string `json:"user_code"`

	// VerificationURI is the page to open. VerificationURIComplete has the
	// code already in it, and is what the CLI should offer first — a code
	// typed by hand is a code mistyped by hand.
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`

	ExpiresIn int `json:"expires_in"`
	Interval  int `json:"interval,omitempty"`
}

// Deadline is when this device code stops being usable.
func (d *DeviceAuth) Deadline(from time.Time) time.Time {
	seconds := d.ExpiresIn
	if seconds <= 0 {
		// RFC 8628 makes expires_in REQUIRED, so this is a server that is
		// not following it. Ten minutes is its own suggested figure, and is
		// a better answer than polling forever.
		seconds = 600
	}

	return from.Add(time.Duration(seconds) * time.Second)
}

// defaultPollInterval is used when the server states none.
//
// The RFC's own figure. A variable rather than a constant so the tests can
// drive the poll loop without waiting five seconds a turn; nothing in the
// command tree changes it.
var defaultPollInterval = 5 * time.Second

// PollInterval is how long to wait between token requests.
func (d *DeviceAuth) PollInterval() time.Duration {
	if d.Interval <= 0 {
		return defaultPollInterval
	}

	return time.Duration(d.Interval) * time.Second
}

// OAuthError is an error response from the authorization server.
//
// RFC 6749 §5.2. Kept as a type rather than flattened to a string because
// four of its codes are not failures — they are the states a device flow
// passes through, and the poll loop has to tell them apart.
type OAuthError struct {
	Code        string `json:"error"`
	Description string `json:"error_description,omitempty"`
	URI         string `json:"error_uri,omitempty"`
	Status      int    `json:"-"`
}

func (e *OAuthError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Description)
	}

	return e.Code
}

// The device flow's own error codes. RFC 8628 §3.5.
const (
	errAuthorizationPending = "authorization_pending"
	errSlowDown             = "slow_down"
	errAccessDenied         = "access_denied"
	errExpiredToken         = "expired_token"
)

// Client talks to the authorization server.
type Client struct {
	// BaseURL is the control plane, without a trailing slash.
	BaseURL string

	// HTTP is the transport. Its timeout applies per request, not to the
	// poll loop, which runs until the device code expires.
	HTTP *http.Client
}

// NewClient builds a client with sensible transport settings.
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// StepUp asks for a fresh authentication rather than a first one.
//
// Passed to Authorize when the API has answered `reauthentication_required`:
// the person is signed in, and the deploy they asked for needs them to prove
// it again — which is what `max_age=0` means in OIDC, and the reason this is
// expressible at all. A bearer token has nothing in it to challenge.
type StepUp struct {
	// Reason is what the API said, printed so the person knows why the
	// browser opened rather than wondering whether their session broke.
	Reason string

	// AccessToken is the credential being re-proved. Sent so the server binds
	// the challenge to this account and nobody else can answer it.
	AccessToken string
}

// Authorize begins a device flow.
//
// deviceName is what the console shows beside this session in somebody's list
// of devices, and on the page where they approve it. Sent because the
// alternative is a list of sessions that all look the same, and a person who
// cannot tell which one to revoke revokes none of them.
func (c *Client) Authorize(ctx context.Context, scopes []string, step *StepUp, deviceName string) (*DeviceAuth, error) {
	form := url.Values{
		"client_id": {ClientID},
	}

	if len(scopes) > 0 {
		form.Set("scope", strings.Join(scopes, " "))
	}

	if deviceName != "" {
		form.Set("device_name", deviceName)
	}

	var headers map[string]string

	if step != nil {
		// Zero, not a small number: the point is that authentication must
		// happen now, in front of whoever is asking, and any tolerance is a
		// window in which a walked-away-from session answers for them.
		form.Set("max_age", "0")

		// The credential being re-proved, so the server can record which
		// account must answer the challenge. Without it a step-up could be
		// satisfied by whoever happens to be signed in on the browser the
		// code was pasted into.
		if step.AccessToken != "" {
			headers = map[string]string{"Authorization": "Bearer " + step.AccessToken}
		}
	}

	var auth DeviceAuth
	if err := c.postWithHeaders(ctx, deviceAuthorizationPath, form, headers, &auth); err != nil {
		return nil, err
	}

	if auth.DeviceCode == "" || auth.UserCode == "" || auth.VerificationURI == "" {
		return nil, fmt.Errorf("the control plane's device authorization response is missing a required field")
	}

	return &auth, nil
}

// Poll waits for the person to finish in the browser.
//
// The four device-flow codes are handled here and nowhere else, because
// three of them are not errors and one of them changes the cadence:
//
//	authorization_pending  they have not finished yet — keep waiting
//	slow_down              polling too fast — add five seconds, permanently
//	access_denied          they said no — stop, and do not call it a failure
//	expired_token          they took too long — stop, and say to try again
//
// onWait is called before each sleep, so a caller can animate without this
// package knowing anything about terminals.
func (c *Client) Poll(ctx context.Context, auth *DeviceAuth, onWait func(waited time.Duration)) (*Credentials, error) {
	interval := auth.PollInterval()
	deadline := auth.Deadline(time.Now())
	started := time.Now()

	for {
		if onWait != nil {
			onWait(time.Since(started))
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the sign-in code expired before it was used; run the command again")
		}

		creds, err := c.exchange(ctx, url.Values{
			"grant_type":  {grantDeviceCode},
			"device_code": {auth.DeviceCode},
			"client_id":   {ClientID},
		})
		if err == nil {
			return creds, nil
		}

		var oauthErr *OAuthError
		if !errors.As(err, &oauthErr) {
			// A transport failure mid-poll. Not fatal: a laptop that slept,
			// a proxy that dropped one connection, a control plane being
			// deployed. The device code is still good until its deadline,
			// so the loop is the retry.
			continue
		}

		switch oauthErr.Code {
		case errAuthorizationPending:
			continue
		case errSlowDown:
			// The RFC says to increase the interval, and says nothing about
			// decreasing it again — so this is permanent for the flow.
			interval += 5 * time.Second
			continue
		case errAccessDenied:
			return nil, fmt.Errorf("sign-in was declined in the browser")
		case errExpiredToken:
			return nil, fmt.Errorf("the sign-in code expired before it was used; run the command again")
		default:
			return nil, err
		}
	}
}

// Refresh exchanges a refresh token for a new access token.
//
// The refresh token that comes back, if one does, replaces the one used:
// rotation is what makes a stolen refresh token detectable, and a client that
// keeps the old one defeats it.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*Credentials, error) {
	return c.exchange(ctx, url.Values{
		"grant_type":    {grantRefreshToken},
		"refresh_token": {refreshToken},
		"client_id":     {ClientID},
	})
}

// Revoke tells the server to forget a token. RFC 7009.
//
// Best effort by design: `vallic logout` must clear the local credential
// whether or not the network is reachable, because a person on a train
// closing their laptop wants the token gone from the disk in front of them.
// The caller drops the error and says so.
func (c *Client) Revoke(ctx context.Context, token string) error {
	return c.post(ctx, revokePath, url.Values{
		"token":     {token},
		"client_id": {ClientID},
	}, nil)
}

// tokenResponse is RFC 6749 §5.1, plus the one field that makes a step-up
// answerable.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`

	// AuthTime is when the person authenticated, seconds since the epoch.
	// Not in RFC 6749 — it is OIDC's, and it is here because "this token was
	// issued recently" and "this person proved who they are recently" are
	// different claims and the second is the one a production deploy needs.
	AuthTime int64 `json:"auth_time"`

	// Acr is how strong that authentication was.
	Acr string `json:"acr"`

	// Subject is the account the credential belongs to. Read so that a
	// step-up can refuse a credential that came back belonging to somebody
	// else — the whole point of re-confirming is knowing who answered.
	Subject string `json:"sub"`
}

// exchange posts to the token endpoint and reads a credential out of it.
func (c *Client) exchange(ctx context.Context, form url.Values) (*Credentials, error) {
	var res tokenResponse
	if err := c.post(ctx, tokenPath, form, &res); err != nil {
		return nil, err
	}

	if res.AccessToken == "" {
		return nil, fmt.Errorf("the control plane returned no access token")
	}

	// Checked rather than assumed. A server answering with a token type this
	// client cannot use should say so here, not produce 401s later that look
	// like an expired credential.
	if res.TokenType != "" && !strings.EqualFold(res.TokenType, "bearer") {
		return nil, fmt.Errorf("unsupported token type %q", res.TokenType)
	}

	creds := &Credentials{
		Kind:         KindOAuth,
		API:          c.BaseURL,
		AccessToken:  res.AccessToken,
		RefreshToken: res.RefreshToken,
		Scope:        res.Scope,
	}

	if res.ExpiresIn > 0 {
		creds.Expiry = time.Now().Add(time.Duration(res.ExpiresIn) * time.Second)
	}

	if res.AuthTime > 0 {
		creds.AuthTime = time.Unix(res.AuthTime, 0)
	}

	creds.Acr = res.Acr
	creds.Subject = res.Subject

	return creds, nil
}

// post sends a form and decodes the answer, turning an OAuth error response
// into an *OAuthError whatever status carried it.
func (c *Client) post(ctx context.Context, path string, form url.Values, out any) error {
	return c.postWithHeaders(ctx, path, form, nil, out)
}

// postWithHeaders is post, with extra headers.
//
// Only the device authorization request needs any, and only for a step-up —
// so the common path keeps the shorter signature.
func (c *Client) postWithHeaders(
	ctx context.Context,
	path string,
	form url.Values,
	headers map[string]string,
	out any,
) error {
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.BaseURL+path,
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	for name, value := range headers {
		req.Header.Set(name, value)
	}

	res, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", c.BaseURL, err)
	}
	defer res.Body.Close()

	// Capped. An authorization endpoint answering with a megabyte is an
	// authorization endpoint that is actually a login page or a proxy's
	// error, and reading all of it to say so helps nobody.
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("cannot read the response from %s: %w", path, err)
	}

	if res.StatusCode >= 400 {
		oauthErr := &OAuthError{Status: res.StatusCode}

		if err := json.Unmarshal(body, oauthErr); err == nil && oauthErr.Code != "" {
			return oauthErr
		}

		// Not JSON, or JSON without an `error`. Almost always a reverse proxy
		// or a control plane that has no OAuth endpoints yet, and the status
		// is the only true thing available to say.
		return fmt.Errorf("%s answered %s", path, res.Status)
	}

	if out == nil {
		return nil
	}

	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s returned a response this version cannot read: %w", path, err)
	}

	return nil
}
