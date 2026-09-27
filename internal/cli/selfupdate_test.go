package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vallic/vallic-cli/internal/api"
)

// Nothing here downloads anything. What is tested is the four decisions that
// happen before and after the bytes move: which entry in the manifest is this
// machine's, whether the digest matches, whether the binary is this CLI's to
// replace, and whether the replacement is atomic. Reaching a release host to
// test an update path would test the release host.

// The key has to be the one the control plane publishes, and these are the
// four literal strings in its cli_downloads config. Written out rather than
// derived, because a helper that built them the same way platformKey does
// would agree with itself while both disagreed with the manifest, which is
// exactly what happened: this was written keyed on the hyphen the built files
// use, the control plane keyed on the slash Go and the Makefile use, and
// self-update found no download for any platform on earth.
//
// The hyphen is a filename. publish-cli.py turns the key into one; nothing
// turns a filename back into a key.
//
// A regression that dropped the OS would ask for "arm64" and be handed
// whichever kernel's build the manifest happened to key that way.
func TestPlatformKeyMatchesWhatTheControlPlanePublishes(t *testing.T) {
	cases := map[string][2]string{
		"linux/amd64":  {"linux", "amd64"},
		"linux/arm64":  {"linux", "arm64"},
		"darwin/amd64": {"darwin", "amd64"},
		"darwin/arm64": {"darwin", "arm64"},
	}

	for want, pair := range cases {
		if got := platformKey(pair[0], pair[1]); got != want {
			t.Errorf("platformKey(%q, %q) = %q, want %q", pair[0], pair[1], got, want)
		}
	}
}

// An entry missing either half is dropped rather than half-used. The control
// plane drops it before sending and this drops it on arrival, because the
// failure is the same at both ends: a URL with no checksum is a binary nobody
// verified.
func TestADownloadMissingEitherHalfIsNotOffered(t *testing.T) {
	release := api.CLIRelease{
		Version: "0.6.0",
		Downloads: api.CLIDownloads{
			"linux-amd64":  {URL: "https://example.invalid/vallic-linux-amd64", SHA256: strings.Repeat("a", 64)},
			"linux-arm64":  {URL: "https://example.invalid/vallic-linux-arm64"},
			"darwin-arm64": {SHA256: strings.Repeat("b", 64)},
		},
	}

	if _, ok := release.Download("linux-amd64"); !ok {
		t.Error("a complete entry was dropped")
	}

	for _, platform := range []string{"linux-arm64", "darwin-arm64", "windows-amd64"} {
		if _, ok := release.Download(platform); ok {
			t.Errorf("Download(%q) was offered, want it dropped", platform)
		}
	}

	if got, want := strings.Join(release.Platforms(), ","), "linux-amd64"; got != want {
		t.Errorf("Platforms() = %q, want %q", got, want)
	}
}

// The manifest of a control plane that has published nothing is the one this
// most has to be able to read, and it is the one PHP renders as a list: an
// empty array and an empty object are the same value there, and unmarshalling
// `[]` into a Go map fails and takes the whole response with it. The person
// would have been told their control plane sent something unreadable, when
// what it sent was "nothing is published".
func TestAManifestWithNothingPublishedIsStillReadable(t *testing.T) {
	for _, body := range []string{
		`{"version":null,"downloads":[]}`,
		`{"version":null}`,
		`{"version":"","downloads":{}}`,
	} {
		var release api.CLIRelease

		if err := json.Unmarshal([]byte(body), &release); err != nil {
			t.Errorf("reading %s = %v, want it read as nothing published", body, err)
			continue
		}

		if release.Version != "" {
			t.Errorf("reading %s gave version %q, want none", body, release.Version)
		}

		if len(release.Platforms()) != 0 {
			t.Errorf("reading %s gave platforms %v, want none", body, release.Platforms())
		}
	}
}

