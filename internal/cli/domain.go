package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/vallic/vallic-cli/internal/api"
	"github.com/vallic/vallic-cli/internal/output"
)

// domainCommand manages the hostnames an environment answers on.
func domainCommand() *Command {
	return &Command{
		Name:    "domain",
		Summary: "manage the hostnames an environment answers on",
		Long: `A hostname is claimed here and proved in DNS. Until it is proved
the platform will not route it, because routing an unproved name would let
anybody point a hostname at us and have us serve somebody else's site under it.

So the sequence is: ` + "`domain add`" + ` prints the records to publish,
` + "`domain verify`" + ` checks them, and ` + "`vallic url --all`" + ` shows what is
actually being routed.`,
		Children: []*Command{
			domainListCommand(),
			domainAddCommand(),
			domainVerifyCommand(),
			domainDeleteCommand(),
		},
	}
}

func domainListCommand() *Command {
	return &Command{
		Name:    "list",
		Summary: "list the hostnames claimed for an environment",
		Usage:   "domain list [<env>]",
		Long: `Unverified hostnames included, which is the point: this is where to
find out why a name does not work yet.

STATE is three answers, not two. ` + "`unverified`" + ` means nothing has checked
yet, ` + "`failed`" + ` means something checked and the record was wrong, and those
call for different things to do next. What the last check found is printed
under the table, in the platform's own words, so which way a record was wrong
does not cost another check. ` + "`vallic domain verify <host>`" + ` prints the
records the platform is looking for when it does not find them.`,
		Run: func(ctx context.Context, env *Env, args []string) error {
			target, err := env.ResolveEnvironment(ctx, first(args))
			if err != nil {
				return err
			}

			client, err := env.Client()
			if err != nil {
				return err
			}

			list, err := client.Domains(ctx, target.Environment.ID)
			if err != nil {
				return err
			}

			table := output.Table{
				Columns: []string{"hostname", "kind", "state", "canonical", "tls"},
				Empty:   "No hostnames, which should not be possible.",
			}

			for i := range list.Domains {
				domain := &list.Domains[i]

				table.Rows = append(table.Rows, []string{
					domain.Hostname,
					domain.Kind,
					domainState(domain),
					yesNo(domain.IsPrimary),
					domain.TLSMode,
				})
			}

			if env.Printer.Structured() {
				return env.Printer.Value(list)
			}

			if err := env.Printer.Print(table, nil); err != nil {
				return err
			}

			printCheckReasons(env, list.Domains)

			if !list.AllowsCustomDomains {
				// Said before somebody tries and is refused. This environment
				// is a kind that cannot hold a custom hostname at all.
				env.Printer.Say("This environment does not take custom hostnames.")

				return nil
			}

			// The target, every time. It is the one fact somebody needs
			// before they can add anything, and it is not derivable from the
			// slug: a CDN in front of the environment answers on its own name.
			env.Printer.Say("Point a new hostname at: %s", list.Target)

			return nil
		},
	}
}

func domainAddCommand() *Command {
	return &Command{
		Name:    "add",
		Summary: "claim a hostname for an environment",
		Usage:   "domain add <hostname> [<env>]",
		Long: `Claims it and prints the DNS records to publish. Nothing is routed
until those records are in place and ` + "`domain verify`" + ` has seen them.

Only the hostname is sent. Whether a CDN fronts it, which TLS mode applies,
whether it is an apex and what its verification token is are all worked out
from the hostname and the environment, and the control plane refuses a request
that tries to set them rather than ignoring it — a script that set
` + "`is_primary`" + ` and was silently ignored would believe it had promoted a
hostname.

On a protected environment this needs an owner. Adding a hostname changes what
the environment answers to, so it counts as reshaping it rather than operating
it.`,
		Run: func(ctx context.Context, env *Env, args []string) error {
			if len(args) == 0 {
				return Usagef(nil, "name a hostname")
			}

			hostname := args[0]

			target, err := env.ResolveEnvironment(ctx, firstOf(args[1:]))
			if err != nil {
				return err
			}

			client, err := env.Client()
			if err != nil {
				return err
			}

			domain, err := client.AddDomain(ctx, target.Environment.ID, hostname)
			if err != nil {
				if api.Code(err) == "hostname_claimed" {
					// The control plane deliberately does not say where it is
					// claimed, because that would leak another tenant's
					// estate. Worth explaining rather than leaving the
					// refusal looking evasive.
					return fmt.Errorf("%w\n  a hostname can only be verified on one environment at a time", err)
				}

				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(map[string]any{"domain": domain})
			}

			env.Printer.Good("%s claimed on %s", domain.Hostname, target.Environment.Name)

			printInstructions(env, domain)

			return nil
		},
	}
}

