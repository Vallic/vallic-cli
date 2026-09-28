package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/vallic/vallic-cli/internal/api"
	"github.com/vallic/vallic-cli/internal/output"
)

// The two scopes a variable can be defined at.
//
// The control plane's own words (VariableScope), which are also what the
// `scope` column prints — so the flag somebody types, the answer they read
// and the API's documentation all say the same thing.
const (
	scopeEnvironment = "environment"
	scopeProject     = "project"
)

// varCommand reads and writes the variables a site runs with.
func varCommand() *Command {
	return &Command{
		Name:    "var",
		Aliases: []string{"variable"},
		Summary: "read and write environment variables",
		Long: `A variable reaches a container through the rendered .env, which is
written on a deploy or by ` + "`var apply`" + ` — so setting one does not change a running
site until one of those. Set several, then apply once: every apply restarts
the site.

A secret is never readable, by anybody, once written. That is the point of
marking one: the platform stores it and does not hand it back. ` + "`var list`" + `
shows the name and no value, and ` + "`--format json`" + ` carries
` + "`\"value\": null`" + ` beside ` + "`\"secret\": true`" + ` — which is how a script tells
"withheld" from "set to the empty string".

Every subcommand takes ` + "`--scope project`" + ` to act on the project's own
definitions instead of one environment's. A project variable reaches every
environment that does not override it.`,
		Children: []*Command{
			varListCommand(),
			varGetCommand(),
			varSetCommand(),
			varDeleteCommand(),
			varApplyCommand(),
		},
	}
}

func varListCommand() *Command {
	var scope string

	return &Command{
		Name:    "list",
		Summary: "list the variables in effect",
		Usage:   "var list [<env>] [--scope project]",
		Long: `Everything in effect for the environment, including what it
inherits from the project.

The SCOPE column says which definition won. A variable shown as ` + "`project`" + `
is inherited — editing it changes every environment that does not override
it, which is usually what somebody wants and occasionally very much not. One
shown as ` + "`environment (overrides project)`" + ` is hiding a project value.

` + "`--scope project`" + ` lists what the project itself defines, before any
environment overrides it.`,
		Flags: scopeFlag(&scope),
		Run: func(ctx context.Context, env *Env, args []string) error {
			chosen, err := variableScope(scope)
			if err != nil {
				return err
			}

			client, err := env.Client()
			if err != nil {
				return err
			}

			var (
				variables []api.Variable
				where     string
				pending   *bool
			)

			if chosen == scopeProject {
				if err := ensureNoExtra(nil, args, 0); err != nil {
					return err
				}

				project, resolveErr := env.ResolveProject(ctx)
				if resolveErr != nil {
					return resolveErr
				}

				where = project.MachineName
				variables, err = client.ProjectVariables(ctx, project.ID)
			} else {
				target, resolveErr := env.ResolveEnvironment(ctx, first(args))
				if resolveErr != nil {
					return resolveErr
				}

				where = target.Environment.Name

				var list *api.VariableList
				list, err = client.EnvironmentVariables(ctx, target.Environment.ID)
				if list != nil {
					variables, pending = list.Variables, list.Pending
				}
			}

			if err != nil {
				return err
			}

			table := output.Table{
				Columns: []string{"name", "value", "scope"},
				Empty:   "No variables.",
			}

			for i := range variables {
				table.Rows = append(table.Rows, []string{
					variables[i].Name,
					variables[i].Display(),
					variables[i].ScopeLabel(),
				})
			}

			data := map[string]any{
				chosen:      where,
				"variables": variables,
			}

			// Only where the control plane said. Absent rather than false
			// when it did not, so a script can tell the two apart.
			if pending != nil {
				data["pending"] = *pending
			}

			if err := env.Printer.Print(table, data); err != nil {
				return err
			}

			// After the table, where it is read: the values listed are the
			// saved ones, and this is the one case where the site disagrees.
			if pending != nil && *pending && !env.Printer.Structured() {
				env.Printer.Warn("not applied yet: %s is still running with the previous values", where)
				env.Printer.Say("  `vallic var apply %s` restarts it with these, or they go out with the next deploy", where)
			}

			return nil
		},
	}
}

