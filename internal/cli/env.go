package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/vallic/vallic-cli/internal/api"
	"github.com/vallic/vallic-cli/internal/auth"
	"github.com/vallic/vallic-cli/internal/config"
	"github.com/vallic/vallic-cli/internal/output"
	"github.com/vallic/vallic-cli/internal/terminal"
)

// Exit codes.
//
// Six rather than one, because `--wait` in a pipeline depends on telling them
// apart: "we would not deploy that" and "we deployed it and it broke" are
// different outcomes, and a single non-zero code makes every CI script parse
// text to find out which happened.
const (
	// ExitOK means the command did what was asked. For --wait, the work also
	// succeeded.
	ExitOK = 0

	// ExitRefused means the platform said no: no such environment, no
	// release ready, a quota, a permission.
	ExitRefused = 1

	// ExitUsage means the command was wrong: an unknown flag, a missing
	// argument, an ambiguous target.
	ExitUsage = 2

	// ExitUnauthenticated means there is no credential, or it has expired.
	ExitUnauthenticated = 3

	// ExitTaskFailed means --wait finished and the work failed. Separate
	// from ExitRefused on purpose; see above.
	ExitTaskFailed = 4

	// ExitUnreachable means the control plane could not be reached at all.
	ExitUnreachable = 5
)

// TaskFailedError is returned by a --wait that watched work fail.
//
// Its own type so the exit code can be 4 without every command threading a
// boolean back up.
type TaskFailedError struct {
	Task *api.Task
}

func (e *TaskFailedError) Error() string {
	if e.Task.Message != "" {
		return fmt.Sprintf("%s failed: %s", e.Task.Type, e.Task.Message)
	}

	return fmt.Sprintf("%s failed", e.Task.Type)
}

// Env is what every command is handed: where to write, what to talk to, and
// the credential — resolved lazily, because `login` runs without one and
// `--help` must run without touching the disk at all.
type Env struct {
	// Version is this binary's version, for the User-Agent and `--version`.
	Version string

	Out io.Writer
	Err io.Writer
	In  io.Reader

	// Config is loaded on the first command that needs it.
	config *config.Config

	// client is built on the first command that makes a request.
	client *api.Client

	// source is the credential behind client, kept so a step-up can reach
	// the account it is re-authenticating.
	source auth.Source

	// creds is what was loaded from disk or the environment.
	creds *auth.Credentials

	// Global flag values.
	flagFormat      string
	flagAPI         string
	flagTeam        string
	flagProject     string
	flagEnvironment string
	flagQuiet       bool
	flagNoInput     bool

	// command is the command Execute resolved, and path how it was reached.
	// Used only to point a usage error at the right help.
	command *Command
	path    []string

	// passthrough is everything after a bare "--", and hasPassthrough says
	// whether there was one at all. Split out before the flags are parsed;
	// see splitPassthrough.
	passthrough    []string
	hasPassthrough bool

	// Printer writes results in the chosen format.
	Printer *output.Printer
}

// Passthrough is what followed a bare "--", and whether there was one.
func (e *Env) Passthrough() ([]string, bool) {
	return e.passthrough, e.hasPassthrough
}

// NewEnv builds an Env around the real process.
func NewEnv(version string) *Env {
	return &Env{
		Version: version,
		Out:     os.Stdout,
		Err:     os.Stderr,
		In:      os.Stdin,
	}
}

// registerGlobals adds the flags every command accepts.
//
// On every command's own FlagSet rather than parsed before dispatch, so
// `vallic env list --format json` works as well as `vallic --format json env
// list` — and so `--help` on any command lists them.
func (e *Env) registerGlobals(fs *flag.FlagSet) {
	fs.StringVar(&e.flagFormat, "format", "", "output as table, json, yaml or csv")
	fs.StringVar(&e.flagAPI, "api", "", "the control plane to talk to")
	fs.StringVar(&e.flagTeam, "team", "", "the team to act in")
	fs.StringVar(&e.flagProject, "project", "", "the project, when the checkout cannot say")
	fs.StringVar(&e.flagEnvironment, "environment", "", "the environment, when the branch cannot say")
	fs.BoolVar(&e.flagQuiet, "quiet", false, "print only what was asked for")
	fs.BoolVar(&e.flagNoInput, "no-input", false, "never prompt; fail instead")
}

// applyGlobals settles the output format once flags are parsed.
func (e *Env) applyGlobals() error {
	cfg, err := e.Config()
	if err != nil {
		return err
	}

	format := e.flagFormat
	if format == "" {
		format = cfg.Format
	}

	printer, err := output.NewPrinter(e.Out, e.Err, format)
	if err != nil {
		return &UsageError{Err: err}
	}

	printer.Quiet = e.flagQuiet
	e.Printer = printer

	return nil
}

// Config returns the configuration, with flags layered over it.
func (e *Env) Config() (*config.Config, error) {
	if e.config != nil {
		return e.config, nil
	}

	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}

	// A flag beats the environment, which beat the file inside Load. One
	// order, applied in two places because the flags are not parsed yet when
	// the file is read.
	if e.flagAPI != "" {
		cfg.API = e.flagAPI
	}
	if e.flagTeam != "" {
		cfg.Team = e.flagTeam
	}

	e.config = cfg

	return cfg, nil
}

