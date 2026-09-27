package ssh

import (
	"strings"
	"testing"

	"github.com/vallic/vallic-cli/internal/api"
)

// The string handed to ssh is parsed by a shell inside the container, so an
// argument that is quoted wrongly is an argument that becomes two — or, with
// a quote in it, becomes something else entirely. Wrapping in single quotes,
// and breaking an embedded single quote out into an escaped one, is the only
// form that is safe for every other character -- nothing inside single quotes
// is special to a POSIX shell.
func TestQuote(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain stays bare", "status", "status"},
		{"a flag stays bare", "--uri=https://example.com", "--uri=https://example.com"},
		{"a path stays bare", "/mnt/files/public", "/mnt/files/public"},
		{"empty becomes a quoted nothing", "", "''"},
		{"a space is quoted", "two words", "'two words'"},
		{"a semicolon is quoted", "status; rm -rf /", "'status; rm -rf /'"},
		{"a pipe is quoted", "status | tee out", "'status | tee out'"},
		{"a backtick is quoted", "echo `id`", "'echo `id`'"},
		{"a dollar is quoted", "echo $HOME", "'echo $HOME'"},
		{"an ampersand is quoted", "sleep 1 &", "'sleep 1 &'"},
		{"a newline is quoted", "one\ntwo", "'one\ntwo'"},

		// The case the naive implementation gets wrong. A closing quote, an
		// escaped literal quote, and a reopening quote.
		{"an embedded quote is broken out", "it's", `'it'\''s'`},
		{
			"a quote used to escape the quoting",
			`'; id; '`,
			`''\''; id; '\'''`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := quote(tc.in); got != tc.want {
				t.Errorf("quote(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A command arrives as one argument to ssh, with each word quoted
// independently — which is what lets `vallic ssh -- drush "sql:query" "SELECT
// 1"` reach the container as three words rather than five.
func TestRunPassesOneQuotedString(t *testing.T) {
	target := targetFor(2299)

	cmd := target.Run([]string{"drush", "sql:query", "SELECT 1"}, false)

	last := cmd.Args[len(cmd.Args)-1]
	if last != "drush sql:query 'SELECT 1'" {
		t.Errorf("remote command = %q, want the last word quoted and the others bare", last)
	}

	if !containsInOrder(cmd.Args, "-p", "2299") {
		t.Errorf("args = %v, want the project's port", cmd.Args)
	}

	// -T where there is no terminal, or ssh warns about a pseudo-terminal it
	// could not allocate into whatever a pipeline was capturing.
	if !contains(cmd.Args, "-T") {
		t.Errorf("args = %v, want -T for a non-interactive command", cmd.Args)
	}
}

// The port is never a constant. It is chosen at random per project in
// 2000-2999, so a client that assumed 22 or 2231 would be wrong for every
// project on the platform.
func TestPortComesFromTheTarget(t *testing.T) {
	for _, port := range []int{2000, 2231, 2457, 2999} {
		cmd := targetFor(port).Shell()

		if !containsInOrder(cmd.Args, "-p", itoa(port)) {
			t.Errorf("args = %v, want -p %d", cmd.Args, port)
		}
	}
}

// Both flags together. -i alone still lets the agent offer every key it holds
// first, and sshd allows six attempts before refusing — so a developer with a
// handful of keys is refused before the right one is tried, on a key that is
// correctly installed.
func TestIdentityIsOfferedExclusively(t *testing.T) {
	target := targetFor(2299)
	target.Identity = "/home/alice/.ssh/vallic"

	cmd := target.Shell()

	if !containsInOrder(cmd.Args, "-o", "IdentitiesOnly=yes") {
		t.Errorf("args = %v, want IdentitiesOnly=yes beside -i", cmd.Args)
	}

	if !containsInOrder(cmd.Args, "-i", "/home/alice/.ssh/vallic") {
		t.Errorf("args = %v, want the identity file", cmd.Args)
	}
}

// A verb is a word, and gets no pseudo-terminal: a dump is a stream and a pty
// in the middle of one would translate newlines and corrupt it.
func TestVerbStreamsWithoutATerminal(t *testing.T) {
	cmd := targetFor(2299).Verb("db-export", false)

	if contains(cmd.Args, "-t") {
		t.Errorf("args = %v, want no pty for a stream", cmd.Args)
	}

	if !contains(cmd.Args, "-T") {
		t.Errorf("args = %v, want -T", cmd.Args)
	}

	if cmd.Args[len(cmd.Args)-1] != "db-export" {
		t.Errorf("last arg = %q, want the verb", cmd.Args[len(cmd.Args)-1])
	}
}

// A prompt is the same verb mechanism with the pty asked for, because that is
// the only difference between a client that reads a dump and one somebody
// types into.
func TestVerbTakesATerminalWhenAsked(t *testing.T) {
	cmd := targetFor(2299).Verb("db-cli", true)

	if !contains(cmd.Args, "-t") {
		t.Errorf("args = %v, want a pty for a prompt", cmd.Args)
	}

	if contains(cmd.Args, "-T") {
		t.Errorf("args = %v, want no -T alongside the pty", cmd.Args)
	}

	if cmd.Args[len(cmd.Args)-1] != "db-cli" {
		t.Errorf("last arg = %q, want the verb", cmd.Args[len(cmd.Args)-1])
	}
}

// rsync gets the port and the identity through -e, because neither fits its
// own flags, and both sides end in a slash — which is what makes rsync copy
// the contents rather than nesting the directory inside itself.
func TestRsyncCarriesTheTransportAndTrailingSlashes(t *testing.T) {
	target := targetFor(2299)
	target.Identity = "/key"

	cmd := target.Rsync("./files/", target.Remote("/mnt/files/public/"), nil)

	transport := ""
	for i, arg := range cmd.Args {
		if arg == "-e" && i+1 < len(cmd.Args) {
			transport = cmd.Args[i+1]
		}
	}

	if !strings.Contains(transport, "-p 2299") {
		t.Errorf("-e %q, want the port in it", transport)
	}

	if !strings.Contains(transport, "IdentitiesOnly=yes") {
		t.Errorf("-e %q, want IdentitiesOnly in it", transport)
	}

	last := cmd.Args[len(cmd.Args)-1]
	if last != "vc-acme@acme-1-production.vallic.cloud:/mnt/files/public/" {
		t.Errorf("destination = %q, want user@host:path with a trailing slash", last)
	}
}

// A nil SSH target is a plain no from the control plane — the role does not
// admit a shell, or nothing runs the environment yet — and wants a sentence
// rather than a nil dereference.
func TestNilTargetIsAnError(t *testing.T) {
	_, err := New(&api.EnvironmentDetail{
		Environment: api.Environment{Slug: "acme-1-production"},
	}, "")

	var noShell *ErrNoShell
	if !asNoShell(err, &noShell) {
		t.Fatalf("error = %v, want an *ErrNoShell", err)
	}

	if !strings.Contains(err.Error(), "acme-1-production") {
		t.Errorf("error = %q, want the environment named", err)
	}

	if !strings.Contains(err.Error(), "developer") {
		t.Errorf("error = %q, want the role that would be enough", err)
	}
}

func targetFor(port int) *Target {
	return &Target{SSHTarget: api.SSHTarget{
		Host:      "acme-1-production.vallic.cloud",
		Port:      port,
		User:      "vc-acme",
		DBExport:  "db-export",
		DBImport:  "db-import",
		FilesPath: "/mnt/files/public",
		HasKeys:   true,
	}}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}

	return false
}

func containsInOrder(haystack []string, first, second string) bool {
	for i := 0; i+1 < len(haystack); i++ {
		if haystack[i] == first && haystack[i+1] == second {
			return true
		}
	}

	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}

	return string(digits)
}

func asNoShell(err error, target **ErrNoShell) bool {
	if e, ok := err.(*ErrNoShell); ok {
		*target = e

		return true
	}

	return false
}
