package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/vallic/vallic-cli/internal/api"
	"github.com/vallic/vallic-cli/internal/auth"
)

// loginCommand signs in.
func loginCommand() *Command {
	var (
		withToken bool
		noBrowser bool
		label     string
	)

	return &Command{
		Name:    "login",
		Summary: "sign in to a control plane",
		Usage:   "login [--token] [--no-browser]",
		Long: `Opens a browser to authenticate, and stores a short-lived token
that the CLI refreshes as it works.

The browser is where the sign-in happens because that is where the session,
the password manager and — when the console gains one — the second factor all
already are. The CLI never sees a password or a one-time code. And when an
action has to be confirmed again, such as deploying a protected environment,
this is the only credential that can answer.

--token reads a personal access token instead, for CI. A token carries none of
the sign-in that was completed to mint it, cannot be asked to confirm
anything, and can do everything its owner may do in the team it was minted
for. Prefer it only where there is no browser.`,
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&withToken, "token", false, "read a personal access token instead of opening a browser")
			fs.BoolVar(&noBrowser, "no-browser", false, "print the URL rather than opening it")
			fs.StringVar(&label, "label", "", "how this device appears in the console (default: this machine's hostname)")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			if len(args) > 0 {
				return Usagef(nil, "login takes no arguments; did you mean --token?")
			}

			if withToken {
				return loginWithToken(ctx, env, label)
			}

			return loginWithBrowser(ctx, env, label, noBrowser)
		},
	}
}

// loginWithBrowser runs the device authorization grant.
func loginWithBrowser(ctx context.Context, env *Env, label string, noBrowser bool) error {
	cfg, err := env.Config()
	if err != nil {
		return err
	}

	client, err := env.AuthClient()
	if err != nil {
		return err
	}

	name := deviceLabel(label)

	creds, err := runDeviceFlow(ctx, env, client, nil, noBrowser, name)
	if err != nil {
		return err
	}

	creds.Label = name
	creds.API = cfg.API

	return finishLogin(ctx, env, creds)
}

// runDeviceFlow does the browser half, and is shared with a step-up.
func runDeviceFlow(
	ctx context.Context,
	env *Env,
	client *auth.Client,
	step *auth.StepUp,
	noBrowser bool,
	deviceName string,
) (*auth.Credentials, error) {
	device, err := client.Authorize(ctx, nil, step, deviceName)
	if err != nil {
		return nil, describeAuthFailure(err, client.BaseURL)
	}

	// The complete URI first, because a code typed by hand is a code
	// mistyped by hand. The plain one and the code are printed too: a
	// browser on another machine is the whole reason this grant exists.
	target := device.VerificationURIComplete
	if target == "" {
		target = device.VerificationURI
	}

	opened := false
	if !noBrowser && env.Interactive() {
		opened = openBrowser(target) == nil
	}

	if opened {
		env.Printer.Say("Opened %s", target)
		env.Printer.Say("Confirm the code shown there: %s", device.UserCode)
	} else {
		env.Printer.Say("Open %s", device.VerificationURI)
		env.Printer.Say("and enter the code: %s", device.UserCode)
	}

	env.Printer.Say("")
	env.Printer.Say("Waiting for you to finish in the browser… (ctrl-c to stop)")

	creds, err := client.Poll(ctx, device, nil)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, fmt.Errorf("sign-in stopped before it finished")
		}

		return nil, err
	}

	return creds, nil
}