func varGetCommand() *Command {
	var scope string

	return &Command{
		Name:    "get",
		Summary: "print one variable's value",
		Usage:   "var get <name> [<env>]",
		Long: `Prints the value alone, so it can be captured:

    DB=$(vallic var get DATABASE_URL)

The value in effect on the environment, which may be one the project defines.
` + "`--scope project`" + ` reads the project's own definition instead.

A secret has no value to print. The command refuses rather than printing an
empty line, because an empty line captured into a variable is a bug somebody
finds much later.`,
		Flags: scopeFlag(&scope),
		Run: func(ctx context.Context, env *Env, args []string) error {
			if len(args) == 0 {
				return Usagef(nil, "name a variable")
			}

			chosen, err := variableScope(scope)
			if err != nil {
				return err
			}

			name := args[0]

			client, err := env.Client()
			if err != nil {
				return err
			}

			var variable *api.Variable

			if chosen == scopeProject {
				if err := ensureNoExtra(nil, args, 1); err != nil {
					return err
				}

				variable, err = projectVariable(ctx, env, client, name)
			} else {
				target, resolveErr := env.ResolveEnvironment(ctx, firstOf(args[1:]))
				if resolveErr != nil {
					return resolveErr
				}

				variable, err = client.EnvironmentVariable(ctx, target.Environment.ID, name)
			}

			if err != nil {
				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(map[string]any{"variable": variable})
			}

			if variable.Secret || variable.Value == nil {
				return fmt.Errorf(
					"%s is a secret, so its value cannot be read back — that is what marking it secret means\n"+
						"  set a new one with: vallic var set %s <value> --secret",
					name, name,
				)
			}

			// The value alone on stdout, with nothing around it, because this
			// is a command whose output somebody captures.
			env.Printer.Line("%s", *variable.Value)

			return nil
		},
	}
}

func varSetCommand() *Command {
	var (
		secret      bool
		scope       string
		description string
		fromStdin   bool
	)

	return &Command{
		Name:    "set",
		Summary: "set a variable",
		Usage:   "var set <name> <value> [<env>] [--secret]",
		Long: `Creates it, or updates it if it is already there — so it is safe to
run twice.

--secret hides the value: it is stored and never handed back, to anybody. An
existing secret stays secret when you set a new value without saying, so
updating a password cannot silently turn it into a readable variable.

--stdin reads the value from standard input instead of the command line,
which keeps it out of shell history and out of ` + "`ps`" + `:

    printf '%s' "$TOKEN" | vallic var set API_TOKEN --stdin --secret`,
		Flags: func(fs *flag.FlagSet) {
			scopeFlag(&scope)(fs)
			fs.BoolVar(&secret, "secret", false, "store it hidden, and never hand it back")
			fs.BoolVar(&fromStdin, "stdin", false, "read the value from standard input")
			fs.StringVar(&description, "description", "", "what it is for")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			if len(args) == 0 {
				return Usagef(nil, "name a variable")
			}

			chosen, err := variableScope(scope)
			if err != nil {
				return err
			}

			name := args[0]
			rest := args[1:]

			var value string

			if fromStdin {
				// Not trimmed. A value somebody piped in is the value they
				// meant, trailing newline included or not — and a token with
				// a stripped character is a token that fails opaquely later.
				value, err = readValue(env)
				if err != nil {
					return err
				}
			} else {
				if len(rest) == 0 {
					return Usagef(nil, "give a value, or pass --stdin to read one")
				}

				value = rest[0]
				rest = rest[1:]
			}

			client, err := env.Client()
			if err != nil {
				return err
			}

			write := api.VariableWrite{
				Name:        name,
				Value:       value,
				Description: description,
			}

			// Only sent when asked for. Omitting it is what lets an existing
			// secret stay secret; sending false would reveal one.
			if secret {
				write.Secret = &secret
			}

			var (
				result *api.VariableWritten
				where  string
			)

			if chosen == scopeProject {
				if err := ensureNoExtra(nil, rest, 0); err != nil {
					return err
				}

				project, resolveErr := env.ResolveProject(ctx)
				if resolveErr != nil {
					return resolveErr
				}

				where = project.MachineName
				result, err = client.SetProjectVariable(ctx, project.ID, write)
			} else {
				target, resolveErr := env.ResolveEnvironment(ctx, firstOf(rest))
				if resolveErr != nil {
					return resolveErr
				}

				where = target.Environment.Name
				result, err = client.SetEnvironmentVariable(ctx, target.Environment.ID, write)
			}

			if err != nil {
				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(result)
			}

			env.Printer.Good("%s set on %s", result.Variable.Name, where)

			// Said every time rather than once in the help, because the
			// alternative is somebody reloading the site and concluding the
			// platform ignored them. In the control plane's own words, so it
			// stays true if the platform's answer ever changes.
			env.Printer.Say("  it reaches the site on the %s", takesEffect(result.TakesEffect))
			env.Printer.Say("  or now, with `vallic var apply%s`", applyHint(chosen))

			if chosen == scopeProject {
				env.Printer.Say("  every environment that does not override it will see it")
			}

			return nil
		},
	}
}

