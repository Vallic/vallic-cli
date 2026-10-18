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
		path := filepath.Join(dir, filepath.FromSlash(name))

		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
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
		{
			name: "a django site with a front end to build",
			files: map[string]string{
				"manage.py":    "import os\n",
				"package.json": `{"scripts":{"build":"vite build"}}`,
			},
			want: typeDjango,
		},
		{
			name:  "django by its requirement, with manage.py elsewhere",
			files: map[string]string{"requirements.txt": "Django[argon2]>=5.2,<6\npsycopg[binary]\n"},
			want:  typeDjango,
		},
		{
			name:  "django by pyproject.toml's dependencies",
			files: map[string]string{"pyproject.toml": "[project]\nname = \"shop\"\ndependencies = [\n  \"django>=5.2\",\n]\n"},
			want:  typeDjango,
		},
		{
			name:  "python, whose packages only sound like django",
			files: map[string]string{"pyproject.toml": "[project]\ndependencies = [\"flask>=3\", \"django-environ\"]\n# not django\n"},
			want:  typePython,
		},
		{
			name:  "python by requirements.txt",
			files: map[string]string{"requirements.txt": "fastapi\nuvicorn\n"},
			want:  typePython,
		},
		{
			name: "a go service with its documentation built in python",
			files: map[string]string{
				"go.mod":           "module example.com/api\n\ngo 1.27\n",
				"requirements.txt": "mkdocs\n",
			},
			want: typeGo,
		},
		{
			name: "a rust service with a front end beside it",
			files: map[string]string{
				"Cargo.toml":   "[package]\nname = \"server\"\n",
				"package.json": `{"scripts":{"build":"vite build"}}`,
			},
			want: typeRust,
		},
		{
			name: "a rails application bundling its javascript",
			files: map[string]string{
				"Gemfile":      "gem \"rails\", \"~> 8.1\"\n",
				"Gemfile.lock": "GEM\n  specs:\n    rails (8.1.0)\n",
				"package.json": `{"scripts":{"build":"esbuild app/javascript/*.*"}}`,
			},
			want: typeRuby,
		},
		{
			name:  "a rack application, which is ruby too",
			files: map[string]string{"Gemfile": "source \"https://rubygems.org\"\ngem \"sinatra\"\n"},
			want:  typeRuby,
		},
		{
			// Bun is a runtime of the nodejs type, not a type of its own.
			name:  "node on bun",
			files: map[string]string{"package.json": `{}`, "bun.lock": "{}"},
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
			name: "python, from .python-version before requires-python",
			kind: typePython,
			files: map[string]string{
				".python-version": "3.13.2\n",
				"pyproject.toml":  "[project]\nrequires-python = \">=3.12\"\n",
			},
			key:  "python",
			want: "3.13",
		},
		{
			name:  "python, from the floor requires-python sets",
			kind:  typeDjango,
			files: map[string]string{"pyproject.toml": "[project]\nrequires-python = \">=3.12,<4\"\n"},
			key:   "python",
			want:  "3.12",
		},
		{
			name:      "python with nothing to say, which is said out loud",
			kind:      typePython,
			files:     map[string]string{"requirements.txt": "flask\n"},
			key:       "python",
			want:      "",
			wantsNote: true,
		},
		{
			name: "rust, from the toolchain the repository builds with",
			kind: typeRust,
			files: map[string]string{
				"Cargo.toml":          "[package]\nname = \"server\"\nrust-version = \"1.80\"\n",
				"rust-toolchain.toml": "[toolchain]\nchannel = \"1.98.1\"\ncomponents = [\"clippy\"]\n",
			},
			key:  "rust",
			want: "1.98",
		},
		{
			// rust-version is the oldest the crate supports, which the
			// platform does not offer: pinning it would run the default
			// while the file said otherwise.
			name:      "rust on a channel, which names no version",
			kind:      typeRust,
			files:     map[string]string{"Cargo.toml": "[package]\nname = \"server\"\nrust-version = \"1.80\"\n", "rust-toolchain.toml": "[toolchain]\nchannel = \"stable\"\n"},
			key:       "rust",
			want:      "",
			wantsNote: true,
		},
		{
			name:  "ruby, from .ruby-version as rbenv writes it",
			kind:  typeRuby,
			files: map[string]string{".ruby-version": "ruby-3.4.2\n", "Gemfile": "ruby \"3.3.0\"\n"},
			key:   "ruby",
			want:  "3.4",
		},
		{
			name:  "ruby, from the version the lock was resolved with",
			kind:  typeRuby,
			files: map[string]string{"Gemfile": "gem \"puma\"\n", "Gemfile.lock": "GEM\n  specs:\n\nRUBY VERSION\n   ruby 4.0.7p0\n"},
			key:   "ruby",
			want:  "4.0",
		},
		{
			name:  "bun, from engines, under its own key",
			kind:  typeNode,
			files: map[string]string{"package.json": `{"engines":{"bun":">=1.4.3"}}`, "bun.lock": "{}"},
			key:   "bun",
			want:  "1.4",
		},
		{
			name:  "bun, from the version corepack pins",
			kind:  typeNode,
			files: map[string]string{"package.json": `{"packageManager":"bun@1.3.14+sha512.0123"}`, "bun.lockb": ""},
			key:   "bun",
			want:  "1.3",
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
	"extra": true, "backup": true,
}

