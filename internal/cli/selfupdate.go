package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/vallic/vallic-cli/internal/api"
)

// maxBinaryBytes bounds the download.
//
// This binary is a few megabytes. A cap two orders of magnitude above that is
// far past any real one and well short of filling a disk, which is what it is
// for: a URL that answers forever would otherwise be written until the
// filesystem stopped.
const maxBinaryBytes = 256 << 20

// downloadTimeout is how long the binary itself has to arrive.
//
// Ten minutes rather than the API client's sixty seconds, which is set for
// requests that answer with a page of JSON. A binary on a hotel connection is
// minutes, and a timeout that cuts a download which was going to succeed is a
// command that fails for a reason nobody can act on.
const downloadTimeout = 10 * time.Minute

// selfUpdateCommand replaces the binary this process is running.
//
// Three things make that safe to do to a running executable, and all three are
// the agent's (agent/internal/agent/upgrade.go), for the same reasons:
//
//   - The checksum is verified before anything is moved, and it comes from the
//     control plane rather than from beside the download. A digest served by
//     whoever served the bytes proves the transfer was not corrupted and proves
//     nothing about the release, so the two live in different systems on
//     purpose: the release host has the binary, the control plane has the
//     digest, and substituting a binary means having both.
//   - The swap is a rename inside one directory, so the path is never half a
//     file. A binary written over in place and interrupted is a CLI that no
//     longer runs and cannot update itself.
//   - Replacing a running executable is allowed on Linux and macOS. The rename
//     unlinks the old inode, which this process keeps open until it exits, so
//     the command finishes in the old code and the next one starts in the new.
//
// What it deliberately does not do is run what it downloaded to see whether it
// works. A binary is verified before it is installed, and running an unverified
// one to find out whether it is safe is the wrong order.
func selfUpdateCommand() *Command {
	var (
		dryRun bool
		yes    bool
		dev    bool
	)

	return &Command{
		Name:    "self-update",
		Summary: "replace this binary with the version the platform publishes",
		Usage:   "self-update [--dry-run] [--yes] [--dev]",
		Long: `Asks the control plane which version it publishes, fetches that build
from the address it names, checks it against the checksum the control plane
holds for it, and renames the result over this binary.

The bytes and the checksum come from two different systems on purpose. A
checksum served by whoever served the binary can only say the download arrived
intact; it says nothing about a release that was replaced at the source.

Nothing is replaced before the checksum matches, and what was downloaded is
never run to find out whether it works.

A copy installed by Homebrew, by a distribution's package manager, by a snap or
by the Go toolchain is refused rather than fought over: that tool owns the file
and is what should replace it.

--dry-run prints what would happen and downloads nothing.

--dev asks for the build cut from the CLI's dev branch instead of the one the
control plane serves by default. It is for trying a change before it is
released, on your own machine, without moving anybody else onto it: nothing is
remembered, so the next self-update without --dev goes back to the ordinary
build. A console that publishes no dev build says so rather than quietly
handing over the stable one.`,
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&dryRun, "dry-run", false, "print what would happen and download nothing")
			fs.BoolVar(&yes, "yes", false, "do not ask for confirmation")
			fs.BoolVar(&dev, "dev", false, "ask for the dev build instead of the published one")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			if err := ensureNoExtra(nil, args, 0); err != nil {
				return err
			}

			target, err := runningBinary()
			if err != nil {
				return err
			}

			// Before the control plane is asked, because the answer does not
			// depend on what it says and a refusal that first spends somebody's
			// time on a request is a slower way to say the same thing.
			if by := installedBy(target, os.Getenv); by != nil {
				return by.refusal(target)
			}

			// No credential needed, and deliberately not required.
			client, err := env.PublicClient()
			if err != nil {
				return err
			}

			release, err := client.LatestCLI(ctx, channelAsked(dev))
			if err != nil {
				return err
			}

			// A version named with no binary behind it, or no version at all.
			// Both are the same answer to a client: there is nothing here to
			// install, which is a refusal rather than a success with no work.
			//
			// Named for the channel, because with two of them "publishes no
			// version" is ambiguous in the way that matters: an installation
			// with a perfectly good stable build and nothing on dev would
			// otherwise read as one that has published nothing at all, and the
			// thing to do about it -- drop the flag -- goes unsaid.
			if release.Version == "" {
				if dev {
					return fmt.Errorf(
						"%s publishes no dev build of this binary\n"+
							"  it may publish a released one: vallic self-update",
						client.BaseURL(),
					)
				}

				return fmt.Errorf("%s publishes no version of this binary to update to", client.BaseURL())
			}

			platform := platformKey(runtime.GOOS, runtime.GOARCH)

			download, ok := release.Download(platform)
			if !ok {
				published := release.Platforms()

				// Half a manifest: a version named, and every entry behind it
				// missing a URL or a checksum. Both ends drop those, so this
				// is what a publish that went wrong looks like from here.
				if len(published) == 0 {
					return fmt.Errorf("release %s has no build behind it that can be verified", release.Version)
				}

				return fmt.Errorf(
					"release %s has no build for %s, only for %s",
					release.Version, platform, strings.Join(published, ", "),
				)
			}

			installed := env.Version

			if sameVersion(release.Version, installed) {
				if env.Printer.Structured() {
					return env.Printer.Value(selfUpdateReport(target, platform, installed, release, download, "current"))
				}

				env.Printer.Say("Already on %s.", installed)

				return nil
			}

			// The control plane is the authority on which version to run, and a
			// lower one is how a release that turned out to be bad is taken
			// back. Refusing a downgrade outright would block the one case the
			// pin exists for, so it happens, once somebody has seen that it is
			// what is happening.
			older, comparable := olderThan(release.Version, installed)
			backwards := comparable && older

			// Refused here rather than after the plan is printed, so that
			// "Replacing" is only ever said about something that is going to
			// happen. A --dry-run is exempt: it is asking what would happen,
			// and the answer includes the confirmation it would have asked
			// for.
			if backwards && !yes && !dryRun && !env.Interactive() {
				return fmt.Errorf(
					"%s is published and %s is installed, so this goes backwards\n"+
						"  there is no terminal to confirm on, so pass --yes if you mean it",
					release.Version, installed,
				)
			}

			// Said before it is done, and in the same words whether or not it
			// is going to happen: a --dry-run that described the plan
			// differently to the run would be a dry run of something else.
			verb := "Replacing"
			if dryRun {
				verb = "Would replace"
			}

			env.Printer.Say("%s %s", verb, target)
			env.Printer.Say("  installed  %s", installed)
			env.Printer.Say("  published  %s", release.Version)

			// Only for dev, and on its own line rather than folded into the
			// version. A dev version already carries `-dev-` and its build
			// stamp, but that is a string somebody has to read carefully at
			// exactly the moment they are not: they typed a flag, or they did
			// not and the console served it anyway.
			if release.Channel == api.ChannelDev {
				env.Printer.Say("  channel    dev — a build being tried, not a release")
			}

			env.Printer.Say("  from       %s", download.URL)
			env.Printer.Say("  verified against the checksum %s holds for it", client.BaseURL())

			if backwards {
				env.Printer.Warn("This goes backwards: %s is installed, %s is published.", installed, release.Version)
			}

			if dryRun {
				if env.Printer.Structured() {
					return env.Printer.Value(selfUpdateReport(target, platform, installed, release, download, "would-update"))
				}

				return nil
			}

			if backwards && !yes {
				if !env.confirm(fmt.Sprintf("Replace %s with %s?", installed, release.Version)) {
					return fmt.Errorf("cancelled")
				}
			}

			if err := replace(ctx, download, target); err != nil {
				return err
			}

			if env.Printer.Structured() {
				return env.Printer.Value(selfUpdateReport(target, platform, installed, release, download, "updated"))
			}

			env.Printer.Good("Updated to %s.", release.Version)

			return nil
		},
	}
}