// Reauthenticate answers a step-up challenge.
//
// Called by WithStepUp when the control plane says an action needs the person
// to prove who they are now. The credential that comes back replaces the one
// on disk, so a second deploy in the same minute does not ask again.
func (e *Env) Reauthenticate(ctx context.Context, reason string) error {
	client, err := e.AuthClient()
	if err != nil {
		return err
	}

	previous, err := e.Credentials()
	if err != nil {
		return err
	}

	e.Printer.Warn("%s", reason)

	// The token being re-proved travels with the request, so the server binds
	// the challenge to this account. Without it a step-up could be answered by
	// whoever is signed in on whatever browser the code reached.
	previousToken, tokenErr := e.source.Token()
	if tokenErr != nil {
		previousToken = ""
	}

	creds, err := runDeviceFlow(
		ctx,
		e,
		client,
		&auth.StepUp{Reason: reason, AccessToken: previousToken},
		false,
		previous.Label,
	)
	if err != nil {
		return err
	}

	// Compared on the subject the server stated, not on the label or the
	// account name this client already held — the point of re-confirming is
	// knowing who answered, and only the server can say. Refused rather than
	// stored: a confirmation completed by somebody else is not a confirmation.
	if previous.Subject != "" && creds.Subject != "" && creds.Subject != previous.Subject {
		return fmt.Errorf(
			"that confirmation was completed by a different account; nothing has been changed",
		)
	}

	creds.Label = previous.Label
	creds.Account = previous.Account
	creds.API = previous.API

	if creds.Subject == "" {
		creds.Subject = previous.Subject
	}

	if err := creds.Save(); err != nil {
		return err
	}

	// Rebuilt rather than mutated: the client holds a Source built around the
	// old credential, and a request made through it would send the token
	// that was just refused.
	e.creds = creds
	e.client = nil
	e.source = nil

	if _, err := e.Client(); err != nil {
		return err
	}

	e.Printer.Good("Confirmed.")

	return nil
}

// loginWithToken reads a personal access token.
func loginWithToken(ctx context.Context, env *Env, label string) error {
	cfg, err := env.Config()
	if err != nil {
		return err
	}

	token := strings.TrimSpace(os.Getenv("VALLIC_TOKEN"))

	if token == "" {
		if !env.Interactive() {
			// Named together, because in CI the answer is the variable and
			// on a terminal the answer is the prompt, and a message that
			// only mentions one sends half the readers the wrong way.
			return fmt.Errorf("no token to read: set VALLIC_TOKEN, or run this on a terminal to be prompted")
		}

		// Prompted rather than taken as a flag. A flag that takes a secret
		// puts it in shell history and in the output of `ps`, where every
		// other account on the machine can read it.
		env.Printer.Say("Paste a personal access token from %s", tokensPage(cfg.API))
		env.Printer.Say("(it will not be shown)")

		token, err = readSecret(env)
		if err != nil {
			return err
		}
	}

	if token == "" {
		return fmt.Errorf("no token given")
	}

	if !strings.HasPrefix(token, "vcp_") {
		// A warning and not a refusal: the prefix is the platform's
		// convention today and a CLI that hard-refused would need a release
		// to accept a new one. But a token pasted one line short is the
		// common mistake, and saying so costs nothing.
		env.Printer.Warn("that does not look like a personal access token (they start with vcp_)")
	}

	return finishLogin(ctx, env, &auth.Credentials{
		Kind:        auth.KindToken,
		API:         cfg.API,
		AccessToken: token,
		Label:       deviceLabel(label),
	})
}

// finishLogin verifies a credential before storing it, then stores it.
//
// Verified first, deliberately. Writing a token that does not work and
// reporting success would move the error to whatever the person ran next,
// which is the command they would then think was broken.
func finishLogin(ctx context.Context, env *Env, creds *auth.Credentials) error {
	client := api.New(creds.API, auth.NewStatic(creds.AccessToken), env.UserAgent())

	me, err := client.Me(ctx)
	if err != nil {
		if api.IsUnauthorized(err) {
			// Three things look identical from here: revoked, mistyped, and
			// run out. Expiry is named because it is the one that happens to
			// a credential that worked yesterday, and because it is the only
			// one somebody would not already suspect.
			return fmt.Errorf(
				"%s did not accept that credential\n"+
					"  it may have been revoked, mistyped, or run out: a token lasts at most ninety days\n"+
					"  mint a new one at %s",
				creds.API, tokensPage(creds.API),
			)
		}

		return err
	}

	creds.Account = me.User.Email
	if creds.Account == "" {
		creds.Account = me.User.Name
	}

	// The deadline, recorded here because this is the only moment the CLI is
	// guaranteed to see it: somebody who signs in and never runs `whoami`
	// would otherwise learn their token had run out from a pipeline.
	recordTokenExpiry(creds, me)

	if err := creds.Save(); err != nil {
		return err
	}

	// Loaded, now that it is stored. Signing in is the first and best moment
	// to hear that a credential is nearly out: somebody pasting a token with
	// two days left has the console already open, and the alternative is
	// finding out from a pipeline on the day it stops.
	env.creds = creds

	// A token is minted for one team and narrows every request to it, so the
	// team is a fact about the credential rather than a choice. Recorded so
	// `whoami` and the resolver do not have to ask again.
	if me.Team != nil {
		cfg, cfgErr := env.Config()
		if cfgErr == nil {
			cfg.Team = me.Team.Label
			_ = cfg.Save()
		}
	}

	path, _ := auth.CredentialsPath()

	env.Printer.Good("Signed in to %s as %s", creds.API, creds.Account)
	if me.Team != nil {
		env.Printer.Say("  team: %s", me.Team.Label)
	}
	env.Printer.Say("  credential: %s, stored in %s", describeKind(creds), path)

	return nil
}

