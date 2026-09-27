// Package api talks to the Vallic Cloud user API.
//
// Every method here is one route on the control plane, and the types are the
// control plane's own field names. That is deliberate: `--format json` is a
// contract, and a client that renamed fields on the way through would make
// the CLI's output and the API's documentation two things to keep in step.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vallic/vallic-cli/internal/auth"
)

// Prefix is where the user API lives. Versioned because its payloads will
// change; the OAuth endpoints beside it are not, because the RFCs fix them.
const Prefix = "/api/vc/v1"

// Client is an authenticated connection to one control plane.
type Client struct {
	baseURL string
	tokens  auth.Source
	http    *http.Client

	// UserAgent identifies the binary and its version in the control plane's
	// logs, which is how "every failure is from one old version" becomes a
	// question somebody can answer.
	UserAgent string
}

// New builds a client.
func New(baseURL string, tokens auth.Source, userAgent string) *Client {
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		tokens:    tokens,
		UserAgent: userAgent,
		// Generous, because a deploy request queues work and a control plane
		// under load answers slowly rather than not at all. Streaming a log
		// sets its own.
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

// BaseURL is the control plane this client talks to.
func (c *Client) BaseURL() string {
	return c.baseURL
}

// Error is a refusal from the control plane.
//
// The API's envelope is `{"error": {"code": "…", "message": "…"}}` with a
// stable machine code, which is the half worth keeping: the message is for
// the person and the code is what the CLI may branch on.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"-"`

	// RetryAfter is what the response's Retry-After header asked for, or zero
	// where it said nothing or said something unreadable.
	//
	// Not part of the envelope: it is a header, and often not one of ours. The
	// control plane's own rate limiter deliberately sends none, because the
	// flood backend cannot say when a window frees — but a proxy, a CDN or a
	// WAF in front of it will, and that is the case where guessing is worst,
	// because those limits are not the ones this client reasoned about.
	RetryAfter time.Duration `json:"-"`
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}

	if e.Code != "" {
		return e.Code
	}

	return fmt.Sprintf("the control plane answered %d", e.Status)
}

// The codes the CLI acts on rather than merely prints.
const (
	// CodeReauthenticationRequired is the one that matters most: the request
	// was understood, the credential is valid, and the action needs the
	// person to prove who they are again — a production deploy by an owner
	// of a team with a protected environment. A personal access token cannot
	// answer it, which is the whole argument for the device grant.
	CodeReauthenticationRequired = "reauthentication_required"

	// CodeEnvironmentBusy is a deployment already in flight. 409 rather than
	// 400 because nothing about the request was wrong, so a retry loop
	// should wait rather than change what it asked for.
	CodeEnvironmentBusy = "environment_busy"

	// CodeNoRelease is a project with nothing ready to deploy.
	CodeNoRelease = "no_release"

	// CodeUnauthorized is a credential the control plane does not accept.
	CodeUnauthorized = "unauthorized"

	// CodeRateLimited is one token asking too often -- 600 requests a minute,
	// counted per token rather than per person, so a runaway CI job does not
	// lock its team out of the console. The message says to wait and repeat
	// the request unchanged, and that is exactly what a poll should do.
	CodeRateLimited = "rate_limited"

	// CodeTooManyAttempts is too many *failed* credentials from one address:
	// twenty in fifteen minutes. A different thing entirely, and the
	// difference matters to what the CLI says -- the credential in hand may
	// be perfectly good, and retrying is the one thing that will not help.
	CodeTooManyAttempts = "too_many_attempts"
)

// IsRateLimited reports whether the control plane asked for a pause.
//
// By status rather than by code, because both limits answer 429 and a poll
// should back off for either -- and because a 429 from something in front of
// the control plane, an edge proxy or a CDN, carries no code of ours at all
// and means the same thing.
func IsRateLimited(err error) bool {
	var apiErr *Error

	return errors.As(err, &apiErr) && apiErr.Status == http.StatusTooManyRequests
}

// IsGuessingRefusal reports whether an address has spent its failed attempts.
//
// Separated from IsRateLimited because the advice is opposite: waiting fixes a
// volume limit, and for this one the credential itself is usually the problem.
func IsGuessingRefusal(err error) bool {
	var apiErr *Error

	return errors.As(err, &apiErr) && apiErr.Code == CodeTooManyAttempts
}

// NeedsReauthentication reports whether an error is a step-up challenge.
//
// Checked by name rather than by status, because 403 is also what an
// ordinary permission refusal is, and re-running a browser flow at somebody
// who simply may not do the thing would be a loop they cannot escape.
func NeedsReauthentication(err error) bool {
	var apiErr *Error

	return errors.As(err, &apiErr) && apiErr.Code == CodeReauthenticationRequired
}