// The checksum is the whole security argument, so what counts as one is
// narrow. A digest that is not a SHA-256 means this version cannot verify the
// download, and the only safe reading of that is to stop.
func TestWantedDigestTakesOnlyASHA256(t *testing.T) {
	digest := strings.Repeat("ab", 32)

	for _, spelling := range []string{digest, strings.ToUpper(digest), "sha256:" + digest, "  " + digest + "  "} {
		got, err := wantedDigest(spelling)
		if err != nil {
			t.Errorf("wantedDigest(%q) = %v, want it accepted", spelling, err)
			continue
		}

		if got != digest {
			t.Errorf("wantedDigest(%q) = %q, want %q", spelling, got, digest)
		}
	}

	for _, bad := range []string{"", "sha256:", digest[:63], digest + "a", strings.Repeat("z", 64), "md5:" + digest} {
		if _, err := wantedDigest(bad); err == nil {
			t.Errorf("wantedDigest(%q) = nil error, want a refusal", bad)
		}
	}
}

// The digest is taken from the bytes on their way through, and a single
// changed byte has to be the difference between installing and refusing.
func TestVerifiedCopyRefusesWhatWasNotPublished(t *testing.T) {
	published := []byte("this is a binary, near enough")
	sum := sha256.Sum256(published)
	want := hex.EncodeToString(sum[:])

	var kept bytes.Buffer
	if err := verifiedCopy(&kept, bytes.NewReader(published), want); err != nil {
		t.Fatalf("verifiedCopy() = %v, want the published bytes accepted", err)
	}

	if !bytes.Equal(kept.Bytes(), published) {
		t.Error("verifiedCopy() wrote something other than what it read")
	}

	substituted := append([]byte{}, published...)
	substituted[0] = 'T'

	err := verifiedCopy(&bytes.Buffer{}, bytes.NewReader(substituted), want)
	if err == nil {
		t.Fatal("verifiedCopy() accepted bytes that are not what the control plane published")
	}

	// Both digests, because a mismatch is either a truncated download or a
	// substituted binary and afterwards those are worth telling apart.
	if !strings.Contains(err.Error(), want) {
		t.Errorf("verifiedCopy() = %q, want it to name the digest it wanted", err)
	}
}