func describeKind(creds *auth.Credentials) string {
	if creds.Kind == auth.KindOAuth {
		if creds.Expiry.IsZero() {
			return "browser sign-in"
		}

		return fmt.Sprintf("browser sign-in, renewed automatically (expires %s)", creds.Expiry.Format(time.Kitchen))
	}

	return "personal access token"
}

// logoutCommand forgets the credential.
func logoutCommand() *Command {
	return &Command{
		Name:    "logout",
		Summary: "forget the stored credential",
		Usage:   "logout",
		Long: `Removes the credential from this machine, and asks the control
plane to revoke it.

The local file goes first and unconditionally. Somebody closing a laptop on a
train wants the token off the disk in front of them whether or not the network
is reachable, so a revocation that cannot be delivered is reported and not
treated as a failure.`,
		Run: func(ctx context.Context, env *Env, args []string) error {
			creds, err := env.Credentials()
			if errors.Is(err, auth.ErrNoCredential) {
				env.Printer.Say("Not signed in.")
				return nil
			}
			if err != nil {
				// Even an unreadable credential file should be removable by
				// the command whose job is removing it.
				if forgetErr := auth.Forget(); forgetErr != nil {
					return forgetErr
				}

				env.Printer.Good("Removed the stored credential.")

				return nil
			}

			if forgetErr := auth.Forget(); forgetErr != nil {
				return forgetErr
			}

			env.Printer.Good("Removed the stored credential.")

			if creds.Kind != auth.KindOAuth {
				// Nothing to revoke from here: a personal access token is
				// revoked where it was minted, and saying so is more use
				// than silence.
				env.Printer.Say("Revoke the token itself in the console: %s", tokensPage(creds.API))

				return nil
			}

			client, err := env.AuthClient()
			if err != nil {
				return nil
			}

			revoke := creds.RefreshToken
			if revoke == "" {
				revoke = creds.AccessToken
			}

			if err := client.Revoke(ctx, revoke); err != nil {
				env.Printer.Warn("could not tell %s to revoke the session: %v", creds.API, err)
				env.Printer.Say("  it will expire on its own, or sign out everywhere in the console")

				return nil
			}

			env.Printer.Good("Revoked the session.")

			return nil
		},
	}
}

// whoamiCommand says who the credential belongs to.
func whoamiCommand() *Command {
	return &Command{
		Name:    "whoami",
		Summary: "show who the stored credential belongs to",
		Usage:   "whoami",
		Run: func(ctx context.Context, env *Env, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}

			me, err := client.Me(ctx)
			if err != nil {
				return err
			}

			creds, _ := env.Credentials()

			// Refreshed from the answer just received, because this is the
			// one command that always asks. A token re-minted with a longer
			// life, or a control plane that has only now started saying,
			// would otherwise leave the CLI warning from a stale date.
			if creds != nil {
				before := creds.TokenExpiry

				recordTokenExpiry(creds, me)

				if !creds.TokenExpiry.Equal(before) {
					// Best effort. Failing to write it back is not a reason
					// to fail a command whose job is to answer a question.
					_ = creds.Save()
				}
			}

			if env.Printer.Structured() {
				// The credential's shape, not its value. Whether a token can
				// be stepped up is the thing worth reporting, and a script
				// gating on it should not have to read a sentence.
				return env.Printer.Value(map[string]any{
					"user": map[string]any{
						"id":    me.User.ID,
						"name":  me.User.Name,
						"email": me.User.Email,
					},
					"team":  me.Team,
					"token": me.Token,
					"credential": map[string]any{
						"api":           creds.API,
						"kind":          string(creds.Kind),
						"label":         creds.Label,
						"scope":         creds.Scope,
						"can_step_up":   creds.Kind == auth.KindOAuth,
						"expires":       nullableTime(creds.Expiry),
						"authenticated": nullableTime(creds.AuthTime),
					},
				})
			}

			env.Printer.Line("%s", me.User.Email)
			env.Printer.Say("  control plane: %s", creds.API)
			env.Printer.Say("  credential:    %s", describeKind(creds))

			if me.Team != nil {
				env.Printer.Say("  team:          %s (id %d)", me.Team.Label, me.Team.ID)
			} else {
				env.Printer.Say("  team:          not narrowed — every team you belong to")
			}

			if creds.Kind != auth.KindOAuth {
				env.Printer.Say("  confirm again:  no — a personal access token cannot be asked to")
			} else {
				env.Printer.Say("  confirm again:  yes, in the browser, when something asks")
			}

			// The deadline, spelled out with both the date and the distance.
			// The date is what goes in a calendar and the distance is what
			// makes somebody act, and a line carrying only one of them gets
			// read as the other.
			if left, known := creds.Remaining(time.Now()); known {
				if left <= 0 {
					env.Printer.Say("  runs out:      %s, which was %s",
						creds.TokenExpiry.Format("2 January 2006"), humanAgo(-left))
				} else {
					env.Printer.Say("  runs out:      %s, %s",
						creds.TokenExpiry.Format("2 January 2006"), humanIn(left))
				}
			}

			return nil
		},
	}
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}

	return t.UTC().Format(time.RFC3339)
}