func varDeleteCommand() *Command {
	var scope string

	return &Command{
		Name:    "delete",
		Aliases: []string{"unset"},
		Summary: "remove a variable",
		Usage:   "var delete <name> [<env>]",
		Long: `Removes the definition at that scope only.

A variable an environment inherits from its project is not defined on the
environment, so removing it there is refused rather than silently deleting the
project's copy — which would change every other environment too. Remove that
one with ` + "`--scope project`" + `, deliberately.`,
		Flags: scopeFlag(&scope),
		Run: func(ctx context.Context, env *Env, args []string) error {
			if len(args) == 0 {
				return Usagef(nil, "name a variable")
			}

			chosen, err := variableScope(scope)
			if err != nil {
				return err
			}

			name := args[0]

			client, err := env.Client()
			if err != nil {
				return err
			}

			var (
				removed *api.VariableRemoved
				where   string
			)

			if chosen == scopeProject {
				if err := ensureNoExtra(nil, args, 1); err != nil {
					return err
				}

				project, resolveErr := env.ResolveProject(ctx)
				if resolveErr != nil {
					return resolveErr
				}

				where = project.MachineName
				removed, err = client.DeleteProjectVariable(ctx, project.ID, name)
			} else {
				target, resolveErr := env.ResolveEnvironment(ctx, firstOf(args[1:]))
				if resolveErr != nil {
					return resolveErr
				}

				where = target.Environment.Name
				removed, err = client.DeleteEnvironmentVariable(ctx, target.Environment.ID, name)
			}

			if err != nil {
				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(removed)
			}

			env.Printer.Good("%s removed from %s", name, where)
			env.Printer.Say("  it leaves the site on the %s", takesEffect(removed.TakesEffect))
			env.Printer.Say("  or now, with `vallic var apply%s`", applyHint(chosen))

			return nil
		},
	}
}

func varApplyCommand() *Command {
	var scope string

	return &Command{
		Name:    "apply",
		Summary: "restart the site with its variables now",
		Usage:   "var apply [<env>] [--scope project]",
		Long: `Setting a variable does not change a running site: its containers keep
the values they were started with until the next deploy. apply sends them now
instead. The site and its workers restart with the saved values, which takes a
few seconds; the database and other services keep running.

Set several, then apply once: every apply is a restart.

` + "`--scope project`" + ` applies to every environment still running on values the
project has since changed, and names each. Where none is behind, it says so
and succeeds, so a script can run it after every change.

It exits non-zero when an environment could not be reached just now — a
machine being rebuilt, say. Its variables then go out with its next deploy,
or run it again in a minute.`,
		Flags: scopeFlag(&scope),
		Run: func(ctx context.Context, env *Env, args []string) error {
			chosen, err := variableScope(scope)
			if err != nil {
				return err
			}

			client, err := env.Client()
			if err != nil {
				return err
			}

			var result *api.VariablesApplied

			if chosen == scopeProject {
				if err := ensureNoExtra(nil, args, 0); err != nil {
					return err
				}

				project, resolveErr := env.ResolveProject(ctx)
				if resolveErr != nil {
					return resolveErr
				}

				result, err = client.ApplyProjectVariables(ctx, project.ID)
			} else {
				if err := ensureNoExtra(nil, args, 1); err != nil {
					return err
				}

				target, resolveErr := env.ResolveEnvironment(ctx, first(args))
				if resolveErr != nil {
					return resolveErr
				}

				result, err = client.ApplyEnvironmentVariables(ctx, target.Environment.ID)
			}

			if err != nil {
				return err
			}

			if env.Printer.Structured() {
				if err := env.Printer.Value(result); err != nil {
					return err
				}
			} else {
				reportApplied(env, result)
			}

			if len(result.Unreachable) > 0 {
				return fmt.Errorf("%d environment(s) could not be reached; their variables go out with the next deploy", len(result.Unreachable))
			}

			return nil
		},
	}
}

