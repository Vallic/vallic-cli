package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkoutWith builds a directory holding the files a detection reads.
func checkoutWith(t *testing.T, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()

	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

// The order is the behaviour, not an implementation detail. A Drupal site
// carries a package.json for its theme and a Go service may carry one for a
// front end, so a detection that looked at package.json first would write
// `type: nodejs` into a repository of PHP and the person would find out from
// the image it deployed on.
func TestDetectProjectReadsTheFilesInTheCheckout(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name:  "drupal, as most sites require it",
			files: map[string]string{"composer.json": `{"require":{"drupal/core-recommended":"^11"}}`},
			want:  typeDrupal,
		},
		{
			name:  "drupal, as a module requires it",
			files: map[string]string{"composer.json": `{"require-dev":{"drupal/core-dev":"^11"}}`},
			want:  typeDrupal,
		},
		{
			name:  "php with a framework this does not know",
			files: map[string]string{"composer.json": `{"require":{"symfony/framework-bundle":"^7"}}`},
			want:  typePHP,
		},
		{
			name: "a drupal site with a theme to build",
			files: map[string]string{
				"composer.json": `{"require":{"drupal/core":"^11"}}`,
				"package.json":  `{"scripts":{"build":"vite build"}}`,
			},
			want: typeDrupal,
		},
		{
			name: "a go service with a front end beside it",
			files: map[string]string{
				"go.mod":       "module example.com/api\n\ngo 1.27\n",
				"package.json": `{}`,
			},
			want: typeGo,
		},
		{
			name:  "node on its own",
			files: map[string]string{"package.json": `{}`},
			want:  typeNode,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, because, err := detectProject(checkoutWith(t, c.files))
			if err != nil {
				t.Fatalf("detectProject() = %v", err)
			}

			if got != c.want {
				t.Errorf("detectProject() = %q, want %q", got, c.want)
			}

			// The file that decided it is printed, so somebody can disagree
			// with the guess rather than wonder where it came from.
			if because == "" {
				t.Error("detectProject() gave no reason, and the reason is what makes it arguable")
			}
		})
	}
}

// A directory that says nothing has to be a usage error: the fix is --type,
// which is exit code 2 and the help that lists what it takes. Writing a
// manifest for a guessed type would be a file somebody deploys without
// reading.
func TestDetectProjectRefusesADirectoryThatSaysNothing(t *testing.T) {
	_, _, err := detectProject(checkoutWith(t, map[string]string{"README.md": "hello"}))
	if err == nil {
		t.Fatal("detectProject() = nil error, want a refusal")
	}

	var usage *UsageError
	if !errors.As(err, &usage) {
		t.Errorf("error = %T, want a *UsageError so the exit code is 2", err)
	}

	if !strings.Contains(err.Error(), "--type") {
		t.Errorf("error = %q, want it to name the flag that settles it", err)
	}
}

// The pin comes from the repository or it does not happen. A version invented
// here would pin every site started with this release of the CLI to whatever
// was current when the binary was built, and the file would look like the
// repository had asked for it.
func TestStarterPinsOnlyWhatTheRepositoryStates(t *testing.T) {
	cases := []struct {
		name      string
		kind      string
		files     map[string]string
		key       string
		want      string
		wantsNote bool
	}{
		{
			name:  "php, to the minor the images are tagged with",
			kind:  typeDrupal,
			files: map[string]string{"composer.json": `{"require":{"php":"^8.3","drupal/core":"^11"}}`},
			key:   "php",
			want:  "8.3",
		},
		{
			name:  "node, to the major and no further",
			kind:  typeNode,
			files: map[string]string{"package.json": `{"engines":{"node":">=22.4.0"}}`},
			key:   "node",
			want:  "22",
		},
		{
			name:  "go, from the directive and not the toolchain",
			kind:  typeGo,
			files: map[string]string{"go.mod": "module example.com/api\n\ngo 1.27.0\n\ntoolchain go1.29.1\n"},
			key:   "go",
			want:  "1.27",
		},
		{
			name:      "nothing at all, which is said out loud",
			kind:      typePHP,
			files:     map[string]string{"composer.json": `{"require":{"monolog/monolog":"^3"}}`},
			key:       "php",
			want:      "",
			wantsNote: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := starterFor(c.kind, checkoutWith(t, c.files))

			if got.VersionKey != c.key {
				t.Errorf("VersionKey = %q, want %q, which is the key the control plane reads", got.VersionKey, c.key)
			}

			if got.Version != c.want {
				t.Errorf("Version = %q, want %q", got.Version, c.want)
			}

			if c.wantsNote && len(got.Notes) == 0 {
				t.Error("no note; an unpinned runtime is the platform's default, not a neutral state")
			}
		})
	}
}