// selfUpdateReport is what --format json answers with.
//
// Every key every time, including the ones a particular outcome did not use:
// a key that appears only when it has a value is a key every script has to
// test for. action is what happened, and is the field to branch on.
func selfUpdateReport(path, platform, installed string, release *api.CLIRelease, download api.CLIDownload, action string) map[string]any {
	return map[string]any{"self_update": map[string]any{
		"path":      path,
		"platform":  platform,
		"installed": installed,
		"published": release.Version,
		// Which channel answered, so a script can refuse a dev build without
		// parsing a version string for `-dev-`. Empty against a control plane
		// from before channels existed, and reported as it arrived rather than
		// defaulted to "stable" here: a key this client invented would be one
		// a script could not distinguish from one the platform sent.
		"channel": release.Channel,
		"url":     download.URL,
		"sha256":  download.SHA256,
		"action":  action,
	}}
}

// platformKey is how a release manifest names the build for one machine.
//
// Go's own spelling of both halves, joined the way Go joins them and the way
// the Makefile's PLATFORMS lists them. Not the hyphen the built files use:
// dist/vallic-darwin-arm64 is a filename, and the publish script converts the
// key to it rather than the other way round, so there is one spelling to be
// wrong about and it is the one both languages already write.
//
// The architecture alone would have been enough for the agent, which runs on
// Linux and nothing else. Here it would offer a macOS build to a Linux machine
// whose checksum matched and which could not run it.
func platformKey(goos, goarch string) string {
	return goos + "/" + goarch
}