func domainVerifyCommand() *Command {
	return &Command{
		Name:    "verify",
		Summary: "check a hostname's DNS records",
		Usage:   "domain verify <hostname> [<env>]",
		Long: `Looks the records up now and says what it found. It is one DNS
lookup, so there is nothing to wait for.

"Not yet" is a success: the command exits 0 with a message saying what is
missing, because nothing about the request was wrong. It exits non-zero only
when the platform refused the question.

An already-verified hostname is refused, and that refusal is deliberate rather
than an oversight. A check re-reads DNS, and a verified apex domain whose TXT
record has since been removed — which is allowed, and documented as allowed —
would be marked failed and stop being routed. Re-checking a live hostname can
only take it out of service.

There is no --wait. Checks are rate limited because DNS answers are cached by
resolvers, so asking again cannot make a record appear sooner, and a scheduled
pass picks up a record that lands later anyway.`,
		Run: func(ctx context.Context, env *Env, args []string) error {
			if len(args) == 0 {
				return Usagef(nil, "name a hostname")
			}

			hostname := args[0]

			target, err := env.ResolveEnvironment(ctx, firstOf(args[1:]))
			if err != nil {
				return err
			}

			client, err := env.Client()
			if err != nil {
				return err
			}

			domain, err := findDomain(ctx, client, target.Environment.ID, hostname)
			if err != nil {
				return err
			}

			result, err := client.VerifyDomain(ctx, domain.ID)
			if err != nil {
				if api.Code(err) == "verification_throttled" {
					return fmt.Errorf("%w\n  the scheduled pass will pick it up", err)
				}

				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(result)
			}

			return reportVerification(env, result)
		},
	}
}

func domainDeleteCommand() *Command {
	var yes bool

	return &Command{
		Name:    "delete",
		Aliases: []string{"remove"},
		Summary: "remove a hostname",
		Usage:   "domain delete <hostname> [<env>]",
		Long: `Stops the platform routing it. The DNS record is yours and is left
alone, so a name whose CNAME still points here will simply stop being served.

The platform hostname cannot be removed. It is issued with the environment and
is never re-issued, so removing it would cost the environment the address that
always works, its SSH host, and the target every custom hostname points at.`,
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&yes, "yes", false, "do not ask for confirmation")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			if len(args) == 0 {
				return Usagef(nil, "name a hostname")
			}

			hostname := args[0]

			target, err := env.ResolveEnvironment(ctx, firstOf(args[1:]))
			if err != nil {
				return err
			}

			client, err := env.Client()
			if err != nil {
				return err
			}

			domain, err := findDomain(ctx, client, target.Environment.ID, hostname)
			if err != nil {
				return err
			}

			if domain.Verified && !yes && env.Interactive() {
				// Asked only for a verified one, because that is the one
				// carrying live traffic. An unverified hostname is usually a
				// typo somebody is cleaning up.
				if !env.confirm(fmt.Sprintf("Stop routing %s, which is live?", domain.Hostname)) {
					return fmt.Errorf("cancelled")
				}
			}

			removed, err := client.DeleteDomain(ctx, domain.ID)
			if err != nil {
				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(removed)
			}

			env.Printer.Good("%s removed from %s", removed.Deleted.Hostname, target.Environment.Name)

			return nil
		},
	}
}

