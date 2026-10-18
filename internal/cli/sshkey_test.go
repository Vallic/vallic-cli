package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vallic/vallic-cli/internal/api"
)

// The private half never leaves the machine. Named, it is read through the
// .pub beside it; read by mistake, it is refused before anything is sent.
func TestOnlyThePublicHalfOfAKeyIsRead(t *testing.T) {
	dir := t.TempDir()
	private := filepath.Join(dir, "vallic")

	if err := os.WriteFile(private, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nb3Blbn...\n-----END OPENSSH PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := readPublicKey(private); err == nil || !strings.Contains(err.Error(), "private key") {
		t.Errorf("readPublicKey(private) = %v, want a refusal", err)
	}

	if err := os.WriteFile(private+".pub", []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample valentino@laptop\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := publicKeyPath(private)
	if err != nil || path != private+".pub" {
		t.Fatalf("publicKeyPath(private) = %q, %v, want the .pub beside it", path, err)
	}

	key, comment, err := readPublicKey(path)
	if err != nil {
		t.Fatalf("readPublicKey() = %v", err)
	}

	if key != "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample valentino@laptop" {
		t.Errorf("key = %q", key)
	}

	// The label a key is given when nobody names one.
	if comment != "valentino@laptop" {
		t.Errorf("comment = %q, want the key's own", comment)
	}

	notAKey := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notAKey, []byte("remember the milk\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := readPublicKey(notAKey); err == nil {
		t.Error("readPublicKey() accepted a file that holds no key")
	}
}

func TestTheSSHKeyAnswerDecodesAsTheControlPlaneSendsIt(t *testing.T) {
	var answer api.SSHKeyAdded
	body := `{"ssh_key":{"id":7,"label":"laptop","type":"ssh-ed25519","bits":null,"fingerprint":"SHA256:abc","comment":"v@laptop","last_used":null},"message":"Key laptop added."}`

	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatal(err)
	}

	if answer.Key.ID != 7 || answer.Key.Fingerprint != "SHA256:abc" || answer.Key.LastUsed != nil || answer.Message == "" {
		t.Errorf("decoded as %+v", answer)
	}
}
