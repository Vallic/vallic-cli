package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vallic/vallic-cli/internal/output"
)

// sshKeyCommand manages the public keys on the signed-in person's account.
//
// Every shell, dump and file copy stands on one of these, and the first thing
// a new developer met used to be "you have no SSH key on file" and a page to
// find in the console. Upsun's `ssh-key:add` puts the key there from the
// terminal the person is already in, and so does this.
func sshKeyCommand() *Command {
	return &Command{
		Name:    "ssh-key",
		Summary: "list, add and remove the SSH keys on your account",
		Long: `The keys every shell, database dump and file copy logs in with. They are
yours rather than a team's: one key opens every environment your roles reach.

They are managed with the credential vallic login stores. A personal access
token cannot reach them: a token reaches only what CI needs.`,
		Children: []*Command{
			sshKeyListCommand(),
			sshKeyAddCommand(),
			sshKeyDeleteCommand(),
		},
	}
}

func sshKeyListCommand() *Command {
	return &Command{
		Name:    "list",
		Summary: "list the keys on your account",
		Usage:   "ssh-key list",
		Run: func(ctx context.Context, env *Env, args []string) error {
			if err := ensureNoExtra(nil, args, 0); err != nil {
				return err
			}

			client, err := env.Client()
			if err != nil {
				return err
			}

			keys, err := client.SSHKeys(ctx)
			if err != nil {
				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(map[string]any{"ssh_keys": keys})
			}

			table := output.Table{
				Columns: []string{"id", "label", "type", "fingerprint", "last used"},
				Empty:   "No SSH keys on your account. Add one: vallic ssh-key add",
			}

			for _, key := range keys {
				used := "never"
				if key.LastUsed != nil {
					used = *key.LastUsed
				}

				table.Rows = append(table.Rows, []string{fmt.Sprint(key.ID), key.Label, key.Type, key.Fingerprint, used})
			}

			return env.Printer.Print(table, nil)
		},
	}
}

func sshKeyAddCommand() *Command {
	var label string

	return &Command{
		Name:    "add",
		Summary: "put a public key on your account",
		Usage:   "ssh-key add [<public-key-file>] [--label <name>]",
		Long: `Reads a public key and adds it to your account. With no file it takes the
one VALLIC_SSH_KEY names, or the first of ~/.ssh/id_ed25519.pub,
id_ecdsa.pub and id_rsa.pub — the keys ssh offers without being told.

Naming the private key is fine: its .pub beside it is what is read, and the
private key never is.

The label is the key's own comment where it has one, which ssh-keygen sets to
user@host. The fingerprint is printed back: compare it with

    ssh-keygen -lf ~/.ssh/id_ed25519.pub`,
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&label, "label", "", "what to call the key on your account (default: its comment)")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			if err := ensureNoExtra(nil, args, 1); err != nil {
				return err
			}

			path, err := publicKeyPath(first(args))
			if err != nil {
				return err
			}

			key, comment, err := readPublicKey(path)
			if err != nil {
				return err
			}

			if label == "" {
				label = comment
			}

			if label == "" {
				label, _ = os.Hostname()
			}

			client, err := env.Client()
			if err != nil {
				return err
			}

			added, err := client.AddSSHKey(ctx, label, key)
			if err != nil {
				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(map[string]any{"ssh_key": added.Key})
			}

			env.Printer.Good("%s", firstNonEmpty(added.Message, "Key "+added.Key.Label+" added."))
			env.Printer.Say("  from %s", path)

			return nil
		},
	}
}

func sshKeyDeleteCommand() *Command {
	return &Command{
		Name:    "delete",
		Aliases: []string{"remove"},
		Summary: "take a key off your account",
		Usage:   "ssh-key delete <id>",
		Long: `Takes the key off your account, by the id ` + "`vallic ssh-key list`" + ` prints.
Nothing logs in with it after that; a session already open stays open.`,
		Run: func(ctx context.Context, env *Env, args []string) error {
			if len(args) == 0 {
				return Usagef(nil, "name a key by the id `vallic ssh-key list` prints")
			}

			if err := ensureNoExtra(nil, args, 1); err != nil {
				return err
			}

			id, err := atoi(args[0])
			if err != nil {
				return Usagef(nil, "%q is not a key id; `vallic ssh-key list` prints them", args[0])
			}

			client, err := env.Client()
			if err != nil {
				return err
			}

			if err := client.RemoveSSHKey(ctx, id); err != nil {
				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(map[string]any{"deleted": map[string]any{"id": id}})
			}

			env.Printer.Good("Key %d removed.", id)

			return nil
		},
	}
}

// The public keys ssh offers without being told, in the order it tries them.
var defaultPublicKeys = []string{"id_ed25519.pub", "id_ecdsa.pub", "id_rsa.pub"}

// publicKeyPath settles which file to read.
//
// A private key named is read through its .pub, which is the mistake most
// worth making harmless: the private half must never leave the machine, and
// it is what VALLIC_SSH_KEY and --identity name everywhere else.
func publicKeyPath(named string) (string, error) {
	if named == "" {
		named = os.Getenv("VALLIC_SSH_KEY")
	}

	if named != "" {
		named = expandHome(named)

		if !strings.HasSuffix(named, ".pub") {
			if _, err := os.Stat(named + ".pub"); err == nil {
				return named + ".pub", nil
			}
		}

		return named, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot tell where ~/.ssh is: %w\n  name the key: vallic ssh-key add path/to/key.pub", err)
	}

	for _, name := range defaultPublicKeys {
		path := filepath.Join(home, ".ssh", name)

		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	return "", errors.New("no public key in ~/.ssh\n  make one: ssh-keygen -t ed25519\n  then: vallic ssh-key add")
}

// readPublicKey reads a public key file, and refuses anything else.
//
// The control plane checks the key properly; this checks only that the file
// is the right half, so a private key is refused here, before any of it is
// sent anywhere.
func readPublicKey(path string) (key, comment string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("cannot read %s: %w", path, err)
	}

	text := strings.TrimSpace(string(raw))

	if strings.Contains(text, "PRIVATE KEY") {
		return "", "", fmt.Errorf("%s is a private key, which never leaves this machine\n  name its .pub instead", path)
	}

	fields := strings.Fields(text)

	if len(fields) < 2 || !(strings.HasPrefix(fields[0], "ssh-") || strings.HasPrefix(fields[0], "ecdsa-") || strings.HasPrefix(fields[0], "sk-")) {
		return "", "", fmt.Errorf("%s does not hold an OpenSSH public key, which starts ssh-ed25519, ssh-rsa or ecdsa-", path)
	}

	if len(fields) > 2 {
		comment = strings.Join(fields[2:], " ")
	}

	return strings.Join(fields[:2], " ") + optionalComment(comment), comment, nil
}

func optionalComment(comment string) string {
	if comment == "" {
		return ""
	}

	return " " + comment
}

// expandHome turns a leading ~/ into the home directory, for a path that
// reached here without a shell to expand it: from VALLIC_SSH_KEY, or quoted.
func expandHome(path string) string {
	rest, found := strings.CutPrefix(path, "~/")
	if !found {
		return path
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}

	return filepath.Join(home, rest)
}