// A copy something else installed is that tool's to replace. The check reads
// the path and nothing else, which is a guess in both directions: /usr/local/bin
// has to stay updatable because it is where a hand install lands, and /usr/bin
// has to be refused because it is the package manager's.
func TestInstalledByNamesTheToolThatOwnsThePath(t *testing.T) {
	environment := map[string]string{"HOME": "/home/alice"}
	getenv := func(key string) string { return environment[key] }

	cases := []struct {
		path string
		want string
	}{
		{"/opt/homebrew/Cellar/vallic/0.6.0/bin/vallic", "Homebrew"},
		{"/usr/local/Cellar/vallic/0.6.0/bin/vallic", "Homebrew"},
		{"/home/linuxbrew/.linuxbrew/bin/vallic", "Homebrew"},
		{"/nix/store/abc123-vallic-0.6.0/bin/vallic", "Nix"},
		{"/snap/vallic/current/bin/vallic", "snap"},
		{"/home/alice/go/bin/vallic", "the Go toolchain"},
		{"/usr/bin/vallic", "this system's package manager"},
		{"/bin/vallic", "this system's package manager"},

		// Updatable: none of these belong to a package manager, and
		// /usr/local/bin in particular is where curl and `make install` put
		// things.
		{"/usr/local/bin/vallic", ""},
		{"/home/alice/.local/bin/vallic", ""},
		{"/home/alice/code/vallic-cli/dist/vallic", ""},

		// Not /snap. A string prefix would have read it as one.
		{"/snapshots/bin/vallic", ""},
	}

	for _, tc := range cases {
		got := ""
		if by := installedBy(tc.path, getenv); by != nil {
			got = by.Tool
		}

		if got != tc.want {
			t.Errorf("installedBy(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// GOBIN wins over GOPATH, which wins over the default, and a GOPATH list
// installs into its first entry. A copy at any of them is the toolchain's.
func TestInstalledByFollowsWhereGoInstallWouldHavePutIt(t *testing.T) {
	cases := []struct {
		name        string
		environment map[string]string
		path        string
		want        string
	}{
		{
			name:        "GOBIN",
			environment: map[string]string{"GOBIN": "/opt/tools/bin", "HOME": "/home/alice"},
			path:        "/opt/tools/bin/vallic",
			want:        "the Go toolchain",
		},
		{
			name:        "GOBIN set, so the default is not the toolchain's",
			environment: map[string]string{"GOBIN": "/opt/tools/bin", "HOME": "/home/alice"},
			path:        "/home/alice/go/bin/vallic",
			want:        "",
		},
		{
			name:        "the first entry of a GOPATH list",
			environment: map[string]string{"GOPATH": "/home/alice/work:/home/alice/other", "HOME": "/home/alice"},
			path:        "/home/alice/work/bin/vallic",
			want:        "the Go toolchain",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ""
			if by := installedBy(tc.path, func(key string) string { return tc.environment[key] }); by != nil {
				got = by.Tool
			}

			if got != tc.want {
				t.Errorf("installedBy(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// The refusal names the tool, and names the one thing a person whose copy this
// misread can do about it. A refusal that only said no would leave them with a
// CLI that will not update and no next step.
func TestARefusalSaysWhatToDoInstead(t *testing.T) {
	message := (owner{Tool: "Homebrew"}).refusal("/opt/homebrew/bin/vallic").Error()

	for _, want := range []string{"Homebrew", "/opt/homebrew/bin/vallic", "by hand"} {
		if !strings.Contains(message, want) {
			t.Errorf("refusal() = %q, want it to mention %q", message, want)
		}
	}

	withCommand := (owner{Tool: "the Go toolchain", Command: "go install example/cmd@latest"}).refusal("/home/alice/go/bin/vallic").Error()

	if !strings.Contains(withCommand, "go install example/cmd@latest") {
		t.Errorf("refusal() = %q, want it to print the command", withCommand)
	}
}

// The swap is a rename inside one directory, and the file is executable before
// it happens. A half-written binary in place is a CLI that no longer runs and
// cannot update itself, which is the failure both rules exist to prevent.
func TestInstallReplacesTheBinaryInOneStep(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "vallic")
	staged := target + ".new"

	if err := os.WriteFile(target, []byte("the old binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(staged, []byte("the new binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := install(staged, target, modeFor(target)); err != nil {
		t.Fatalf("install() = %v", err)
	}

	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}

	if got, want := string(contents), "the new binary"; got != want {
		t.Errorf("the target holds %q, want %q", got, want)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("the installed binary is %v, want it executable", info.Mode().Perm())
	}

	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Error("the staged file is still there, want it consumed by the rename")
	}
}

// The mode of the binary being replaced is kept, so a copy installed
// deliberately private stays private. A target that carries no execute bit at
// all falls back to 0o755: copying that mode would install a file nobody can
// run.
func TestModeForKeepsWhatTheBinaryAlreadyHad(t *testing.T) {
	dir := t.TempDir()

	private := filepath.Join(dir, "private")
	if err := os.WriteFile(private, nil, 0o700); err != nil {
		t.Fatal(err)
	}

	if got, want := modeFor(private), os.FileMode(0o700); got != want {
		t.Errorf("modeFor(private) = %v, want %v", got, want)
	}

	notExecutable := filepath.Join(dir, "not-executable")
	if err := os.WriteFile(notExecutable, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if got, want := modeFor(notExecutable), os.FileMode(0o755); got != want {
		t.Errorf("modeFor(not executable) = %v, want %v", got, want)
	}

	if got, want := modeFor(filepath.Join(dir, "missing")), os.FileMode(0o755); got != want {
		t.Errorf("modeFor(missing) = %v, want %v", got, want)
	}
}

// A binary somewhere this user cannot write fails before anything is
// downloaded, and the failure names the directory, which is the thing that has
// to change. A read-only mount arrives here too and keeps the system's own
// words for itself, because there is no remedy to offer for one.
func TestCannotWriteNamesTheDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is not refused by the permission this is about")
	}

	dir := t.TempDir()

	// Restored before the test framework removes the directory, which cannot
	// remove what it cannot write to.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(dir, "vallic")

	staged, err := os.OpenFile(target+".new", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o700)
	if err == nil {
		staged.Close()
		t.Fatal("the staged file was created in a directory this user cannot write to")
	}

	message := cannotWrite(target, err).Error()

	for _, want := range []string{dir, "as the user that installed it"} {
		if !strings.Contains(message, want) {
			t.Errorf("cannotWrite() = %q, want it to mention %q", message, want)
		}
	}
}

// The tag a release is built from carries a "v" and is stamped into the
// binary; the version pinned on the control plane is typed by a person and may
// not. Reading those as two versions would download the binary already
// installed, every single run.
func TestSameVersionIgnoresHowTheVIsSpelled(t *testing.T) {
	for _, pair := range [][2]string{
		{"v0.6.0", "0.6.0"},
		{"0.6.0", "v0.6.0"},
		{"v0.6.0", "v0.6.0"},
		{" 0.6.0 ", "0.6.0"},
	} {
		if !sameVersion(pair[0], pair[1]) {
			t.Errorf("sameVersion(%q, %q) = false, want them the same release", pair[0], pair[1])
		}
	}

	for _, pair := range [][2]string{
		{"v0.6.0", "0.6.1"},
		{"0.6.0", "0.6.0-rc1"},
		{"dev", "0.6.0"},
	} {
		if sameVersion(pair[0], pair[1]) {
			t.Errorf("sameVersion(%q, %q) = true, want them different releases", pair[0], pair[1])
		}
	}
}

// A published version lower than the installed one is a release being taken
// back, and is worth a confirmation rather than a refusal. What must not
// happen is calling an update a downgrade because the version is spelled in a
// way this cannot order.
func TestOlderThanKnowsWhenItCannotTell(t *testing.T) {
	cases := []struct {
		published  string
		installed  string
		older      bool
		comparable bool
	}{
		{published: "0.5.9", installed: "0.6.0", older: true, comparable: true},
		{published: "0.6.0", installed: "0.5.9", older: false, comparable: true},
		{published: "0.6.0", installed: "0.6.0", older: false, comparable: true},
		{published: "v0.6.1", installed: "0.6.0", older: false, comparable: true},
		{published: "0.6", installed: "0.6.0", older: false, comparable: true},
		{published: "0.10.0", installed: "0.9.0", older: false, comparable: true},

		// A build from source, and a build `git describe` stamped between
		// tags. The first cannot be ordered at all; the second is ordered by
		// the tag it is past, which is not an earlier release than that tag.
		{published: "0.6.0", installed: "dev", older: false, comparable: false},
		{published: "0.6.0", installed: "0.6.0-4-gabc123", older: false, comparable: true},
	}

	for _, tc := range cases {
		older, comparable := olderThan(tc.published, tc.installed)

		if older != tc.older || comparable != tc.comparable {
			t.Errorf("olderThan(%q, %q) = %v, %v, want %v, %v",
				tc.published, tc.installed, older, comparable, tc.older, tc.comparable)
		}
	}
}

// A release manifest naming an http download is a mistake in the manifest.
// The digest would still catch a substituted binary, so this is not what makes
// the update safe, and accepting it quietly is how it stays a mistake.
func TestOnlyHTTPSIsFetched(t *testing.T) {
	if err := httpsOnly("https://example.invalid/vallic-linux-amd64"); err != nil {
		t.Errorf("httpsOnly(https) = %v, want it accepted", err)
	}

	for _, bad := range []string{"http://example.invalid/vallic", "file:///tmp/vallic", "://"} {
		if err := httpsOnly(bad); err == nil {
			t.Errorf("httpsOnly(%q) = nil error, want a refusal", bad)
		}
	}
}

// Only `--dev` asks for anything. The ordinary run sends no channel at all
// rather than an empty one, so one URL stays one cache entry on the control
// plane and every older client makes the identical request.
//
// There is deliberately no `--stable`: an installation serving dev is a test
// console, everything pointed at it is being tested, and a client opting out
// of that would report results for a binary nobody meant to look at.
func TestOnlyDevIsEverAskedFor(t *testing.T) {
	if got := channelAsked(true); got != api.ChannelDev {
		t.Errorf("channelAsked(true) = %q, want %q", got, api.ChannelDev)
	}

	if got := channelAsked(false); got != "" {
		t.Errorf("channelAsked(false) = %q, want the parameter left off entirely", got)
	}
}

// The channel reaches --format json, so a pipeline can refuse a dev build
// without reading a version string for `-dev-`.
//
// Reported as it arrived rather than defaulted to "stable" here: a control
// plane from before channels existed sends none, and a word this client
// invented would be one a script could not tell from one the platform sent.
func TestTheReportCarriesWhicheverChannelAnswered(t *testing.T) {
	for _, tc := range []struct {
		name    string
		release api.CLIRelease
		want    string
	}{
		{"dev", api.CLIRelease{Channel: api.ChannelDev, Version: "0.4.1-dev-202609241830"}, api.ChannelDev},
		{"stable", api.CLIRelease{Channel: api.ChannelStable, Version: "0.4.0"}, api.ChannelStable},
		{"an older control plane says nothing", api.CLIRelease{Version: "0.4.0"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := selfUpdateReport(
				"/usr/local/bin/vallic", "linux/amd64", "0.3.0",
				&tc.release, api.CLIDownload{URL: "https://example/x", SHA256: "abc"}, "updated",
			)

			body, ok := report["self_update"].(map[string]any)
			if !ok {
				t.Fatalf("report = %#v, want a self_update member", report)
			}

			if got := body["channel"]; got != tc.want {
				t.Errorf("channel = %#v, want %q", got, tc.want)
			}
		})
	}
}

// A dev version is ahead of the release it is a pre-release of, and level with
// that release rather than behind it.
//
// This is what decides whether self-update asks "this goes backwards?", and
// both answers here are the ones somebody wants. Coming off a dev build onto
// the release it was building towards is an ordinary update and must not
// prompt; coming off it onto an *earlier* release is a downgrade and must.
//
// It works because versionParts drops everything from the first hyphen, which
// is not semver's rule and is the right one for the only question asked here.
func TestADevBuildOrdersAgainstTheReleaseItPrecedes(t *testing.T) {
	const dev = "0.4.1-dev-202609241830"

	// Onto the release it was becoming: not backwards.
	older, comparable := olderThan("0.4.1", dev)
	if !comparable || older {
		t.Errorf("olderThan(0.4.1, %s) = (%v, %v), want not-older and comparable", dev, older, comparable)
	}

	// Onto an earlier release: backwards, and somebody is asked.
	older, comparable = olderThan("0.4.0", dev)
	if !comparable || !older {
		t.Errorf("olderThan(0.4.0, %s) = (%v, %v), want older and comparable", dev, older, comparable)
	}

	// A newer dev build of the same base: not backwards, and not the same
	// version either, so it installs without a prompt.
	newer := "0.4.1-dev-202609250900"
	older, comparable = olderThan(newer, dev)

	if !comparable || older {
		t.Errorf("olderThan(%s, %s) = (%v, %v), want not-older", newer, dev, older, comparable)
	}

	if sameVersion(newer, dev) {
		t.Error("two dev builds of one base compare the same, so neither would ever install")
	}
}