// The catalogue matches a manifest on the prefix of an image tag, so how much
// of a constraint to keep is a fact about the tags: `8.4-4.70.12-3` for PHP,
// `24.20-1.76.0-1` for Node. Keeping a minor Node never ships matches no tag,
// and the deploy silently runs the default instead.
func TestFirstVersionKeepsOnlyWhatMatchesATag(t *testing.T) {
	cases := []struct {
		constraint string
		components int
		want       string
	}{
		{"^8.3", 2, "8.3"},
		{">=8.2 <9", 2, "8.2"},
		{"~8.2.0", 2, "8.2"},
		{"8", 2, "8"},
		{">=22.4.0", 1, "22"},
		{"22.x", 1, "22"},
		{"1.27.0", 2, "1.27"},
		{"", 2, ""},
		{"*", 2, ""},
	}

	for _, c := range cases {
		if got := firstVersion(c.constraint, c.components); got != c.want {
			t.Errorf("firstVersion(%q, %d) = %q, want %q", c.constraint, c.components, got, c.want)
		}
	}
}

// Every key the parser understands, from AppManifestParser::KEYS. A key
// outside this list is not ignored on the control plane: it is reported as
// "not something vallic.yaml understands", so a file this CLI wrote would
// fail the validate it tells people to run.
var manifestKeys = map[string]bool{
	"version": true, "type": true, "runtime": true, "services": true,
	"cron": true, "workers": true, "build": true, "deploy": true,
	"health": true, "env": true, "mounts": true, "start": true, "port": true,
}

func TestStarterUsesOnlyKeysTheControlPlaneUnderstands(t *testing.T) {
	for _, kind := range []string{typeDrupal, typePHP, typeNode, typeGo} {
		t.Run(kind, func(t *testing.T) {
			rendered := renderStarter(starterFor(kind, checkoutWith(t, map[string]string{
				"composer.json": `{"require":{"php":"^8.4","drupal/core":"^11"}}`,
				"package.json":  `{"engines":{"node":"24"},"scripts":{"build":"next build","start":"next start"}}`,
				"go.mod":        "module example.com/api\n\ngo 1.27\n",
			})))

			for _, line := range strings.Split(rendered, "\n") {
				name, _, isKey := strings.Cut(line, ":")

				if !isKey || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "#") || line == "" {
					continue
				}

				if !manifestKeys[name] {
					t.Errorf("top-level key %q, which the parser reports as one it does not understand", name)
				}
			}

			// The commands belong under `steps`, and a list written straight
			// under `build:` is refused by name. It used to be accepted and
			// ignored, which deployed a site with no vendor directory.
			if !strings.Contains(rendered, "build:\n  steps:\n") {
				t.Error("build has no steps block, which is the shape the parser refuses when it is missing")
			}

			if !strings.HasPrefix(rendered, "#") {
				t.Error("no first line saying what wrote the file and what to run next")
			}
		})
	}
}

// Commented out, every time. A declared service the environment does not run
// refuses the deploy, and for a database the refusal is "contact support,
// this is a migration", which is a hard stop for a file nobody chose to
// write, in exchange for a guess.
func TestStarterDeclaresNoServiceItCannotKnowAbout(t *testing.T) {
	rendered := renderStarter(starterFor(typeDrupal, checkoutWith(t, map[string]string{
		"composer.json": `{"require":{"drupal/core":"^11"}}`,
	})))

	for _, line := range strings.Split(rendered, "\n") {
		if strings.HasPrefix(line, "services:") {
			t.Fatal("services are declared; the file may only suggest them, in a comment")
		}
	}

	// The suggestion is still there to uncomment, with a version on it,
	// because a service without one is a problem the parser reports.
	if !strings.Contains(rendered, "#   - mariadb: '11.8'") {
		t.Error("no commented service to start from")
	}
}

// The file somebody has edited is the one worth keeping. Overwriting it is
// not something they recover from by running the command again, so it takes
// --force and says so.
func TestWriteManifestWillNotReplaceWhatIsThere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vallic.yaml")

	if err := writeManifest(path, "version: 1\n", false); err != nil {
		t.Fatalf("writeManifest() into an empty directory = %v", err)
	}

	err := writeManifest(path, "version: 1\ntype: drupal\n", false)
	if err == nil {
		t.Fatal("writeManifest() over an existing file = nil error, want a refusal")
	}

	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error = %q, want it to name the flag that means it", err)
	}

	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}

	if string(raw) != "version: 1\n" {
		t.Errorf("the file is now %q, want the refusal to have left it alone", raw)
	}

	if err := writeManifest(path, "version: 1\ntype: drupal\n", true); err != nil {
		t.Fatalf("writeManifest() with --force = %v", err)
	}

	raw, readErr = os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}

	// Truncated rather than written over, or the tail of the longer file
	// would still be there under the new one.
	if string(raw) != "version: 1\ntype: drupal\n" {
		t.Errorf("the file is now %q, want exactly what was written", raw)
	}
}