// reportApplied says what `var apply` did, one line per environment.
func reportApplied(env *Env, result *api.VariablesApplied) {
	if len(result.Applied) == 0 && len(result.Unreachable) == 0 {
		env.Printer.Say("Every environment already has these values. Nothing to apply.")

		return
	}

	for _, slug := range result.Applied {
		env.Printer.Good("%s is restarting with its variables", slug)
	}

	for _, slug := range result.Unreachable {
		env.Printer.Warn("%s could not be reached just now", slug)
	}
}

// applyHint is the flag `var apply` needs to act where a write just did.
func applyHint(scope string) string {
	if scope == scopeProject {
		return " --scope project"
	}

	return ""
}

// scopeFlag registers --scope on a command.
//
// Not --project, which is already global and names *which* project. Two
// flags a letter apart meaning "which project" and "the project rather than
// the environment" would be a coin toss every time somebody typed one.
func scopeFlag(into *string) func(fs *flag.FlagSet) {
	return func(fs *flag.FlagSet) {
		fs.StringVar(into, "scope", "", "environment (default) or project")
	}
}

// variableScope settles which scope was asked for.
func variableScope(value string) (string, error) {
	switch value {
	case "", scopeEnvironment:
		return scopeEnvironment, nil
	case scopeProject:
		return scopeProject, nil
	default:
		return "", Usagef(nil, "--scope takes %s or %s, not %q", scopeEnvironment, scopeProject, value)
	}
}

// projectVariable reads one of a project's own definitions.
//
// By listing and filtering, because the control plane has no route for one
// project variable — there is nothing to resolve at that scope, so the list
// is the whole answer. Worth the round trip to keep --scope meaning the same
// thing on all four subcommands.
func projectVariable(ctx context.Context, env *Env, client *api.Client, name string) (*api.Variable, error) {
	project, err := env.ResolveProject(ctx)
	if err != nil {
		return nil, err
	}

	variables, err := client.ProjectVariables(ctx, project.ID)
	if err != nil {
		return nil, err
	}

	for i := range variables {
		if variables[i].Name == name {
			return &variables[i], nil
		}
	}

	return nil, fmt.Errorf("%s is not defined on %s", name, project.MachineName)
}

// readValue reads a variable's value from standard input.
func readValue(env *Env) (string, error) {
	if env.Interactive() {
		// stdin is the terminal, so --stdin would sit waiting for somebody
		// to type a value with no prompt to say so. Better to refuse than to
		// look like a hang.
		return "", Usagef(nil, "--stdin has nothing to read: pipe a value in, as `printf '%%s' \"$TOKEN\" | vallic var set NAME --stdin`")
	}

	raw, err := io.ReadAll(io.LimitReader(env.In, maxVariable+1))
	if err != nil {
		return "", fmt.Errorf("cannot read standard input: %w", err)
	}

	if len(raw) > maxVariable {
		// Refused rather than truncated. A key or a certificate cut short is
		// a value that looks set and fails at runtime, which is very much
		// worse than being told now.
		return "", fmt.Errorf("that is more than %dKB, which is more than one variable holds", maxVariable/1024)
	}

	return string(raw), nil
}

// maxVariable is the most --stdin will read.
//
// Generous enough for a private key or a certificate chain, which are the
// large things people legitimately put in a variable, and small enough that
// a misdirected pipe fails rather than filling memory.
const maxVariable = 256 << 10

// takesEffect renders the control plane's own words, with a fallback.
//
// Empty only where a control plane did not say, and "next deploy" is the
// right thing to assume there — it is what every version so far does.
func takesEffect(answer string) string {
	if answer == "" {
		return "next deploy"
	}

	return answer
}
