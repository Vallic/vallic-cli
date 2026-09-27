// Command vallic works on Vallic Cloud sites from a terminal.
//
// One static binary, no runtime to install, and no dependency beyond the Go
// standard library — the same rule the agent follows, and here it is also
// what makes the binary auditable by the people whose production sites it
// reaches.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/vallic/vallic-cli/internal/cli"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	env := cli.NewEnv(version)
	root := cli.Root()

	args := os.Args[1:]

	// Before the tree, because both are questions about the binary rather
	// than commands it runs, and neither should need a configuration file to
	// answer. `vallic` with nothing at all is somebody asking what it does.
	if len(args) == 0 {
		cli.PrintHelp(env.Err, root, []string{root.Name})
		os.Exit(cli.ExitOK)
	}

	if args[0] == "--version" || args[0] == "-version" || args[0] == "version" {
		fmt.Println(version)
		os.Exit(cli.ExitOK)
	}

	// Cancelled on the first signal. Unlike the agent, which drains because
	// killing it mid-deploy leaves a machine between two releases, nothing
	// here holds state worth protecting: a cancelled poll leaves the work
	// running on the platform, and that is the correct outcome.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := cli.Execute(ctx, root, env, args)

	os.Exit(cli.Report(env, root, err))
}
