package cli

import (
	"context"
	"flag"
	"fmt"
	"os/exec"
	"runtime"

	"github.com/vallic/vallic-cli/internal/api"
	"github.com/vallic/vallic-cli/internal/output"
)

// urlCommand prints where an environment can be reached.
func urlCommand() *Command {
	var (
		all  bool
		open bool
	)

	return &Command{
		Name:    "url",
		Summary: "print an environment's browsable address",
		Usage:   "url [<env>] [--all] [--open]",
		Long: `The canonical address, alone on stdout, so it can be captured:

    curl -sI "$(vallic url staging)"

--all lists every name the site answers on instead. That list is the one the
edge proxy routes by, so a custom domain missing from it is a domain that does
not answer yet, whatever is in DNS — ` + "`vallic domain list`" + ` says why.

The scheme comes from the control plane rather than being assumed here: which
of several TLS modes terminates the connection is not something a client can
work out, and every one of them terminates TLS.`,
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&all, "all", false, "every hostname the site answers on")
			fs.BoolVar(&open, "open", false, "open it in a browser")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			if all && open {
				return Usagef(nil, "--all lists names and --open takes one, so they cannot go together")
			}

			target, err := env.ResolveEnvironment(ctx, first(args))
			if err != nil {
				return err
			}

			detail, err := env.Detail(ctx, target)
			if err != nil {
				return err
			}

			if detail.Hostnames == nil {
				// An older control plane. Said plainly rather than guessed at
				// from the slug: the platform hostname is built from config
				// this binary does not have, and a wrong URL printed
				// confidently is worse than none.
				return fmt.Errorf(
					"this control plane does not report an environment's hostnames yet\n"+
						"  the console's routing page for %s has them",
					detail.Name,
				)
			}

			if all {
				table := output.Table{
					Columns: []string{"hostname", "role"},
					Empty:   "This environment answers on nothing, which should not be possible.",
				}

				for _, hostname := range detail.Hostnames.Routed {
					table.Rows = append(table.Rows, []string{hostname, hostnameRole(detail.Hostnames, hostname)})
				}

				return env.Printer.Print(table, map[string]any{
					"environment": detail.Name,
					"hostnames":   detail.Hostnames,
				})
			}

			if env.Printer.Structured() {
				return env.Printer.Value(map[string]any{
					"environment": detail.Name,
					"hostnames":   detail.Hostnames,
				})
			}

			if open {
				return openInBrowser(env, detail.Hostnames.URL)
			}

			// Alone on stdout, with nothing around it. This is a command
			// whose output somebody captures.
			env.Printer.Line("%s", detail.Hostnames.URL)

			return nil
		},
	}
}

// hostnameRole says what a routed hostname is for.
func hostnameRole(hostnames *api.Hostnames, hostname string) string {
	switch {
	case hostnames.Primary != nil && hostname == *hostnames.Primary:
		return "canonical"
	case hostname == hostnames.Platform:
		// Worth naming rather than leaving blank: it is the one that cannot
		// be removed, and somebody deciding what to point DNS at should see
		// which name that is.
		return "platform"
	default:
		return "custom"
	}
}

// openInBrowser hands a URL to whatever opens one.
//
// Best effort, and it says so when it fails: a command that silently did
// nothing would be indistinguishable from one whose browser took a while.
func openInBrowser(env *Env, url string) error {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}

	if err := cmd.Start(); err != nil {
		// The URL is printed anyway. Somebody on a machine with no browser
		// still wants the address, and this is the only place it appears.
		env.Printer.Line("%s", url)

		return fmt.Errorf("cannot open a browser here: %w", err)
	}

	env.Printer.Say("Opening %s", url)

	// Not waited on. A browser holds its process open for as long as the
	// window lives, and `vallic url --open` that does not return is a
	// terminal somebody has to kill.
	go func() { _ = cmd.Wait() }()

	return nil
}
