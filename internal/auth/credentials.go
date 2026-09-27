// Package auth owns the credential: how it is obtained, where it is kept,
// and how a command asks for one without learning which kind it is.
//
// Two kinds exist, and the reason both do is that they are for two different
// holders. A person at a terminal signs in through a browser, because that is
// where their session, their password manager and their second factor already
// are — and, once the console gates a production deploy on a second factor,
// it is the only place that gate can be satisfied. CI has no browser and no
// human, so it carries a personal access token, and a pipeline that could
// satisfy a check whose whole purpose is that somebody is watching would
// defeat the check rather than pass it.
//
// Every command asks Source.Token() and gets a string. That is the whole
// interface on purpose: a command that could tell a refreshable token from a
// permanent one would grow a second branch in its error handling, and the
// branch that is never exercised is the one that is wrong.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/vallic/vallic-cli/internal/config"
)

// Kind is how a credential was obtained.
type Kind string

const (
	// KindOAuth is a token from the device authorization grant. Short-lived,
	// refreshable, revocable per device, and the only kind that can be
	// stepped up.
	KindOAuth Kind = "oauth"

	// KindToken is a personal access token minted in the console. It carries
	// none of the second factor that was satisfied to mint it.
	KindToken Kind = "token"
)

// ErrNoCredential is returned when nothing is stored and nothing is in the
// environment. Commands turn this into exit code 3 and a line telling the
// person to run `vallic login`.
var ErrNoCredential = errors.New("not signed in")

// Source hands out a bearer token, refreshing it if that is what it takes.
//
// Nothing beyond this in the signature. See the package comment.
type Source interface {
	Token() (string, error)
}

// Credentials is what is written to credentials.json.
//
// Its own file at 0600, never config.json. See the config package.
type Credentials struct {
	Kind Kind `json:"kind"`

	// API is the control plane this credential is for. Stored beside it so
	// pointing the CLI at a different installation cannot silently send one
	// installation's token to another.
	API string `json:"api"`

	// AccessToken is the bearer string. For KindToken it is the `vcp_` token;
	// for KindOAuth it is short-lived and refreshed.
	AccessToken string `json:"access_token"`

	// RefreshToken is empty for KindToken, which has nothing to refresh.
	RefreshToken string `json:"refresh_token,omitempty"`

	// Expiry is when AccessToken stops being accepted. For an OAuth
	// credential that is a few minutes away and every refresh moves it, so it
	// is not a deadline anybody acts on. Zero where nothing stated one.
	Expiry time.Time `json:"expiry,omitzero"`

	// TokenExpiry is when the credential itself runs out, as the control
	// plane states it, and no refresh moves it.
	//
	// Separate from Expiry, which for OAuth is the access token's own few
	// minutes. This is the deadline behind that one: a personal access token
	// is minted for at most ninety days and there is no "never".
	//
	// Kept because nothing renews it. The platform decided against letting a
	// token mint its successor — a credential that can always replace itself
	// turns ninety days from a control into a liveness check, since a leaked
	// token rotates as happily as the real one. So a lapse is made survivable
	// by saying it is coming, which is what this field is for and the only
	// thing it is for.
	//
	// Zero where the control plane did not say, which is how an older one
	// answers and what an OAuth credential leaves it as: `/me` carries a
	// token expiry only for a `vcp_`, because an OAuth session is not one.
	TokenExpiry time.Time `json:"token_expiry,omitzero"`

	// Scope is what the authorization server granted, space-separated.
	Scope string `json:"scope,omitempty"`

	// AuthTime is when the person actually authenticated, as opposed to when
	// this token was issued. A refresh moves the expiry and leaves this
	// alone, which is what makes a step-up challenge answerable: the server
	// asks for a recent authentication, not a recent token.
	AuthTime time.Time `json:"auth_time,omitzero"`

	// Acr is how strong the authentication behind this credential was.
	Acr string `json:"acr,omitempty"`

	// Subject is the account id the server says this belongs to. Compared
	// after a step-up, so a credential that came back for somebody else is
	// refused rather than stored.
	Subject string `json:"subject,omitempty"`

	// Account is who this is, for `whoami` to print without a round trip and
	// for a second `login` to notice it is replacing somebody else.
	Account string `json:"account,omitempty"`

	// Label is how this device appears in the console's session list, so
	// revoking the right one does not require guessing.
	Label string `json:"label,omitempty"`
}