// Interactive reports whether the CLI may prompt.
//
// False with --no-input, and false whenever stdin is not a terminal — a
// prompt in a pipeline is a build that hangs for twenty minutes and then
// fails on a timeout, which is much worse than failing at once with the flag
// that would have settled it.
func (e *Env) Interactive() bool {
	if e.flagNoInput {
		return false
	}

	file, ok := e.In.(*os.File)
	if !ok {
		return false
	}

	return terminal.IsTerminal(file)
}

// Credentials returns the stored credential.
//
// VALLIC_TOKEN wins over the disk: a container that mounts a home directory
// it did not build should use the token the pipeline set.
func (e *Env) Credentials() (*auth.Credentials, error) {
	if e.creds != nil {
		return e.creds, nil
	}

	cfg, err := e.Config()
	if err != nil {
		return nil, err
	}

	if fromEnv := auth.FromEnvironment(cfg.API); fromEnv != nil {
		e.creds = fromEnv
		return e.creds, nil
	}

	creds, err := auth.LoadCredentials()
	if err != nil {
		return nil, err
	}

	// A credential minted against one control plane is not sent to another.
	// Without this, pointing --api at a staging installation would hand it a
	// production token — which it would log, and reasonably so.
	if creds.API != "" && creds.API != cfg.API {
		return nil, fmt.Errorf(
			"%w: the stored credential is for %s, not %s\n  sign in there with: vallic login --api %s",
			auth.ErrNoCredential, creds.API, cfg.API, cfg.API,
		)
	}

	e.creds = creds

	return creds, nil
}

// Client returns an authenticated API client.
func (e *Env) Client() (*api.Client, error) {
	if e.client != nil {
		return e.client, nil
	}

	cfg, err := e.Config()
	if err != nil {
		return nil, err
	}

	creds, err := e.Credentials()
	if err != nil {
		return nil, err
	}

	e.source = auth.SourceFor(auth.NewClient(cfg.API), creds)
	e.client = api.New(cfg.API, e.source, e.UserAgent())

	return e.client, nil
}

// PublicClient returns a client for the routes that need no credential.
//
// The signed-in client where there is one, so a request from somebody with a
// credential looks like every other request they make and the control plane's
// logs can attribute it. An anonymous one otherwise, rather than the refusal
// Client() gives: `vallic self-update` has to work for somebody who has never
// run `vallic login`, and for somebody whose sign-in is the thing that broke.
func (e *Env) PublicClient() (*api.Client, error) {
	if client, err := e.Client(); err == nil {
		return client, nil
	}

	cfg, err := e.Config()
	if err != nil {
		return nil, err
	}

	return api.New(cfg.API, auth.Anonymous{}, e.UserAgent()), nil
}

// UserAgent identifies this binary to the control plane.
func (e *Env) UserAgent() string {
	return "vallic-cli/" + e.Version
}

// AuthClient returns a client for the authorization server.
func (e *Env) AuthClient() (*auth.Client, error) {
	cfg, err := e.Config()
	if err != nil {
		return nil, err
	}

	return auth.NewClient(cfg.API), nil
}

// WithStepUp runs an action, and re-authenticates once if the control plane
// asks it to.
//
// This is the reason the device grant is the login path rather than a nicer
// way to paste a token. One action can demand that the person asking prove
// who they are *now* — `reauthentication_required` — and the only credential
// that can answer is one obtained by authenticating in a browser, where the
// second factor lives.
//
// One action: restoring a backup over a protected production environment.
// Deploying, rolling back and redeploying were gated the same way until
// 2026-09-20 and are not any more, because each of them moves a pointer at
// code and is the thing you do *during* an incident. A restore overwrites the
// database that is serving customers, which is the act the rule was always
// really for.
//
// Kept general rather than folded into the restore command: what it does is
// answer a challenge, and which actions challenge is the control plane's to
// decide. A second caller should wrap itself in this rather than grow its own
// copy of the retry.
//
// Exactly one retry. A server that asks again after a fresh authentication is
// a server that will ask forever, and a loop that opens a browser every five
// seconds is worse than an error.
func (e *Env) WithStepUp(ctx context.Context, action func() error) error {
	err := action()
	if !api.NeedsReauthentication(err) {
		return err
	}

	creds, credErr := e.Credentials()
	if credErr != nil {
		return err
	}

	if creds.Kind != auth.KindOAuth {
		// A personal access token has nothing to challenge and no way to
		// answer. Said plainly, because the alternative is a permission
		// error that looks like a bug in the platform — and because a
		// pipeline reading this should learn that the environment is one a
		// human deploys, not that its token is broken.
		return fmt.Errorf(
			"%w\n  this has to be confirmed by a person at a browser, which a personal\n"+
				"  access token cannot do.\n"+
				"  run it from a terminal where you can sign in: vallic login",
			err,
		)
	}

	if !e.Interactive() {
		return fmt.Errorf(
			"%w\n  this has to be confirmed, and there is no terminal to ask on.\n"+
				"  a restore over a protected environment is one a person asks for,\n"+
				"  not a pipeline.",
			err,
		)
	}

	var apiErr *api.Error
	reason := "this has to be confirmed before it can go ahead"
	if errors.As(err, &apiErr) && apiErr.Message != "" {
		reason = apiErr.Message
	}

	if stepErr := e.Reauthenticate(ctx, reason); stepErr != nil {
		return stepErr
	}

	return action()
}