// runningBinary is the file this process is running, with symlinks resolved.
//
// Asked of the kernel rather than assumed, so a copy installed somewhere
// unusual updates itself where it is instead of writing a second copy at a
// path nothing runs.
//
// Resolved, because the name on $PATH is often a link: people link ~/bin at a
// checkout's dist/, and replacing the link would leave the real binary in
// place and point the link at a version nobody installed.
func runningBinary() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot tell which file this binary is: %w", err)
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		// Not fatal. os.Executable already answered, and a path that cannot be
		// resolved is one the write below will refuse loudly rather than
		// quietly write to the wrong place.
		return path, nil
	}

	return resolved, nil
}

// owner is a tool that installed this binary and expects to replace it.
type owner struct {
	// Tool is what the refusal names.
	Tool string

	// Command is how to update through it, empty wherever naming one would be
	// a guess. A wrong command printed with confidence is worse than none: the
	// person runs it, it fails, and now they are debugging the CLI's advice
	// instead of updating.
	Command string
}

// refusal is the error printed instead of replacing a binary somebody else
// installed.
func (o owner) refusal(path string) error {
	message := fmt.Sprintf("%s was installed by %s, which is what should replace it", path, o.Tool)

	if o.Command != "" {
		message += "\n  update it with: " + o.Command
	}

	// Named, because the check below reads the path and nothing else, and a
	// binary copied here by hand is refused although nothing owns it. One move
	// is the whole remedy, and somebody who knows how their copy got there
	// should not have to guess that.
	message += "\n  if this copy was put there by hand, move it somewhere you own and update it there"

	return errors.New(message)
}