// Expired reports whether the access token is past its stated expiry.
//
// A minute of slack, because the alternative is sending a token that expires
// in flight and turning a refresh into a failed command. Erring early costs
// one extra refresh; erring late costs the user their command.
func (c *Credentials) Expired() bool {
	if c.Expiry.IsZero() {
		return false
	}

	return time.Now().Add(time.Minute).After(c.Expiry)
}

// CredentialsPath is where the credential lives.
func CredentialsPath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}

	return filepath.Join(dir, "credentials.json"), nil
}

// LoadCredentials reads the stored credential.
//
// Returns ErrNoCredential when there is none, which is a state and not a
// failure — it is what every first run is in.
func LoadCredentials() (*Credentials, error) {
	path, err := CredentialsPath()
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoCredential
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}

	// Refused rather than warned about. A warning that a credential file is
	// readable by every account on the machine is a warning everybody has
	// learned to scroll past, and the fix is one command. Skipped on Windows,
	// where the mode bits do not describe the ACL that actually applies and
	// refusing on them would refuse every correctly-secured file.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return nil, fmt.Errorf(
				"%s is readable by other accounts on this machine (mode %04o)\n"+
					"  fix it with: chmod 600 %s",
				path, perm, path,
			)
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}

	creds := &Credentials{}
	if err := json.Unmarshal(raw, creds); err != nil {
		// Not repaired and not deleted. Overwriting what might be somebody's
		// only copy of a working token, to fix a file the CLI could simply
		// describe, is a worse outcome than an error.
		return nil, fmt.Errorf("%s is not valid JSON: %w\n  sign in again to replace it: vallic login", path, err)
	}

	if creds.AccessToken == "" {
		return nil, ErrNoCredential
	}

	return creds, nil
}

// Save writes the credential at 0600.
func (c *Credentials) Save() error {
	path, err := CredentialsPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("cannot create %s: %w", filepath.Dir(path), err)
	}

	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	return writeAtomic(path, append(raw, '\n'), 0o600)
}

// Forget removes the stored credential.
//
// A missing file is success: `vallic logout` twice should not fail the second
// time, and neither should logging out having never logged in.
func Forget() error {
	path, err := CredentialsPath()
	if err != nil {
		return err
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cannot remove %s: %w", path, err)
	}

	return nil
}

// FromEnvironment is the credential a pipeline supplies, or nil.
//
// VALLIC_TOKEN beats anything on disk. A container that mounts a home
// directory it did not build should still use the token the pipeline set,
// and a developer debugging CI locally should be able to hold one for a
// single command without disturbing their own login.
func FromEnvironment(api string) *Credentials {
	token := strings.TrimSpace(os.Getenv("VALLIC_TOKEN"))
	if token == "" {
		return nil
	}

	return &Credentials{
		Kind:        KindToken,
		API:         api,
		AccessToken: token,
		Label:       "VALLIC_TOKEN",
	}
}

// writeAtomic is config.writeAtomic's twin; see the reasoning there.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("cannot write in %s: %w", dir, err)
	}
	defer os.Remove(tmp.Name())

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}

	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmp.Name(), path)
}

// Anonymous is a Source with nothing to hand out.
//
// For the one route that does not need a credential: the CLI's own release
// manifest. A binary that has never been signed in still has to be able to ask
// what the current version is, and a binary whose sign-in has stopped working
// is the case where asking matters most.
//
// Its own type rather than a nil Source, so a route reached without a
// credential is a deliberate choice at the call site and not a missing check.
type Anonymous struct{}

// Token hands out nothing, successfully.
func (Anonymous) Token() (string, error) { return "", nil }

// Remaining is how long this credential has left, and whether that is known.
//
// The second return distinguishes "no deadline stated" from "no time left",
// which a bare zero could not: an older control plane and a token that ran
// out an hour ago are different answers and lead to different advice.
func (c *Credentials) Remaining(now time.Time) (time.Duration, bool) {
	if c.TokenExpiry.IsZero() {
		return 0, false
	}

	return c.TokenExpiry.Sub(now), true
}

// Lapsed reports whether the stated deadline has passed.
func (c *Credentials) Lapsed(now time.Time) bool {
	left, known := c.Remaining(now)

	return known && left <= 0
}