// deviceLabel names this machine in the console's session list.
func deviceLabel(given string) string {
	if given != "" {
		return given
	}

	host, err := os.Hostname()
	if err != nil || host == "" {
		return "vallic-cli"
	}

	return host
}

// readSecret reads a line without echoing it.
//
// Done by asking the terminal driver to stop echoing, through `stty` on unix
// — the alternative is golang.org/x/term, which is a dependency for one
// call. Where that is not available the line is read with echo on and the
// person is told, because a prompt that silently echoes a pasted token is
// worse than one that admits it.
func readSecret(env *Env) (string, error) {
	restore, hidden := hideInput()
	if !hidden {
		env.Printer.Warn("cannot turn off echo on this terminal; what you paste will be visible")
	}

	defer restore()

	reader := bufio.NewReader(env.In)
	line, err := reader.ReadString('\n')

	if hidden {
		// The newline the person typed was not echoed either, so the next
		// output would land on the prompt's line.
		fmt.Fprintln(env.Err)
	}

	if err != nil && line == "" {
		return "", err
	}

	return strings.TrimSpace(line), nil
}

// hideInput turns terminal echo off, returning how to put it back.
func hideInput() (restore func(), ok bool) {
	if runtime.GOOS == "windows" {
		return func() {}, false
	}

	if err := sttyRun("-echo"); err != nil {
		return func() {}, false
	}

	return func() { _ = sttyRun("echo") }, true
}

func sttyRun(arg string) error {
	cmd := exec.Command("stty", arg)
	cmd.Stdin = os.Stdin

	return cmd.Run()
}

// openBrowser asks the desktop to open a URL.
func openBrowser(target string) error {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}

	// Detached from this process's streams: xdg-open on a bare server prints
	// its own complaints, and they would land in the middle of the code the
	// person is meant to be reading.
	cmd.Stdout = nil
	cmd.Stderr = nil

	return cmd.Start()
}

// describeAuthFailure turns a failure to even begin into something useful.
//
// The likely cause today is that the control plane has no OAuth endpoints
// yet, and a bare 404 sends people looking for a problem with their machine.
func describeAuthFailure(err error, baseURL string) error {
	var oauthErr *auth.OAuthError
	if errors.As(err, &oauthErr) {
		return err
	}

	if strings.Contains(err.Error(), "404") {
		return fmt.Errorf(
			"%s has no browser sign-in yet\n  use a personal access token instead: vallic login --token",
			baseURL,
		)
	}

	return err
}

// recordTokenExpiry copies the control plane's stated deadline onto the
// credential.
//
// Only where it said one. `/me` carries a token expiry for a `vcp_` and not
// for an OAuth session, which is not an access-token row, so an absent value
// means "this kind has no such deadline" rather than "it never runs out".
// Overwriting a known deadline with zero would quietly turn a warning off.
func recordTokenExpiry(creds *auth.Credentials, me *api.Me) {
	if me.Token == nil || me.Token.Expires == nil || *me.Token.Expires <= 0 {
		return
	}

	creds.TokenExpiry = time.Unix(*me.Token.Expires, 0)
}