// installedBy reports which tool owns the binary at path, from where it is.
//
// The path is all this reads. It could have asked dpkg, rpm or brew what owns
// the file, and that would mean running them: this binary has no dependency it
// did not compile in, and a CLI whose behaviour changes with what is installed
// beside it is a CLI nobody can predict.
//
// So it is a guess in both directions, and worth being honest about which.
// A binary somebody copied into /usr/bin is refused although no package owns
// it, which costs one move command and is what the refusal names. A package
// manager installing somewhere this does not know is missed, and that case
// falls through to the write, which fails on a directory the user does not own
// and names it. Neither failure installs anything.
func installedBy(path string, getenv func(string) string) *owner {
	clean := filepath.ToSlash(filepath.Clean(path))
	dir := filepath.ToSlash(filepath.Dir(clean))

	switch {
	// Homebrew's bin is links into the Cellar, and this runs after the links
	// are resolved, so the Cellar is what is seen. The prefixes are there for
	// the copies that are not linked, and for a HOMEBREW_PREFIX somewhere
	// else entirely, which is supported and common on shared machines.
	case strings.Contains(clean, "/Cellar/"),
		under(clean, filepath.ToSlash(getenv("HOMEBREW_PREFIX"))),
		under(clean, "/opt/homebrew"),
		under(clean, "/home/linuxbrew/.linuxbrew"):
		return &owner{Tool: "Homebrew"}

	// The store is read-only, so the rename would fail anyway. Named rather
	// than left to fail, because "read-only file system" is a true message
	// that tells a Nix user nothing they can act on.
	case under(clean, "/nix/store"):
		return &owner{Tool: "Nix"}

	case under(clean, "/snap"), under(clean, "/var/lib/snapd"):
		return &owner{Tool: "snap"}

	case goBin(getenv) != "" && dir == goBin(getenv):
		return &owner{
			Tool: "the Go toolchain",
			// Safe to print because it is not a guess: it is the module path
			// in go.mod and the command in the README.
			Command: "go install github.com/vallic/vallic-cli/cmd/vallic@latest",
		}

	// The directories a distribution's packages own. /usr/local/bin is
	// deliberately not among them: under the FHS it is the local
	// administrator's, which is where a curl install and `make install` put
	// things, and refusing it would refuse the most common hand install there
	// is.
	case dir == "/usr/bin", dir == "/bin", dir == "/usr/sbin", dir == "/sbin":
		return &owner{Tool: "this system's package manager"}
	}

	return nil
}

// under reports whether path is prefix or sits inside it.
//
// Segment by segment rather than by string prefix, so /snapshot is not read as
// something inside /snap.
func under(path, prefix string) bool {
	if prefix == "" {
		return false
	}

	prefix = strings.TrimSuffix(prefix, "/")

	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// goBin is where `go install` would have put this binary.
//
// Read from the environment rather than from `go env`, which means running the
// toolchain: see installedBy. GOPATH may be a list, and the toolchain installs
// into the first entry, so that is the one this looks at.
func goBin(getenv func(string) string) string {
	if bin := getenv("GOBIN"); bin != "" {
		return filepath.ToSlash(filepath.Clean(bin))
	}

	if gopath := getenv("GOPATH"); gopath != "" {
		first := strings.Split(gopath, string(os.PathListSeparator))[0]
		if first != "" {
			return filepath.ToSlash(filepath.Join(filepath.Clean(first), "bin"))
		}
	}

	if home := getenv("HOME"); home != "" {
		return filepath.ToSlash(filepath.Join(filepath.Clean(home), "go", "bin"))
	}

	return ""
}

// sameVersion reports whether two version strings name the same release.
//
// Compared without a leading "v", because the two strings come from two places
// that spell it differently and both are right: a release is tagged v0.6.0 and
// the binary is stamped from that tag, while the version pinned on the control
// plane is a field somebody fills in. A client that read those as two
// different versions would download the binary it is already running, on every
// invocation, and never arrive anywhere.
func sameVersion(a, b string) bool {
	return strings.TrimPrefix(strings.TrimSpace(a), "v") == strings.TrimPrefix(strings.TrimSpace(b), "v")
}

// olderThan reports whether version a is behind version b.
//
// comparable is false when either side is not something this can order: the
// "dev" a build from source carries, or a version a release process spells in
// a way this does not know. The caller treats that as "cannot tell" and goes
// ahead, because the alternative is calling an update a downgrade on the
// strength of a string comparison.
func olderThan(a, b string) (older, comparable bool) {
	left, leftOK := versionParts(a)
	right, rightOK := versionParts(b)

	if !leftOK || !rightOK {
		return false, false
	}

	for i := 0; i < len(left) || i < len(right); i++ {
		if part(left, i) != part(right, i) {
			return part(left, i) < part(right, i), true
		}
	}

	return false, true
}

// versionParts reads a dotted numeric version, or says it could not.
func versionParts(version string) ([]int, bool) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(version), "v")

	// Everything from the first suffix is dropped rather than ordered. That
	// makes 0.6.0-rc1 and 0.6.0 compare equal, which semver would not, and
	// which is right for the only question asked here: whether the control
	// plane is pointing this binary at an earlier release. A build tagged
	// 0.6.0-4-gabc123 by `git describe` is not an earlier release than 0.6.0.
	if cut := strings.IndexAny(trimmed, "-+"); cut >= 0 {
		trimmed = trimmed[:cut]
	}

	if trimmed == "" {
		return nil, false
	}

	fields := strings.Split(trimmed, ".")
	parts := make([]int, 0, len(fields))

	for _, field := range fields {
		number, err := strconv.Atoi(field)
		if err != nil || number < 0 {
			return nil, false
		}

		parts = append(parts, number)
	}

	return parts, true
}