// findDomain turns a hostname into the id the write routes take.
//
// A hostname rather than an id at the command line, because an id is a number
// nobody has: it appears in `--format json` and nowhere a person reads. The
// cost is one list request, which also means a misspelling is answered with
// the hostnames that do exist rather than a bare 404.
func findDomain(ctx context.Context, client *api.Client, environmentID int, hostname string) (*api.Domain, error) {
	list, err := client.Domains(ctx, environmentID)
	if err != nil {
		return nil, err
	}

	// Every trailing dot rather than one, because the control plane normalises
	// with an rtrim before it stores. Trimming a single dot would look up a
	// name nothing holds and answer "no hostname acme.com. here" with a list
	// the name is plainly in.
	wanted := strings.ToLower(strings.TrimRight(strings.TrimSpace(hostname), "."))

	var known []string

	for i := range list.Domains {
		if list.Domains[i].Hostname == wanted {
			return &list.Domains[i], nil
		}

		known = append(known, list.Domains[i].Hostname)
	}

	if len(known) == 0 {
		return nil, fmt.Errorf("this environment has no hostnames at all, which should not be possible")
	}

	return nil, fmt.Errorf("no hostname %s here\n  it has: %s", wanted, strings.Join(known, ", "))
}

// printInstructions writes the DNS records the platform is looking for.
func printInstructions(env *Env, domain *api.Domain) {
	if len(domain.DNSInstructions) == 0 {
		return
	}

	env.Printer.Say("")
	env.Printer.Say("Publish these records:")

	for i := range domain.DNSInstructions {
		instruction := &domain.DNSInstructions[i]

		suffix := ""
		if instruction.Optional {
			suffix = "   (optional)"
		}

		env.Printer.Say("  %-6s %-34s %s%s", instruction.Type, instruction.Name, instruction.Value, suffix)

		if instruction.Note != nil && *instruction.Note != "" {
			env.Printer.Say("         %s", *instruction.Note)
		}
	}

	env.Printer.Say("")
	env.Printer.Say("Then: vallic domain verify %s", domain.Hostname)
}

// reportVerification says what the check found.
func reportVerification(env *Env, result *api.VerificationResult) error {
	if result.Verification.Verified {
		env.Printer.Good("%s is verified", result.Domain.Hostname)

		if !result.Verification.PointsHere {
			// Proved but not pointed. The distinction matters: the hostname is
			// now allowed to be routed and no traffic is arriving on it, which
			// looks like a failure to somebody who only sees "verified".
			env.Printer.Say("  nothing is pointed here yet, so it will not serve until the CNAME moves")
		}

		return nil
	}

	// Not an error. The request was fine and the answer is "not yet", so a
	// retry loop that is working correctly must not exit non-zero.
	env.Printer.Say("%s is not verified yet", result.Domain.Hostname)
	env.Printer.Say("  %s", result.Verification.Message)

	if result.Verification.PointsHere {
		env.Printer.Say("  traffic is already reaching us, so only the proof is missing")
	}

	printInstructions(env, &result.Domain)

	return nil
}

// printCheckReasons writes what the last check found, under the table.
//
// Under it rather than in a column, because the reason is a sentence and it
// is empty for every hostname that is verified or has never been checked: a
// column would be a dash on most rows and would push the hostname it belongs
// to off a narrow terminal.
func printCheckReasons(env *Env, domains []api.Domain) {
	reasons := checkReasons(domains)
	if len(reasons) == 0 {
		return
	}

	env.Printer.Say("")
	env.Printer.Say("Why these are not verified:")

	for _, reason := range reasons {
		env.Printer.Say("  %s", reason)
	}
}

// checkReasons pairs each unverified hostname with the reason the last check
// gave, in the control plane's own words.
//
// The state says a record was checked and was wrong, and stops there. Only
// the message says which way, and the control plane keeps two answers apart
// on purpose: no TXT record at the name at all, or one there holding
// something else. Without this the only way to tell them apart is `domain
// verify`, which spends one of twelve hourly slots and a DNS lookup to
// re-read what the list already carried.
//
// Verified rather than the state string, and never the message text: the
// boolean is sent precisely so a client can ask whether a hostname routes
// without holding a copy of the enum, and a state added on the control plane
// should still print its reason here.
func checkReasons(domains []api.Domain) []string {
	var reasons []string

	for i := range domains {
		domain := &domains[i]

		if domain.Verified || domain.VerificationMessage == "" {
			continue
		}

		reasons = append(reasons, fmt.Sprintf("%s: %s", domain.Hostname, domain.VerificationMessage))
	}

	return reasons
}

// domainState renders a verification state for a table cell.
func domainState(domain *api.Domain) string {
	if domain.Kind == "platform" {
		// Always verified, and saying "verified" beside a name nobody had to
		// prove reads as though somebody did something. The platform owns
		// this zone.
		return "ours"
	}

	return domain.VerificationState
}
