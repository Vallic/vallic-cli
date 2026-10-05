# Using vallic

Worked examples, grouped by what you are trying to do. Every command has
`--help` with the detail; the [README](README.md) has what the CLI is and how
it finds your project.

Inside a checkout of your project the CLI works out the project from the git
remote and the environment from the branch, so most of these need neither.
Elsewhere, name the environment and add `--project <machine-name>`.

## Getting set up

```bash
# Install, and sign in through the browser
curl -fsSL https://raw.githubusercontent.com/Vallic/vallic-cli/main/installer.sh | bash
vallic login

# On a server or in CI, where there is no browser: a token from your account
vallic login --token

# Who the stored credential belongs to, and which teams it reaches
vallic whoami
vallic team

# Record which project this directory is, outside a clone of its repository
vallic link acme-site

# What this checkout points at: project, environment, branch, last deploy
vallic status

# Stay current, and tab completion for your shell
vallic self-update
vallic completion bash > ~/.local/share/bash-completion/completions/vallic
```

## What exists

```bash
vallic project list
vallic env list
vallic env info staging

# The machines your credential can see, and the stack an environment runs
vallic server list
vallic service list production

# The address, alone on stdout so it can be captured, or opened
vallic url staging
vallic url staging --all
vallic url staging --open
```

## vallic.yaml

```bash
# A starting file from what the checkout looks like
vallic init --type drupal

# Check it before committing: on its own, or against what an environment runs
vallic validate --local
vallic validate --against production
```

## Environments

```bash
# A development environment from a branch, deployed on every push
vallic env create feature/checkout --type development --auto-deploy

# Point staging at another branch, or stop deploying it on push
vallic env source staging --branch release/2.0
vallic env source staging --auto-deploy false

# Remove an environment and its machines
vallic env delete feature-checkout
```

## Building and deploying

```bash
# Deploy what staging's branch points at, and wait for the result
vallic deploy staging --wait

# Build and deploy in one go, or build only
vallic build staging --deploy
vallic build staging

# The last five builds of production, and deploy an earlier one
vallic release list production --limit 5
vallic deploy production --release 41 --wait

# Deploy what is live again, or put the previous release back
vallic redeploy production --wait
vallic rollback production --wait

# Skip the deploy steps for one deploy
vallic deploy staging --skip-steps
```

## What the platform has been doing

```bash
# Recent deploys and builds, one task, and its output as it runs
vallic activity list production --type deploy,build --limit 20
vallic activity get 1234
vallic activity log 1234 --follow

# Where the site's logs go
vallic logs production
```

## A shell and commands

```bash
# A shell in the site's container
vallic ssh staging

# One command, then back
vallic ssh staging -- ls -la /mnt/files/public
vallic ssh production -- php artisan about

# drush, in a Drupal project
vallic drush staging -- cr
vallic drush production -- updb -y
vallic drush status

# The ssh command it would run, without running it
vallic ssh production --dry-run
```

### Several machines, and workers

An environment on more than one machine is reached through its front, which
carries the connection on to the machine you name. Without `--machine` the
login goes where the site runs.

```bash
# The machines, by the name --machine takes
vallic env info production

# A shell on the second web server, or on the worker machine
vallic ssh production --machine web-2
vallic ssh production --machine worker

# A worker's container rather than the site's
vallic ssh production --container queue-1
vallic ssh production --container queue-1 -- php artisan queue:failed
```

If ssh refuses a key you have registered, name it: an agent offers every key it
holds, and sshd allows six attempts.

```bash
vallic ssh production -i ~/.ssh/vallic      # or: export VALLIC_SSH_KEY=~/.ssh/vallic
```

## Files

```bash
# Public files up, and back down
vallic mount upload staging ./files
vallic mount download production ./files-from-production

# Private files, and a mount vallic.yaml declares
vallic mount upload staging ./private --area private
vallic mount upload staging ./exports --area mounts/exports
vallic mount download production ./exports --area mounts/exports

# Every area an environment has, and where one is on the far side
vallic mount list staging
vallic mount path staging --area private

# Make staging's public files match a local directory, removing the rest
vallic mount upload staging ./files --delete
```

## Databases

```bash
# A copy of production's database, then onto staging
vallic db export production > production.sql
vallic db import staging < production.sql

# Compressed on the way out
vallic db export production | gzip > production.sql.gz
gunzip -c production.sql.gz | vallic db import staging

# An interactive prompt
vallic sql staging

# A local port onto the database, for a desktop client, and the credentials
vallic tunnel staging db --port 3307
vallic ssh staging -- printenv DB_NAME DB_USER DB_PASSWORD
```

## Backups

```bash
# What production has, and take one now
vallic backup list production --kind database
vallic backup create production --kind database
vallic backup create production --kind files

# A copy of one to keep
vallic backup download 42 production --wait

# Put the newest database backup back over staging
vallic backup restore latest staging --kind database
```

A backup is restored over the environment it was taken from and nowhere else,
and a restore always names that environment. On a protected environment an
owner confirms it in a browser.

## Variables

```bash
# What is in effect for staging, including the project's
vallic var list staging
vallic var list --scope project

# Set a secret, read a value, remove one
vallic var set PAYMENT_KEY sk_live_example production --secret
vallic var get API_URL staging
vallic var delete OLD_FLAG staging

# Restart the site so changed variables take effect now
vallic var apply production
```

A secret is never returned, to anybody: `var get` on one refuses.

## Domains

```bash
# Claim a hostname, check the DNS records it printed, and list them
vallic domain add shop.example.com production
vallic domain verify shop.example.com production
vallic domain list production

# Give one up
vallic domain delete old.example.com production
```

`domain verify` exits 0 when the answer is "not yet", so a retry loop does not
fail on a record that has not propagated.

## In scripts and CI

```bash
# JSON instead of tables, for jq
vallic env list --format json | jq -r '.[].slug'
vallic var get API_URL staging --format json

# Deploy from CI with a token, and fail the job if the deploy fails
VALLIC_TOKEN="$TOKEN" vallic login --token
vallic deploy production --wait

# Check vallic.yaml in a pre-commit hook, with no credential
vallic validate --local || exit 1

# The address of the environment a pipeline just deployed
url=$(vallic url staging)
```