// part is one component of a version, or zero past its end, so 1.2 and 1.2.0
// compare equal.
func part(parts []int, i int) int {
	if i < len(parts) {
		return parts[i]
	}

	return 0
}

// replace downloads the published binary and puts it where the running one is.
func replace(ctx context.Context, download api.CLIDownload, target string) error {
	want, err := wantedDigest(download.SHA256)
	if err != nil {
		return err
	}

	if err := httpsOnly(download.URL); err != nil {
		return err
	}

	// Beside the binary it replaces, so the rename below cannot cross a
	// filesystem. A rename that crosses one is a copy and a delete, which is
	// exactly the moment of a half-written binary this exists to avoid, and it
	// is why the download does not go to a temporary directory: on Linux that
	// is usually a different filesystem, and on some machines it is memory.
	//
	// ".new" rather than a random name: an interrupted run leaves one file
	// with an obvious name beside a binary on somebody's PATH, rather than an
	// accumulation of them.
	staged := target + ".new"
	defer os.Remove(staged)

	// 0o700 while it is being written, so nothing else can run a file that has
	// not been checked yet. It gets the mode below once it is known to be the
	// binary the control plane published.
	file, err := os.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o700)
	if err != nil {
		return cannotWrite(target, err)
	}

	err = fetchInto(ctx, file, download.URL, want)
	closeErr := file.Close()

	if err != nil {
		return err
	}

	if closeErr != nil {
		return fmt.Errorf("writing the new binary: %w", closeErr)
	}

	return install(staged, target, modeFor(target))
}

// fetchInto downloads a URL into an open file and refuses anything that is not
// the digest the control plane published.
func fetchInto(ctx context.Context, file io.Writer, address, want string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return fmt.Errorf("requesting the new binary: %w", err)
	}

	// Its own client, not the API's. The API client attaches the credential
	// that reaches somebody's production site to everything it sends, and the
	// bytes are at whatever address a manifest named. Sending a token to a
	// third party because a response asked for it is the one thing an update
	// path must never do.
	//
	// Redirects are followed, which is the default and is correct here: a
	// release asset normally answers with one to storage. Following it is safe
	// because what makes the download trustworthy is the digest at the end,
	// not the hostname at the start.
	client := &http.Client{Timeout: downloadTimeout}

	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", address, err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading the new binary: %s answered %s", address, res.Status)
	}

	return verifiedCopy(file, res.Body, want)
}

// verifiedCopy copies src into dst and fails unless it hashes to want.
//
// The digest is taken from the bytes on their way through rather than by
// reading the file back afterwards, so there is no window in which what is on
// disk is not what was checked.
func verifiedCopy(dst io.Writer, src io.Reader, want string) error {
	digest := sha256.New()

	written, err := io.Copy(io.MultiWriter(dst, digest), io.LimitReader(src, maxBinaryBytes))
	if err != nil {
		// A download cut off halfway lands here or on the digest below, and
		// either way nothing has been moved: the staged file is removed by the
		// caller and the binary in place has not been touched.
		return fmt.Errorf("downloading the new binary: %w", err)
	}

	if written >= maxBinaryBytes {
		return fmt.Errorf("the published binary is larger than %d bytes, so it is not being installed", int64(maxBinaryBytes))
	}

	if got := hex.EncodeToString(digest.Sum(nil)); got != want {
		// Both digests, because a mismatch is either a download that was
		// truncated or a binary that is not the one the control plane
		// published, and afterwards those are worth telling apart.
		return fmt.Errorf("the download is not what the control plane published: wanted %s, got %s", want, got)
	}

	return nil
}