// IsUnauthorized reports whether the credential was rejected.
func IsUnauthorized(err error) bool {
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		return false
	}

	return apiErr.Status == http.StatusUnauthorized || apiErr.Code == CodeUnauthorized
}

// Code returns an error's machine code, or "".
func Code(err error) string {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}

	return ""
}

// get reads a route into out.
func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// post sends a JSON body, or none at all.
//
// A nil body becomes `{}` rather than an empty request: the deploy route
// reads "deploy the newest ready release" out of an empty object, and a
// request with no body at all is a different thing to a JSON parser.
func (c *Client) post(ctx context.Context, path string, body any, out any) error {
	encoded := []byte("{}")

	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}

	return c.do(ctx, http.MethodPost, path, encoded, out)
}

// delete removes a resource.
//
// No body, in either direction beyond the envelope — a DELETE that carried a
// request body would be one some proxies drop.
func (c *Client) delete(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodDelete, path, nil, out)
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	return c.send(ctx, method, path, body, out, false)
}

// getPublic reads a route that does not require a credential.
//
// One route does: the CLI's own release manifest, which is asked for by a
// client that has not signed in and often by one that never will. Refusing it
// for want of a credential would make `vallic self-update` unusable by the
// person most likely to need it, and the manifest carries a version number and
// a public download URL, which are not secrets.
//
// It still SENDS a credential where there is one, rather than never sending
// one: a request from somebody signed in should look like every other request
// they make, and the route becoming authenticated later should not break this.
func (c *Client) getPublic(ctx context.Context, path string, out any) error {
	return c.send(ctx, http.MethodGet, path, nil, out, true)
}

// send performs the request. optionalAuth tolerates having no credential.
func (c *Client) send(ctx context.Context, method, path string, body []byte, out any, optionalAuth bool) error {
	token, err := c.tokens.Token()
	if err != nil && !optionalAuth {
		return err
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+Prefix+path, reader)
	if err != nil {
		return err
	}

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := c.http.Do(req)
	if err != nil {
		// Wrapped with the address, because the most common cause is a
		// VALLIC_API pointing somewhere that is not a control plane, and the
		// value is the one thing the person needs to see to spot that.
		return fmt.Errorf("cannot reach %s: %w", c.baseURL, err)
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("cannot read the response from %s: %w", path, err)
	}

	if res.StatusCode >= 400 {
		return decodeError(res, raw)
	}

	if out == nil {
		return nil
	}

	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s returned a response this version cannot read: %w", path, err)
	}

	return nil
}

// decodeError reads the API's error envelope, or falls back to the status.
func decodeError(res *http.Response, raw []byte) error {
	var envelope struct {
		Error *Error `json:"error"`
	}

	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Error != nil {
		envelope.Error.Status = res.StatusCode
		envelope.Error.RetryAfter = retryAfter(res.Header.Get("Retry-After"), time.Now())

		return envelope.Error
	}

	// No envelope. Either something in front of the control plane answered —
	// a proxy, a WAF, a maintenance page — or this route predates the
	// envelope. The status is the only true thing available.
	message := strings.TrimSpace(string(raw))
	if len(message) > 200 || strings.Contains(message, "<") {
		// An HTML error page. Printing it into a terminal is worse than
		// printing nothing, and the status already says what happened.
		message = ""
	}

	return &Error{
		Status:     res.StatusCode,
		Message:    strings.TrimSpace(fmt.Sprintf("the control plane answered %s. %s", res.Status, message)),
		RetryAfter: retryAfter(res.Header.Get("Retry-After"), time.Now()),
	}
}

// retryAfter reads a Retry-After header, in either form RFC 9110 allows.
//
// Delta-seconds (`120`) and an HTTP-date (`Wed, 21 Oct 2026 07:28:00 GMT`) are
// both legal and both are seen in the wild: origin servers tend to send the
// first and CDNs the second. Parsed here rather than at the caller so there is
// one place that is wrong if it is wrong.
//
// Zero for anything this cannot read, which is the honest answer and lets a
// caller fall back to its own schedule. Zero also for a date in the past: a
// clock skewed the wrong way would otherwise produce a negative wait, and a
// header that has already elapsed is asking for nothing.
//
// `now` is a parameter so this is testable without the clock.
func retryAfter(header string, now time.Time) time.Duration {
	header = strings.TrimSpace(header)

	if header == "" {
		return 0
	}

	if seconds, err := strconv.Atoi(header); err == nil {
		if seconds <= 0 {
			return 0
		}

		return time.Duration(seconds) * time.Second
	}

	when, err := http.ParseTime(header)
	if err != nil {
		return 0
	}

	if wait := when.Sub(now); wait > 0 {
		return wait
	}

	return 0
}
