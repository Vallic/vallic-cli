package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
)

// tunnelPorts is the local port a tunnel listens on by default: each service's
// usual one, so a desktop client's defaults work unchanged. The agent decides
// which port it reaches on the far side; these are only this end's.
var tunnelPorts = map[string]int{
	"db":          3306,
	"mariadb":     3306,
	"mysql":       3306,
	"postgres":    5432,
	"redis":       6379,
	"valkey":      6379,
	"memcached":   11211,
	"solr":        8983,
	"meilisearch": 7700,
	"rabbitmq":    5672,
	"vinyl":       6081,
	"varnish":     6081,
}

// tunnelCommand opens a local port onto one of an environment's services.
func tunnelCommand() *Command {
	var identity string
	var dryRun bool
	var port int

	return &Command{
		Name:    "tunnel",
		Summary: "open a local port onto an environment's database, cache or search",
		Usage:   "tunnel [<env>] <service> [--port N]",
		Long: `A local port that reaches one of the environment's own services, for a
desktop client: a database tool, a Redis or Solr browser, curl against
Varnish.

Not SSH port forwarding, which stays off on every managed machine. Each
connection to the local port runs one SSH session, and the machine relays
that session to the service you named — of this environment, on its own
port, and nothing else. Services: db (whichever database the stack runs),
mariadb, mysql, postgres, redis, valkey, memcached, solr, meilisearch,
rabbitmq, varnish.

Credentials are the environment's own. For the database:
  vallic ssh <env> -- printenv DB_NAME DB_USER DB_PASSWORD

Ctrl-C closes the port.`,
		Flags: func(fs *flag.FlagSet) {
			identityFlag(fs, &identity)
			fs.IntVar(&port, "port", 0, "the local port to listen on (default: the service's usual one)")
			fs.BoolVar(&dryRun, "dry-run", false, "print the ssh command each connection runs instead of listening")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			var positional, service string

			switch len(args) {
			case 1:
				service = args[0]
			case 2:
				positional, service = args[0], args[1]
			default:
				return errors.New("name a service: vallic tunnel [<env>] <service>")
			}

			if service == "varnish" {
				// What a person calls it; the stack calls it vinyl.
				service = "vinyl"
			}

			if _, known := tunnelPorts[service]; !known {
				return fmt.Errorf("%q is not a service a tunnel can reach; try db, redis, solr or varnish", service)
			}

			if port == 0 {
				port = tunnelPorts[service]
			}

			target, err := env.sshTarget(ctx, positional, identity)
			if err != nil {
				return err
			}

			// The forced command's verb with the service as its argument:
			// the machine decides the container and the port from it.
			verb := "vallic-tunnel " + service

			if dryRun {
				return runOrPrint(env, target.Verb(verb, false), true)
			}

			listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
			if err != nil {
				return fmt.Errorf("port %d is taken here; choose another with --port: %w", port, err)
			}

			env.Printer.Good("%s is on 127.0.0.1:%d. Ctrl-C closes it.", service, port)

			// Closed when Ctrl-C cancels the context, which ends Accept.
			go func() {
				<-ctx.Done()
				_ = listener.Close()
			}()

			var open sync.WaitGroup
			defer open.Wait()

			for {
				conn, err := listener.Accept()
				if err != nil {
					if ctx.Err() != nil {
						return nil
					}

					return err
				}

				open.Add(1)

				go func() {
					defer open.Done()
					defer conn.Close()

					// One session per connection, carrying its bytes over
					// stdin and stdout; a stream, so no terminal.
					cmd := target.Verb(verb, false)
					cmd.Stdin = conn
					cmd.Stdout = conn
					cmd.Stderr = os.Stderr

					if err := cmd.Run(); err != nil && ctx.Err() == nil {
						env.Printer.Warn("a connection to %s ended: %v", service, err)
					}
				}()
			}
		},
	}
}