// wantedDigest normalises the control plane's checksum, or refuses it.
//
// Refused rather than compared leniently: a checksum that is not a SHA-256 is
// a manifest this version cannot verify against, and the only safe reading of
// "cannot verify" is to stop. The case and a "sha256:" prefix are tolerated
// because both spellings are written by tools people already use, and neither
// changes what is being compared.
func wantedDigest(checksum string) (string, error) {
	normalised := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(checksum)), "sha256:")

	if len(normalised) != hex.EncodedLen(sha256.Size) {
		return "", fmt.Errorf("the published checksum is not a SHA-256 digest: %q", checksum)
	}

	if _, err := hex.DecodeString(normalised); err != nil {
		return "", fmt.Errorf("the published checksum is not a SHA-256 digest: %q", checksum)
	}

	return normalised, nil
}

// httpsOnly refuses a manifest that names a plaintext download.
//
// Not what makes the update safe: the digest would still catch a substituted
// binary over http. It is a refusal to quietly accept an http URL in a release
// manifest, which is a mistake in the manifest and worth hearing about rather
// than working around.
func httpsOnly(address string) error {
	parsed, err := url.Parse(address)
	if err != nil {
		return fmt.Errorf("the published download address cannot be read: %q", address)
	}

	if parsed.Scheme != "https" {
		return fmt.Errorf("the published download address is not https: %q", address)
	}

	return nil
}

// modeFor is the permission the replacement should carry.
//
// The mode the binary already has, so a copy installed deliberately private
// stays private. 0o755 where the target cannot be read or carries no execute
// bit at all: copying that mode would install a file nobody can run, which is
// a CLI that cannot be started to update itself again.
func modeFor(target string) os.FileMode {
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm()&0o111 == 0 {
		return 0o755
	}

	return info.Mode().Perm()
}

// install puts a verified file at target.
//
// Executable before the rename, not after. Between a rename and a later chmod
// there is a moment where the path is the CLI and cannot be run, and that
// moment is where somebody's next command lands.
func install(staged, target string, mode os.FileMode) error {
	if err := os.Chmod(staged, mode); err != nil {
		return fmt.Errorf("making the new binary executable: %w", err)
	}

	if err := os.Rename(staged, target); err != nil {
		return fmt.Errorf("installing the new binary at %s: %w", target, err)
	}

	return nil
}

// cannotWrite explains a failure to create the staged file beside target.
//
// The directory is named because it is the thing that has to change, and the
// underlying error is kept because it says which of the two usual causes this
// is. A directory owned by somebody else is answered by running this as the
// user that installed the binary; a read-only mount has no answer here at all,
// and says so in its own words.
func cannotWrite(target string, err error) error {
	dir := filepath.Dir(target)

	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf(
			"cannot write to %s: %w\n  this binary is in a directory this user cannot write to; update it as the user that installed it",
			dir, err,
		)
	}

	return fmt.Errorf("cannot write to %s: %w", dir, err)
}

// channelAsked is the channel to request, if any.
//
// Empty for the ordinary case, which asks for nothing and gets whatever the
// installation serves. There is deliberately no `--stable`: a console serving
// dev is a test console, everything pointed at it is being tested, and a client
// opting out of that reports results for a binary nobody meant to look at. See
// CliRelease::resolve() in the control plane, which is where the rule lives.
func channelAsked(dev bool) string {
	if dev {
		return api.ChannelDev
	}

	return ""
}