func TestStarterUsesOnlyKeysTheControlPlaneUnderstands(t *testing.T) {
	for _, kind := range []string{typeDrupal, typePHP, typeNode, typeGo, typeRust, typePython, typeDjango, typeRuby} {
		t.Run(kind, func(t *testing.T) {
			rendered := renderStarter(starterFor(kind, checkoutWith(t, map[string]string{
				"composer.json":      `{"require":{"php":"^8.4","drupal/core":"^11"}}`,
				"package.json":       `{"engines":{"node":"24"},"scripts":{"build":"next build","start":"next start"}}`,
				"go.mod":             "module example.com/api\n\ngo 1.27\n",
				"Cargo.toml":         "[package]\nname = \"server\"\n",
				"Cargo.lock":         "version = 4\n",
				"src/main.rs":        "fn main() {}\n",
				"pyproject.toml":     "[project]\nrequires-python = \">=3.14\"\n",
				"uv.lock":            "version = 1\n",
				"manage.py":          `os.environ.setdefault("DJANGO_SETTINGS_MODULE", "mysite.settings")`,
				"mysite/wsgi.py":     "",
				"mysite/settings.py": "STATIC_ROOT = BASE_DIR / 'staticfiles'\n",
				"Gemfile":            "gem \"rails\"\ngem \"propshaft\"\ngem \"puma\"\n",
				"Gemfile.lock":       "PLATFORMS\n  x86_64-linux\n\nRUBY VERSION\n   ruby 4.0.7p0\n",
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

// The build is whatever the repository locks with. uv with its lock installs
// exactly what was tested; requirements.txt is pip's file, and uv given it
// would want a pyproject.toml the project does not have; a pyproject.toml
// with no lock still builds, but resolves afresh every time, which is worth
// saying.
func TestPythonBuildsWithWhatTheRepositoryLocks(t *testing.T) {
	cases := []struct {
		name      string
		files     map[string]string
		want      []string
		wantsNote string
	}{
		{
			name:  "uv, held to its lock",
			files: map[string]string{"pyproject.toml": "[project]\n", "uv.lock": "version = 1\n", "requirements.txt": "flask\n"},
			want:  []string{"uv sync --locked --no-dev --no-editable"},
		},
		{
			name:  "pip, into the release's own environment",
			files: map[string]string{"requirements.txt": "flask\n"},
			want:  []string{"python -m venv .venv", ".venv/bin/pip install -r requirements.txt"},
		},
		{
			name:      "uv with nothing locked",
			files:     map[string]string{"pyproject.toml": "[project]\n"},
			want:      []string{"uv sync --no-dev --no-editable"},
			wantsNote: "uv lock",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := starterFor(typePython, checkoutWith(t, c.files))

			if strings.Join(got.Build, " | ") != strings.Join(c.want, " | ") {
				t.Errorf("Build = %q, want %q", got.Build, c.want)
			}

			if c.wantsNote != "" && !strings.Contains(strings.Join(got.Notes, "\n"), c.wantsNote) {
				t.Errorf("Notes = %q, want one mentioning %q", got.Notes, c.wantsNote)
			}

			// No start line for a project that is not Django: the module and
			// the variable are the application's own, and a guess names one
			// that is not there. Said instead.
			if got.Start != "" {
				t.Errorf("Start = %q, want none written for a plain Python project", got.Start)
			}

			if !strings.Contains(strings.Join(got.Notes, "\n"), "start:") {
				t.Errorf("Notes = %q, want one asking for a start: line", got.Notes)
			}
		})
	}
}

// Django's starter does what the framework needs and nothing it cannot know:
// static files collected in the build, migrate on every deploy — the first
// one too, which is what creates the tables — and Gunicorn on the WSGI module
// manage.py names.
func TestDjangoStarterServesTheProjectManagePyNames(t *testing.T) {
	dir := checkoutWith(t, map[string]string{
		"manage.py":                     "os.environ.setdefault('DJANGO_SETTINGS_MODULE', 'config.settings.production')\n",
		"config/wsgi.py":                "",
		"config/settings/production.py": "DEBUG = False\n",
		"requirements.txt":              "Django>=5.2\n",
	})

	got := starterFor(typeDjango, dir)

	if got.Start != "python -m gunicorn config.wsgi" {
		t.Errorf("Start = %q, want Gunicorn on config.wsgi", got.Start)
	}

	if last := got.Build[len(got.Build)-1]; last != "python manage.py collectstatic --noinput" {
		t.Errorf("last build step = %q, want collectstatic after the dependencies", last)
	}

	if len(got.Deploy) != 1 || !strings.Contains(got.Deploy[0], "manage.py migrate --noinput") {
		t.Errorf("Deploy = %v, want migrate, with no prompt to hang on", got.Deploy)
	}

	// The settings have no STATIC_ROOT, so the step the file writes would
	// fail the first build. Said, with the file that needs it.
	if !strings.Contains(strings.Join(got.Notes, "\n"), "STATIC_ROOT in config/settings/production.py") {
		t.Errorf("Notes = %q, want one naming the settings file STATIC_ROOT is missing from", got.Notes)
	}

	// With no manage.py, the one directory holding wsgi.py and settings.py
	// is the project; with two, the answer is the project's to give.
	found, _ := djangoProject(checkoutWith(t, map[string]string{"shop/wsgi.py": "", "shop/settings.py": ""}))
	if found != "shop" {
		t.Errorf("djangoProject() = %q, want the one package holding both files", found)
	}

	ambiguous, _ := djangoProject(checkoutWith(t, map[string]string{
		"shop/wsgi.py": "", "shop/settings.py": "",
		"blog/wsgi.py": "", "blog/settings.py": "",
	}))
	if ambiguous != "" {
		t.Errorf("djangoProject() = %q from two candidates, want none picked", ambiguous)
	}
}

// The image starts bin/server and cargo names the binary after its package,
// so a package called anything else serves nothing unless the file says what
// to start. Several binaries, or none at the root, are the repository's to
// choose between.
func TestRustStarterStartsTheBinaryCargoInstalls(t *testing.T) {
	cases := []struct {
		name      string
		files     map[string]string
		start     string
		wantsNote string
	}{
		{
			name:  "the package's own binary",
			files: map[string]string{"Cargo.toml": "[package]\nname = \"shop-api\"\n", "src/main.rs": "", "Cargo.lock": ""},
			start: "bin/shop-api",
		},
		{
			name:  "a [[bin]] named for itself",
			files: map[string]string{"Cargo.toml": "[package]\nname = \"shop\"\n\n[[bin]] # the only one\nname = \"server\"\npath = \"src/server.rs\"\n", "Cargo.lock": ""},
			start: "bin/server",
		},
		{
			name: "several, which are the repository's to choose between",
			files: map[string]string{
				"Cargo.toml":             "[package]\nname = \"shop\"\n",
				"src/main.rs":            "",
				"src/bin/migrate.rs":     "",
				"src/bin/worker/main.rs": "",
				"Cargo.lock":             "",
			},
			wantsNote: "shop, migrate, worker",
		},
		{
			name:      "a workspace, with nothing to build at the root",
			files:     map[string]string{"Cargo.toml": "[workspace]\nmembers = [\"api\", \"core\"]\n", "Cargo.lock": ""},
			wantsNote: "builds no binary",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := starterFor(typeRust, checkoutWith(t, c.files))

			if got.Start != c.start {
				t.Errorf("Start = %q, want %q", got.Start, c.start)
			}

			if c.wantsNote != "" && !strings.Contains(strings.Join(got.Notes, "\n"), c.wantsNote) {
				t.Errorf("Notes = %q, want one mentioning %q", got.Notes, c.wantsNote)
			}

			// target/ under the cache would be copied into every release,
			// and cargo's own caches are kept by the platform.
			if len(got.Cache) != 0 {
				t.Errorf("Cache = %v, want nothing for Rust", got.Cache)
			}
		})
	}

	// --locked with no Cargo.lock has nothing to hold the build to.
	unlocked := starterFor(typeRust, checkoutWith(t, map[string]string{"Cargo.toml": "[package]\nname = \"server\"\n", "src/main.rs": ""}))

	if strings.Contains(strings.Join(unlocked.Build, " "), "--locked") {
		t.Errorf("Build = %v with no Cargo.lock, want no --locked", unlocked.Build)
	}

	if !strings.Contains(strings.Join(unlocked.Notes, "\n"), "Cargo.lock") {
		t.Errorf("Notes = %q, want one asking for Cargo.lock to be committed", unlocked.Notes)
	}
}

// Rails's build and deploy, and only what the bundle supports: an API
// application has no assets:precompile to run, and db:prepare is what makes
// the schema on the empty database of a first release.
func TestRailsStarterCompilesAssetsAndPreparesTheDatabase(t *testing.T) {
	lock := "GEM\n  remote: https://rubygems.org/\n  specs:\n    propshaft (1.2.0)\n    puma (6.6.0)\n    rails (8.1.0)\n      railties (= 8.1.0)\n\nPLATFORMS\n  arm64-darwin-24\n  x86_64-linux\n\nDEPENDENCIES\n  propshaft\n  puma\n  rails (~> 8.1)\n"

	got := starterFor(typeRuby, checkoutWith(t, map[string]string{
		"Gemfile":       "gem \"rails\", \"~> 8.1\"\ngem \"propshaft\"\ngem \"puma\"\n",
		"Gemfile.lock":  lock,
		"config.ru":     "",
		".ruby-version": "4.0.7\n",
	}))

	if want := []string{"bundle install", "SECRET_KEY_BASE_DUMMY=1 bin/rails assets:precompile"}; strings.Join(got.Build, " | ") != strings.Join(want, " | ") {
		t.Errorf("Build = %q, want %q", got.Build, want)
	}

	if len(got.Deploy) != 1 || !strings.Contains(got.Deploy[0], "bin/rails db:prepare") {
		t.Errorf("Deploy = %v, want db:prepare", got.Deploy)
	}

	if strings.Join(got.Cache, " ") != "vendor/bundle" {
		t.Errorf("Cache = %v, want the bundle the release carries", got.Cache)
	}

	// Puma serves from the release with no start: line written.
	if got.Start != "" {
		t.Errorf("Start = %q, want none: the image runs Puma", got.Start)
	}

	// Locked for Linux as well as the Mac, so nothing to say about it.
	if len(got.Notes) != 0 {
		t.Errorf("Notes = %q, want none for a Rails application that has everything", got.Notes)
	}

	api := starterFor(typeRuby, checkoutWith(t, map[string]string{
		"Gemfile":      "gem \"rails\"\ngem \"puma\"\n",
		"Gemfile.lock": "GEM\n  specs:\n    puma (6.6.0)\n    rails (8.1.0)\n\nPLATFORMS\n  arm64-darwin-24\n",
	}))

	if strings.Contains(strings.Join(api.Build, " "), "assets:precompile") {
		t.Errorf("Build = %v, want no assets step without Propshaft or Sprockets", api.Build)
	}

	notes := strings.Join(api.Notes, "\n")

	for _, want := range []string{"no asset pipeline", "bundle lock --add-platform x86_64-linux", "arm64-darwin-24"} {
		if !strings.Contains(notes, want) {
			t.Errorf("Notes = %q, want one mentioning %q", api.Notes, want)
		}
	}
}

// A Rack application that is not Rails gets the bundle and nothing else, and
// is told what Puma needs to serve it.
func TestRackStarterSaysWhatPumaNeeds(t *testing.T) {
	got := starterFor(typeRuby, checkoutWith(t, map[string]string{"Gemfile": "gem 'sinatra'\n"}))

	if strings.Join(got.Build, " | ") != "bundle install" || len(got.Deploy) != 0 {
		t.Errorf("Build = %q, Deploy = %q, want the bundle and no deploy step", got.Build, got.Deploy)
	}

	notes := strings.Join(got.Notes, "\n")

	for _, want := range []string{"Gemfile.lock", "Puma", "config.ru"} {
		if !strings.Contains(notes, want) {
			t.Errorf("Notes = %q, want one mentioning %q", got.Notes, want)
		}
	}
}

// Bun is chosen by its key under runtime, so the key is written even where
// nothing states a version: without it the control plane builds on Node.
func TestBunStarterChoosesBunUnderRuntime(t *testing.T) {
	got := starterFor(typeNode, checkoutWith(t, map[string]string{
		"package.json": `{"scripts":{"build":"bun build ./index.ts --outdir dist","start":"bun dist/index.js"}}`,
		"bun.lock":     "{}",
	}))

	if got.Type != typeNode || got.VersionKey != "bun" {
		t.Errorf("Type = %q, VersionKey = %q, want nodejs on bun", got.Type, got.VersionKey)
	}

	if got.Version != "1" || !strings.Contains(strings.Join(got.Notes, "\n"), "Bun 1") {
		t.Errorf("Version = %q, Notes = %q, want Bun's major and a note saying why", got.Version, got.Notes)
	}

	want := []string{
		"bun install --frozen-lockfile",
		"bun run build",
		"rm -rf node_modules && bun install --frozen-lockfile --production",
	}

	if strings.Join(got.Build, " | ") != strings.Join(want, " | ") {
		t.Errorf("Build = %q, want %q", got.Build, want)
	}

	if !strings.Contains(renderStarter(got), "runtime:\n  bun: '1'\n") {
		t.Errorf("rendered without the key that chooses Bun:\n%s", renderStarter(got))
	}
}

// node_modules travels in the release, so a Node build ends by leaving only
// what production needs — the step the control plane's own suggestion ends on.
func TestNodeStarterLeavesOnlyProductionDependencies(t *testing.T) {
	got := starterFor(typeNode, checkoutWith(t, map[string]string{
		"package.json": `{"scripts":{"build":"next build","start":"next start"}}`,
	}))

	want := []string{"npm ci", "npm run build", "npm prune --omit=dev"}

	if strings.Join(got.Build, " | ") != strings.Join(want, " | ") {
		t.Errorf("Build = %q, want %q", got.Build, want)
	}
}

// The caches are the control plane's ManifestSuggestion::cachePaths(), and
// they are written under the build they belong to.
func TestStarterCachesWhatTheBuildInstalls(t *testing.T) {
	cases := []struct {
		kind  string
		files map[string]string
		want  []string
	}{
		{typeDrupal, map[string]string{"composer.json": `{"require":{"drupal/core":"^11"}}`}, []string{"vendor", "web/core", "web/modules/contrib", "web/themes/contrib", "web/profiles/contrib", "web/libraries"}},
		{typeLaravel, map[string]string{"composer.json": `{}`}, []string{"vendor"}},
		{typeWordPress, map[string]string{"wp-config.php": "<?php"}, nil},
		{typeNode, map[string]string{"package.json": `{}`}, []string{"node_modules"}},
		{typePython, map[string]string{"requirements.txt": "flask\n"}, []string{".venv"}},
		{typeRuby, map[string]string{"Gemfile": "gem \"puma\"\n"}, []string{"vendor/bundle"}},
	}

	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			got := starterFor(c.kind, checkoutWith(t, c.files))

			if strings.Join(got.Cache, " ") != strings.Join(c.want, " ") {
				t.Errorf("Cache = %v, want %v", got.Cache, c.want)
			}

			rendered := renderStarter(got)

			if len(c.want) > 0 && !strings.Contains(rendered, "  cache:\n    - "+c.want[0]+"\n") {
				t.Errorf("no cache under the build:\n%s", rendered)
			}

			if len(c.want) == 0 && strings.Contains(rendered, "cache:") {
				t.Errorf("a cache with no build to keep it for:\n%s", rendered)
			}
		})
	}
}
