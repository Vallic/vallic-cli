# vallic

The command line for [Vallic Cloud](https://vallic.com). Deploy, open a shell, move a database, read an activity log — from a terminal, in a checkout, without retyping anything the platform already knows.

One static binary, no runtime to install, and **no dependency beyond the Go standard library**. `go.mod` has no `require` block, and that is a rule rather than a coincidence: this binary holds a credential that reaches your production site, so its dependency tree should be auditable in an afternoon.

```console
$ vallic status
webshop → production
  running, tracking main
  release 412, a1b2c3d4

$ vallic deploy staging --wait
✓ Queued release 412 to acme-webshop-16-staging (deployment 77)
Waiting for deployment 77…
Fetching release 412
composer install --no-dev
drush updatedb -y
Switching current -> releases/412
✓ Deployed.

$ vallic ssh
vallic@acme-webshop-16-production:/var/www/html$ drush status

$ vallic db export > dump.sql
$ gunzip -c dump.sql.gz | vallic db import staging
$ vallic mount download ./files

$ vallic tunnel staging db
✓ db is on 127.0.0.1:3306. Ctrl-C closes it.
```

## Installing

```bash
curl -fsSL https://raw.githubusercontent.com/Vallic/vallic-cli/main/installer.sh | bash
```

Linux and macOS, amd64 and arm64. Needs `curl` and one of `sha256sum` or
`shasum`, and nothing else — an installer that pulls in a package manager first
is not a one-line installer. It lands in `/usr/local/bin` where that is
writable and `~/.local/bin` otherwise; `VALLIC_INSTALL_DIR` overrides both.

**It asks the control plane which version to install, not GitHub**, and that is
the point rather than an implementation detail. GitHub holds the bytes and the
control plane holds their SHA-256, so they are two systems an attacker would
both have to hold: a digest served by whoever served the binary can only say
the download arrived intact. Nothing is installed unless the checksum matches,
and the control plane also decides *which* version a customer should run, so a
tag that exists but was thought better of is not something this will hand you.
`VALLIC_API` points it at another control plane. Its own version is `--version`
away and `vallic self-update` reads the same endpoint.

Or with Go:

```bash
go install github.com/vallic/vallic-cli/cmd/vallic@latest
```

Or build from a checkout:

```bash
make build      # dist/vallic
make install    # into $GOBIN
make dist       # every platform, with dist/SHA256SUMS
```

### Staying current

```bash
vallic self-update         # the build the control plane publishes
vallic self-update --dev   # the build cut from this repository's dev branch
```

Two channels, the agent's: `stable` is what a laptop installs, `dev` is a build
from `dev` published as a GitHub pre-release on every push, versioned
`0.4.1-dev-202609241830` — the next patch, then the build stamp.

The control plane decides what each channel *is*, and which one it serves by
default (`$settings['vc_cli_channel']`, stable unless it says otherwise).
`--dev` asks for the other one for that command only: nothing is remembered, so
the next `self-update` goes back to the released build. There is deliberately
no `--stable`, because a console set to serve dev is a test console and
everything pointed at it is meant to be under test.

The bytes come from GitHub and the checksum from the control plane, and
`self-update` refuses anything that does not match the pin. A checksum served
by whoever served the binary can only say the download arrived intact.

## Signing in

```console
$ vallic login
Open https://console.vallic.com/device
and enter the code: FKDM-QRT-ZHV

Waiting for you to finish in the browser… (ctrl-c to stop)
✓ Signed in to https://console.vallic.com as alice@example.com
```

The browser does the authenticating, because that is where your session, your password manager and your second factor already are. The CLI never sees a password or a one-time code; it gets a short-lived token and refreshes it as it works.

**In CI**, where there is no browser and no human, use a personal access token:

```bash
export VALLIC_TOKEN=vcp_…     # minted in the console
vallic deploy staging --wait
```

The two are not interchangeable, and the difference is worth knowing:

| | `vallic login` | `VALLIC_TOKEN` |
|---|---|---|
| Lifetime | Minutes, refreshed automatically | Until revoked |
| Second factor | Yes, in the browser | No, and cannot be |
| Re-provable on demand | **Yes** | No |
| Scope | You, in every team you belong to | One team, everything you may do in it |

That third row is the one that bites, for exactly one action. Restoring a backup over a protected production environment requires the person asking to prove who they are *again* — the API answers `reauthentication_required`, and the CLI re-runs the browser flow and retries. A bearer token has nothing in it to challenge, so a pipeline cannot satisfy that check. It is not meant to: a gate whose purpose is that somebody is watching should not be passable by something that is not.

Deploying, rolling back and redeploying were gated the same way and are not any more. Each of them moves a pointer at code and is the thing you do *during* an incident, so a prompt arrived at the worst possible moment. A restore overwrites the database that is serving customers, which is the act the rule was always for.

## Not typing what the platform already knows

Inside a checkout, the project comes from the git remote and the environment from the current branch — both matched against what the API already returns. So `--project` and `--environment` are rarely needed:

```console
$ git switch develop
$ vallic status
webshop → staging
```

A branch that matches no environment is **refused, not defaulted to production**:

```console
$ git switch spike/caching
$ vallic deploy
vallic: cannot tell which environment you mean: no environment tracks the branch "spike/caching"
  try one of: production, staging
  or name it: --environment <name>
```

And nothing prompts when there is no terminal. In a pipeline every question that would have been asked is an error naming the flag that would have answered it, because a CLI that hangs on a prompt in CI hangs for twenty minutes and then fails on a timeout.

## Following a deploy

`--wait` prints the deploy log as it happens and exits on the outcome, which is
what makes it usable in CI:

```bash
vallic deploy staging --wait || exit $?    # 4 means it deployed and broke
```

The log goes to **stderr** during a deploy, because there it is progress — so
`--format json` stays parseable on stdout:

```console
$ vallic deploy staging --wait --format json 2>deploy.log | jq .task.state
"succeeded"
```

`vallic activity log <id>` prints the same bytes to **stdout**, because there
the log is what was asked for, and `--follow` keeps printing until the work
stops:

```bash
vallic activity log 77 --follow > deploy.log
```

Following backs off while nothing is happening and asks again immediately
while output is flowing — the control plane says which of those it is, so the
cadence is not this client guessing.

## Output

`--format table|json|yaml|csv`, defaulting to `table`. The format does not change because a pipe is attached — a command whose output shape depends on whether stdout is a terminal is a command that breaks in CI for a reason nobody can see in the diff. Scripts pass `--format json` and say what they meant.

`--format json` is a contract: the field names are the API's field names, unabbreviated, and nothing is dropped because it was empty. Progress, colour and anything conversational go to **stderr**, so `vallic db export > dump.sql` produces a dump rather than a dump with a spinner in it.

Exit codes, because `--wait` in CI depends on them:

| Code | Means |
|---|---|
| 0 | Did what was asked. For `--wait`, the work also succeeded |
| 1 | The platform refused: no such environment, no release ready, a quota, a permission |
| 2 | The command was wrong: unknown flag, missing argument, ambiguous target |
| 3 | Not signed in, or the credential has expired |
| 4 | `--wait` finished and the work **failed** |
| 5 | Could not reach the control plane |

4 is separate from 1 so a pipeline can tell "we would not deploy that" from "we deployed it and it broke" without parsing text. `vallic ssh -- <cmd>` passes the far side's own exit status through, because you are asking about that command and not about this one.

## Getting inside an environment

`ssh`, `db`, `sql` and `mount` all stand on a forced command the platform already installs behind your key. There is no new capability here: the CLI derives the connection — the host, the account, and the **port, which is chosen at random per project** and is not something a client could guess — and gets the quoting right.

Everything lands in the application container, never on the host. The code is mounted read-only; the writable path is what `vallic mount path` prints. `--dry-run` shows the command that would run:

```console
$ vallic ssh production --dry-run
ssh -p 2457 -t vc-acme@acme-webshop-16-production.vallic.cloud
```

If `ssh` refuses a key you have registered, it is almost always an agent offering every key it holds before the right one, six attempts being all sshd allows. Name the key:

```bash
vallic ssh -i ~/.ssh/vallic        # or: export VALLIC_SSH_KEY=~/.ssh/vallic
```

## The commands

`vallic <command> --help` has the detail, including why each one refuses what
it refuses. [USAGE.md](USAGE.md) has worked examples for all of it, grouped by
what you are trying to do. Everything takes `[<env>]`, which is worked out from the checkout
when it is left off.

| | |
|---|---|
| `login` · `logout` · `whoami` · `team` | Signing in, and which team a credential reaches |
| `project` · `env` · `server` · `status` | What exists |
| `url [--all] [--open]` | The browsable address, alone on stdout. `--all` lists every name the edge routes |
| `deploy` · `rollback` · `redeploy` · `release list` | Shipping |
| `activity list` · `activity get` · `activity log` | What the platform has been doing, and the log of one task |
| `logs` | Where the site's own request and error logs go — the project's destination and the machine's copy. Not a tail: Vallic Cloud does not keep them |
| `ssh` · `sql` · `db` · `mount` | Getting inside |
| `drush` | `drush` in the container, over the same forced command `ssh --` uses |
| `var list\|get\|set\|delete\|apply` | Variables, at either scope; `apply` restarts the site with them now |
| `domain list\|add\|verify\|delete` | Hostnames, and the DNS each one needs |
| `service list` | The stack, and the versions that go in `vallic.yaml` |
| `backup list\|create\|download\|restore` | Copies, and putting one back |
| `env create` · `env source` · `env delete` | Raising, repointing and removing an environment |
| `validate [--local]` | Checking `vallic.yaml` before you commit it |
| `init` | Writes a starting `vallic.yaml` from what the checkout looks like |
| `link` | Records which project this checkout is, when the git remote cannot say |

Four of these are worth knowing about before you need them.

**A secret is never readable, by anybody.** Not masked for a viewer and shown
to an owner: never returned at all. `var get` on one refuses rather than
printing an empty line, and `--format json` carries `"value": null` beside
`"secret": true` — which is how a script tells *withheld* from *set to the
empty string*. Setting a new value on an existing secret leaves it secret
unless you say otherwise, so rotating a password cannot quietly publish it.

**`domain verify` exits 0 when the answer is "not yet".** Nothing about the
request was wrong, and a retry loop that reported failure for a DNS record
that has not propagated is a retry loop that works correctly and cries wolf.
It refuses to re-check a hostname that is already verified, which sounds
unhelpful and is not: a re-check re-reads DNS, and a verified apex whose TXT
record has since been removed — which is allowed — would be marked failed and
stop being routed.

**`env create` does not build anything.** You get a draft with no machines,
and a deploy to it is refused until it is built. Building spends money, and
one command should not do that as a side effect of another.

**`init` and `validate --local` need no credential.** Both work on a checkout
for a site nobody has ordered yet: `init` writes a file from what it finds in
the directory and says what it guessed from, `validate --local` asks whether
that file parses. Neither touches an environment.

**`backup restore` will not infer its target.** Inheriting the environment
from the current branch is right for a deploy, where the worst case is a
release you can deploy again. Run unattended, a restore has to name what it
is overwriting.

## Configuration

| | |
|---|---|
| `~/.config/vallic/config.json` | Control plane, team, default format |
| `~/.config/vallic/credentials.json` | The credential, `0600`, refused if looser |
| `VALLIC_API` | The control plane to talk to |
| `VALLIC_TOKEN` | A personal access token; wins over what is on disk |
| `VALLIC_TEAM`, `VALLIC_PROJECT`, `VALLIC_ENVIRONMENT` | Targets, when a checkout cannot say |
| `VALLIC_SSH_KEY` | The private key to offer |
| `VALLIC_CONFIG_DIR` | Move both files elsewhere |
| `NO_COLOR` | Honoured, whatever its value |

Two files rather than one, deliberately: `config.json` is what people paste into an issue, and `credentials.json` is what must never appear there. A flag that takes a secret is never the documented path, because a flag puts it in `~/.bash_history` and in the output of `ps`.

## Development

```bash
make check     # gofmt, vet, test — what CI runs
make test
make smoke     # drives the built binary against a real control plane
```

`make smoke` is the one worth knowing about. Everything else here tests this
code against itself, and the three worst bugs this CLI has had could not have
been caught that way: each was a disagreement between what the client assumed
and what the platform actually sends, so a test written from the same
assumption agreed with it.

`deploy --release 7` sent the project's release *number* where the route loads
by entity *id*; `validate` could not decode its own reply, because PHP renders
an empty map as `[]` and that fails a Go map outright; `status` printed
`release 3339` for what the project calls release 7, which is a number
somebody would then type back into `--release`. gofmt, vet and the whole unit
suite were green through all three.

It needs a control plane and a token, and a DDEV site is a perfectly good one:

```bash
VALLIC_API=https://vallic.ddev.site VALLIC_TOKEN=vcp_... make smoke
```

Without both it skips every test and says so, so `make check` on a laptop and
CI on a fork stay green. It is read-only by construction — nothing in it
deploys, restores, backs up, claims a hostname or writes a variable, because a
smoke test that can change a customer's site is one nobody dares point at
anything worth testing against.

### Releasing

`main`, tagged `v0.4.0`, goes out through `.github/workflows/release.yml`:
the tag has to be reachable from `main`, the built binary has to report the
tag, and the checksums have to match what was built. Pushing to `dev` goes out
through `dev-release.yml` as a pre-release instead, which is the one place the
two disagree on purpose — a release is a deliberate act and a dev channel that
waits for one is just a slower stable.

Neither one serves anything. Pinning is `scripts/publish-cli.py` in the
`vallic` repository, and it reads the checksums out of the release's own
`SHA256SUMS` rather than from a local rebuild: a rebuild is not bit-identical,
and pinning one gives every client a checksum mismatch, which is what an attack
looks like.

New dependencies are not added. If something seems to need one, it is worth writing the twenty lines instead; see the package comments in `internal/output` and `internal/cli` for the two places that decision was already made.

## Layout

| Package | Holds |
|---|---|
| `cmd/vallic` | `main`, and nothing else |
| `internal/cli` | The command tree, dispatch, exit codes, the step-up retry |
| `internal/auth` | The credential store and the device authorization grant |
| `internal/api` | The user API client, its models and its error codes |
| `internal/resolve` | Which project and environment a command means |
| `internal/ssh` | Building the ssh and rsync commands |
| `internal/config` | What is remembered between runs |
| `internal/output` | table, json, yaml, csv |
| `internal/terminal` | Whether a file is a terminal, asked of the kernel rather than guessed |
| `internal/cli` (expiry.go) | When a credential runs out, and saying so before it does |
| `internal/smoke` | Build-tagged tests that drive the binary against a real control plane |

## Licence

[Apache-2.0](LICENSE). Permissive on purpose: this is a client for a public
API, and the platform is the product. A fork gains a way to talk to Vallic
Cloud, which is not something worth preventing.

Apache rather than MIT for the express patent grant, which matters more for a
commercial platform than the extra paragraph costs.

The licence covers the code and not the name. "Vallic" is a trademark, so this
may be forked and redistributed freely and a fork may not be called `vallic` —
the same pairing that lets every package manager ship `gh` while nobody else
may publish a fork under that name.