// A framework is also a PHP application, so detection has to try the specific
// ones first. Getting this wrong is silent and expensive: the kind decides
// which directories survive a deploy, so a Laravel site written as plain PHP
// loses every upload at the next release, because the platform is looking for
// Drupal's paths and finding nothing.
func TestFrameworksAreDetectedBeforePlainPhp(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		want    string
		because string
	}{
		{
			name:    "laravel by its framework package",
			files:   map[string]string{"composer.json": `{"require":{"laravel/framework":"^11.0"}}`},
			want:    typeLaravel,
			because: "laravel/framework",
		},
		{
			name:    "wordpress the composer-managed way",
			files:   map[string]string{"composer.json": `{"require":{"johnpbloch/wordpress-core":"^6.5"}}`},
			want:    typeWordPress,
			because: "johnpbloch/wordpress-core",
		},
		{
			// WordPress has no single canonical core package, so both
			// spellings have to be known or a Bedrock checkout reads as PHP.
			name:    "wordpress the bedrock way",
			files:   map[string]string{"composer.json": `{"require":{"roots/wordpress":"^6.5"}}`},
			want:    typeWordPress,
			because: "roots/wordpress",
		},
		{
			// The common shape: core, plugins and themes committed whole,
			// with no composer.json anywhere.
			name:    "wordpress with nothing but wp-config.php",
			files:   map[string]string{"wp-config.php": "<?php"},
			want:    typeWordPress,
			because: "wp-config.php",
		},
		{
			name:    "drupal still wins over a bare composer.json",
			files:   map[string]string{"composer.json": `{"require":{"drupal/core-recommended":"^11"}}`},
			want:    typeDrupal,
			because: "drupal/core",
		},
		{
			name:    "and a composer.json naming no framework is still php",
			files:   map[string]string{"composer.json": `{"require":{"monolog/monolog":"^3"}}`},
			want:    typePHP,
			because: "no framework",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()

			for name, body := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			kind, because, err := detectProject(dir)
			if err != nil {
				t.Fatalf("detectProject() = %v", err)
			}

			if kind != tc.want {
				t.Errorf("kind = %q, want %q", kind, tc.want)
			}

			// The reason is printed so somebody can disagree with the guess,
			// which is only useful if it names the file that decided.
			if !strings.Contains(because, tc.because) {
				t.Errorf("because = %q, want it to mention %q", because, tc.because)
			}
		})
	}
}

// WordPress is the one kind that commonly has no composer.json, and
// `composer install` with no composer.json fails the build at the point where
// there is nothing to roll back to.
func TestAWordpressCheckoutWithNoComposerGetsNoBuildStep(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "wp-config.php"), []byte("<?php"), 0o644); err != nil {
		t.Fatal(err)
	}

	bare := starterFor(typeWordPress, dir)

	if len(bare.Build) != 0 {
		t.Errorf("Build = %v, want none without a composer.json", bare.Build)
	}

	// And with one, the build comes back: the dependencies are not committed.
	managed := t.TempDir()

	if err := os.WriteFile(filepath.Join(managed, "composer.json"), []byte(`{"require":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if len(starterFor(typeWordPress, managed).Build) == 0 {
		t.Error("Build = none, want composer install where there is a composer.json")
	}
}

// WordPress upgrades its own schema on the first request after a version
// change, so a deploy step would run before that is true and report success
// having done nothing. Laravel's migration is the opposite: nothing else runs
// it, and it needs --force because there is no terminal to confirm at.
func TestDeployStepsMatchWhatEachFrameworkNeeds(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "composer.json"), []byte(`{"require":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := starterFor(typeWordPress, dir).Deploy; len(got) != 0 {
		t.Errorf("wordpress Deploy = %v, want none", got)
	}

	laravel := starterFor(typeLaravel, dir).Deploy

	if len(laravel) != 1 || !strings.Contains(laravel[0], "migrate") {
		t.Errorf("laravel Deploy = %v, want a migration", laravel)
	}

	if !strings.Contains(laravel[0], "--force") {
		t.Errorf("laravel Deploy = %v, want --force: a prompt nobody can answer hangs the deploy", laravel)
	}
}
