package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// The schema version a file written today declares. AppManifest::VERSION on
// the control plane, which refuses a number it does not know rather than
// reading the file leniently.
const manifestSchemaVersion = 1

// The kinds of project this writes a file for, spelled as the control plane's
// own ProjectType values.
const (
	typeDrupal    = "drupal"
	typeLaravel   = "laravel"
	typeWordPress = "wordpress"
	typePHP       = "php"
	typeNode      = "nodejs"
	typeGo        = "go"
	typePython    = "python"
	typeDjango    = "django"
	typeRuby      = "ruby"
	typeRust      = "rust"
)

// initCommand writes a starting vallic.yaml.
//
// The one command in the tree that holds no credential and makes no request,
// which is not an oversight: the checkout it is for may be of a project
// nobody has created on the platform yet, and a command that asked for a
// credential first would be unusable exactly where it is wanted.
//
// What it writes is deliberately shorter than the platform's worked examples.
// Every line in it is either something the checkout stated about itself or a
// command that comes with the framework. Services in particular are left
// commented out rather than guessed at, because a manifest naming a database
// the environment does not run does not warn, it refuses the deploy with a
// message about migrations.
func initCommand() *Command {
	var (
		kind  string
		force bool
	)

	return &Command{
		Name:    "init",
		Summary: "write a starting vallic.yaml for this checkout",
		Usage:   "init [--type drupal|laravel|wordpress|php|nodejs|go|rust|python|django|ruby] [--force]",
		Long: `Writes vallic.yaml in the working directory, and nothing else. It
needs no credential and reaches nothing, so it works in a checkout of a
project that does not exist on the platform yet.

What kind of project this is comes from the files here: a composer.json
requiring drupal/core is Drupal, laravel/framework is Laravel, a WordPress
core package or a wp-config.php is WordPress, a composer.json on its own is
PHP, go.mod is Go, Cargo.toml is Rust, manage.py or a dependency on Django is
Django, a uv.lock, pyproject.toml or requirements.txt is otherwise Python, a
Gemfile is Ruby, and package.json is Node — run on Bun where a bun.lock sits
beside it. The command says which file decided it, and --type overrides the
answer.

The kind is worth getting right rather than settling for the generic one: it
decides the cron command the platform runs, which directories survive a
deploy, and which directory is served. A Laravel site written as plain PHP
keeps none of its uploads, because the platform would be looking for Drupal's
paths and finding nothing.

The language version is pinned to the one the repository already states, and
left out where it states none, because a version invented here would pin a
site to whatever was current when this binary was built.

Services are written commented out. What an environment runs is not something
this can know without asking it, and declaring a database it does not run is a
deploy refused rather than a warning. ` + "`vallic service list`" + ` prints the
versions to fill in.

It will not replace a vallic.yaml that is already here. Once it is written:

    vallic validate --local`,
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&kind, "type", "", "the kind of project, when the files here do not say")
			fs.BoolVar(&force, "force", false, "replace a vallic.yaml that is already here")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			if err := ensureNoExtra(nil, args, 0); err != nil {
				return err
			}

			dir, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("cannot tell which directory this is: %w", err)
			}

			because := "--type, which overrides what the files here say"

			if kind == "" {
				kind, because, err = detectProject(dir)
			} else {
				err = knownProjectType(kind)
			}

			if err != nil {
				return err
			}

			manifest := starterFor(kind, dir)
			manifest.Because = because

			contents := renderStarter(manifest)
			path := "vallic.yaml"

			if err := writeManifest(filepath.Join(dir, path), contents, force); err != nil {
				return err
			}

			if env.Printer.Structured() {
				notes := manifest.Notes
				if notes == nil {
					// An empty list rather than null, so a script reading
					// .notes[] does not have to handle two shapes for the
					// ordinary case of there being nothing to say.
					notes = []string{}
				}

				return env.Printer.Value(map[string]any{"manifest": map[string]any{
					"path":          path,
					"type":          manifest.Type,
					"detected_from": manifest.Because,
					"notes":         notes,
					"contents":      contents,
				}})
			}

			env.Printer.Good("Wrote %s.", path)

			// Said rather than left implied, because the detection is a guess
			// from two or three filenames and somebody has to be able to
			// disagree with it.
			env.Printer.Say("  %s, from %s", manifest.Type, manifest.Because)

			for _, note := range manifest.Notes {
				env.Printer.Say("  %s", note)
			}

			env.Printer.Say("  check it: vallic validate --local")

			return nil
		},
	}
}

// starter is the file init is about to write.
//
// Settled before any of it reaches the disk, so that what the file will say
// can be read back in a test without a directory to write into.
type starter struct {
	// Type is the manifest's `type`, one of the control plane's ProjectType
	// values.
	Type string

	// Because names what decided that, for somebody who has to disagree.
	Because string

	// VersionKey is the `runtime:` key for this language, which is not the
	// language's own name everywhere: the control plane's
	// MANIFEST_VERSION_KEYS maps its nodejs runtime onto `node`, and a file
	// written with `nodejs:` under runtime would be parsed, understood and
	// then ignored.
	VersionKey string

	// Version is what the repository states it needs, empty where it states
	// nothing.
	Version string

	// Build and Deploy are the commands, in the order they are written.
	Build  []string
	Deploy []string

	// Cache is what the build keeps between builds, as the control plane's
	// ManifestSuggestion::cachePaths() names it for the same kind. Written
	// only beside a build, since it is a key of the build.
	Cache []string

	// Start is what serves the application, for a runtime that is its own
	// server.
	Start string

	// Services are suggestions, written commented out. See initCommand.
	Services []string

	// Notes are printed and never written: something about this checkout the
	// file cannot answer for itself.
	Notes []string
}

// detectProject works out what kind of project is in dir.
//
// Ordered rather than scored, and composer.json comes first because a Drupal
// or Laravel repository carries a package.json for its theme and is not a Node
// project. go.mod is asked before package.json for the same reason, a Go
// service with a front end beside it.
func detectProject(dir string) (kind, because string, err error) {
	if raw, readErr := os.ReadFile(filepath.Join(dir, "composer.json")); readErr == nil {
		// Ordered most specific first. Every framework below is also a PHP
		// application, so a bare `php` is the answer only once none of them
		// match, and matching the wrong one costs a site its uploads.
		for _, known := range []struct{ require, kind string }{
			{"drupal/core", typeDrupal},
			{"laravel/framework", typeLaravel},
			// Two spellings, because WordPress has no single canonical
			// package: Bedrock-style checkouts require one, Composer-managed
			// ones the other.
			{"johnpbloch/wordpress-core", typeWordPress},
			{"roots/wordpress", typeWordPress},
		} {
			if composerRequires(raw, known.require) {
				return known.kind, "composer.json, which requires " + known.require, nil
			}
		}

		// A WordPress site with a composer.json that names no core package is
		// still WordPress, and wp-config.php is the file that says so.
		if _, statErr := os.Stat(filepath.Join(dir, "wp-config.php")); statErr == nil {
			return typeWordPress, "wp-config.php, beside a composer.json naming no core package", nil
		}

		return typePHP, "composer.json, which names no framework this recognises", nil
	}

	// WordPress is the one kind that commonly has no composer.json at all:
	// core, plugins and themes are frequently committed whole. Checked before
	// go.mod and package.json because a build tool's manifest at the root of
	// a WordPress theme would otherwise win.
	if _, readErr := os.Stat(filepath.Join(dir, "wp-config.php")); readErr == nil {
		return typeWordPress, "wp-config.php", nil
	}

	if _, readErr := os.Stat(filepath.Join(dir, "go.mod")); readErr == nil {
		return typeGo, "go.mod", nil
	}

	if _, readErr := os.Stat(filepath.Join(dir, "Cargo.toml")); readErr == nil {
		return typeRust, "Cargo.toml", nil
	}

	// Before package.json, for the reason composer.json is first: a Django
	// site builds its front end with npm and is not a Node project.
	if kind, because := detectPython(dir); kind != "" {
		return kind, because, nil
	}

	// Before package.json too: a Rails application bundling its JavaScript
	// with jsbundling-rails carries one.
	if because := detectRuby(dir); because != "" {
		return typeRuby, because, nil
	}

	if _, readErr := os.Stat(filepath.Join(dir, "package.json")); readErr == nil {
		// Still Node: Bun is a runtime of the nodejs type, chosen under
		// `runtime` and not by the type. Named, because it changes every
		// step the file writes.
		if lock := bunLockfile(dir); lock != "" {
			return typeNode, "package.json, with " + lock + " beside it, so on Bun", nil
		}

		return typeNode, "package.json", nil
	}

	// A usage error rather than a failure: the fix is an argument, and the
	// help printed with it lists the kinds that work.
	return "", "", Usagef(nil,
		"nothing here says what this project is: no composer.json, wp-config.php, go.mod, Cargo.toml, pyproject.toml, requirements.txt, Gemfile or package.json\n"+
			"  name it: vallic init --type drupal|laravel|wordpress|php|nodejs|go|rust|python|django|ruby",
	)
}

// detectRuby recognises a Ruby project, and says whether it is Rails.
//
// One type for both, as on the control plane, where Ruby is Rails's type and
// any other Rack application runs on it unchanged. Rails is still worth
// naming in the reason: it is what decides the asset and migration steps.
func detectRuby(dir string) (because string) {
	raw, err := os.ReadFile(filepath.Join(dir, "Gemfile"))
	if err != nil {
		return ""
	}

	if lock, err := os.ReadFile(filepath.Join(dir, "Gemfile.lock")); err == nil && rubyRequires(lock, "rails") {
		return "Gemfile.lock, which requires Rails"
	}

	if rubyRequires(raw, "rails") {
		return "Gemfile, which requires Rails"
	}

	return "Gemfile"
}

// bunLockfile is the Bun lockfile in dir, or empty where there is none.
//
// Text since Bun 1.2 and binary before it; the control plane looks for both
// when it asks whether a Bun build was declared.
func bunLockfile(dir string) string {
	for _, name := range []string{"bun.lock", "bun.lockb"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return name
		}
	}

	return ""
}

// The files a Python project declares its dependencies in, the most exact
// first: a lock names every package, a requirements file may only include
// another.
var pythonFiles = []string{"uv.lock", "pyproject.toml", "requirements.txt"}

// detectPython recognises a Python project, and Django among them.
//
// Django by manage.py, which `django-admin startproject` writes at the root,
// or by a dependency on it where the project keeps manage.py elsewhere. The
// difference is worth the look: Django is the kind whose uploads survive a
// deploy and whose sessions are cleared, and a Django site written as plain
// Python keeps neither.
func detectPython(dir string) (kind, because string) {
	if _, err := os.Stat(filepath.Join(dir, "manage.py")); err == nil {
		return typeDjango, "manage.py"
	}

	for _, file := range pythonFiles {
		if raw, err := os.ReadFile(filepath.Join(dir, file)); err == nil && pythonRequires(raw, "django") {
			return typeDjango, file + ", which requires Django"
		}
	}

	for _, file := range pythonFiles {
		if _, err := os.Stat(filepath.Join(dir, file)); err == nil {
			return typePython, file
		}
	}

	return "", ""
}

// knownProjectType refuses a --type this writes no file for.
func knownProjectType(kind string) error {
	switch kind {
	case typeDrupal, typeLaravel, typeWordPress, typePHP, typeNode, typeGo, typeRust, typePython, typeDjango, typeRuby:
		return nil
	}

	return Usagef(nil, "--type takes drupal, laravel, wordpress, php, nodejs, go, rust, python, django or ruby, not %q", kind)
}

// starterFor builds the starting file for one kind of project.
func starterFor(kind, dir string) *starter {
	switch kind {
	case typeNode:
		return nodeStarter(dir)
	case typeGo:
		return goStarter(dir)
	case typeRust:
		return rustStarter(dir)
	case typePython, typeDjango:
		return pythonStarter(kind, dir)
	case typeRuby:
		return rubyStarter(dir)
	default:
		return phpStarter(kind, dir)
	}
}

// phpStarter is Drupal, or PHP without it.
//
// No `cron` block, though an hourly drush cron is the obvious thing to put in
// one. Declaring any schedule replaces the one the platform already runs for
// the framework, so a starter carrying a copy of that default would quietly
// take ownership of it, and the copy is the one that goes stale.
func phpStarter(kind, dir string) *starter {
	raw, readErr := os.ReadFile(filepath.Join(dir, "composer.json"))

	manifest := &starter{
		Type:       kind,
		VersionKey: "php",
		// Two components of it. The PHP images are tagged with the language
		// version and the build, `8.4-4.70.12-3`, and the catalogue matches a
		// manifest on the prefix: `8.4` finds that tag and so does `8`.
		Version: firstVersion(composerRequirement(raw, "php"), 2),
		// The step this repository would otherwise be refused for: a
		// composer.json with no committed vendor directory and no build
		// declared is a deploy the control plane stops, naming the command it
		// expected to find here.
		Build:    []string{"composer install --no-dev --optimize-autoloader"},
		Cache:    []string{"vendor"},
		Services: []string{"mariadb: '11.8'"},
	}

	// Everything the install writes, not just vendor/: composer puts Drupal
	// core and contrib under web/, and a build that cached vendor alone still
	// extracted all of core into an empty web/core on every push.
	if kind == typeDrupal {
		manifest.Cache = []string{
			"vendor",
			"web/core",
			"web/modules/contrib",
			"web/themes/contrib",
			"web/profiles/contrib",
			"web/libraries",
		}
	}

	// What brings a release up to date with the code in it. Each is the
	// framework's own command, run by the platform inside the application
	// container, and each is idempotent: a deploy that runs twice is a deploy
	// that happens, so a step that is not safe to repeat is a step that
	// breaks the second time somebody redeploys.
	switch kind {
	case typeDrupal:
		manifest.Deploy = []string{"'drush deploy'"}
	case typeLaravel:
		// --force because there is no terminal to confirm at. Laravel asks
		// before migrating in production and a prompt nobody can answer is a
		// deploy that hangs until it times out.
		manifest.Deploy = []string{"'php artisan migrate --force'"}
	case typeWordPress:
		// Nothing. WordPress upgrades its own schema on the first request
		// after a version change, and `wp core update-db` needs the new
		// version already in place to know there is anything to do. A deploy
		// step here would run before that is true and report success having
		// done nothing.
		manifest.Notes = append(manifest.Notes,
			"WordPress updates its own database on the first request after a deploy, so no deploy step is written")
	}

	if kind == typeWordPress && readErr != nil {
		// Core, plugins and themes committed whole, which is the common shape
		// for WordPress and the one case where the build step below would be
		// wrong. `composer install` with no composer.json fails the build, and
		// it fails it at the point where there is nothing to roll back to.
		manifest.Build = nil
		manifest.Cache = nil
		manifest.Notes = append(manifest.Notes,
			"no composer.json here, so no build step is written and the repository is deployed as it is")
	}

	manifest.noteUnpinned(readErr == nil, "php", "composer.json")

	return manifest
}

// nodeStarter is a Node application, which serves its own HTTP.
func nodeStarter(dir string) *starter {
	raw, readErr := os.ReadFile(filepath.Join(dir, "package.json"))

	if bunLockfile(dir) != "" {
		return bunStarter(raw, readErr)
	}

	manifest := &starter{
		Type:       typeNode,
		VersionKey: "node",
		// The major on its own. Node images are tagged `24.20-1.76.0-1`, so
		// the minor a repository asks for is almost never the minor the
		// platform ships, and a pin matching no tag falls back to the default
		// with nothing in the file to say that it did.
		Version:  firstVersion(packageEngine(raw, "node"), 1),
		Build:    []string{"npm ci"},
		Cache:    []string{"node_modules"},
		Services: []string{"postgres: '18'"},
	}

	if hasScript(raw, "build") {
		manifest.Build = append(manifest.Build, "npm run build")
	}

	// node_modules is the runtime, not a build input, and travels in the
	// release, so the last step leaves only what production needs.
	manifest.Build = append(manifest.Build, "npm prune --omit=dev")

	// Left out of the file rather than guessed at. The image runs `npm start`
	// where nothing says otherwise, and inventing an entry point here would
	// write a `start:` line naming a file that may not exist. Only where the
	// file was read: with no package.json at all there is nothing to say this
	// about, and saying it anyway names a file that is not there.
	if readErr == nil && !hasScript(raw, "start") {
		manifest.Notes = append(manifest.Notes,
			"package.json has no start script, so add a start: line naming what serves the application")
	}

	manifest.noteUnpinned(readErr == nil, "node", "package.json")

	return manifest
}

// bunStarter is a Node application run by Bun, in Node's place.
//
// The same type, port and node_modules as Node, with Bun as the runtime and
// the package manager. What says so is `bun:` under runtime rather than
// `node:`, so it is written whether or not a version is known: without the
// key the project would build and run on Node.
func bunStarter(raw []byte, readErr error) *starter {
	manifest := &starter{
		Type:       typeNode,
		VersionKey: "bun",
		// Two components. The Bun images are tagged `1.4.3-slim-1`, and the
		// catalogue matches a manifest's `1.4` on that line.
		Version: firstVersion(firstNonEmpty(packageEngine(raw, "bun"), packageManagerVersion(raw, "bun")), 2),
		Build:   []string{"bun install --frozen-lockfile"},
		Cache:   []string{"node_modules"},
		// postgres because Bun.sql reads the DATABASE_URL the platform sets
		// for it, with nothing to configure.
		Services: []string{"postgres: '18'"},
	}

	if hasScript(raw, "build") {
		manifest.Build = append(manifest.Build, "bun run build")
	}

	// Bun has no prune. The production install is made again, from Bun's own
	// download cache, into a node_modules with nothing else in it.
	manifest.Build = append(manifest.Build, "rm -rf node_modules && bun install --frozen-lockfile --production")

	if readErr == nil && !hasScript(raw, "start") {
		manifest.Notes = append(manifest.Notes,
			"package.json has no start script, so add a start: line naming what serves the application, such as bun index.ts")
	}

	// The one runtime written without the repository stating it, because a
	// key with no version is not asking for Bun at all: the control plane
	// would build on Node. The major is the one every Bun lockfile is
	// written by, and it matches whichever 1.x the platform offers.
	if manifest.Version == "" {
		manifest.Version = "1"
		manifest.Notes = append(manifest.Notes,
			"nothing here states a Bun version — engines.bun or packageManager in package.json — so runtime names Bun 1, which is what chooses Bun over Node; pin the minor you test with")
	}

	return manifest
}

// packageManagerVersion reads the version a package.json's packageManager
// field pins for one tool: `bun@1.4.3` is 1.4.3 for bun.
func packageManagerVersion(raw []byte, tool string) string {
	var file struct {
		PackageManager string `json:"packageManager"`
	}

	if err := json.Unmarshal(raw, &file); err != nil {
		return ""
	}

	name, version, found := strings.Cut(file.PackageManager, "@")
	if !found || name != tool {
		return ""
	}

	// Corepack appends a hash, `bun@1.4.3+sha512.…`.
	version, _, _ = strings.Cut(version, "+")

	return version
}

// goStarter is a compiled Go binary, which serves its own HTTP too.
func goStarter(dir string) *starter {
	raw, readErr := os.ReadFile(filepath.Join(dir, "go.mod"))

	manifest := &starter{
		Type:       typeGo,
		VersionKey: "go",
		Version:    firstVersion(goDirective(raw), 2),
		Build:      []string{"go build -o bin/server ."},
		// Written for Go and not for Node, because the Go image has no
		// default command: the platform runs what the file names, and a file
		// naming nothing serves nothing.
		Start:    "bin/server",
		Services: []string{"postgres: '18'"},
	}

	// The build step names the package at the root, which is where a
	// single-binary repository keeps it. Reported rather than searched for:
	// a repository with several commands under cmd/ has an answer this
	// cannot pick between, and picking wrongly builds the wrong binary.
	if _, err := os.Stat(filepath.Join(dir, "main.go")); err != nil {
		manifest.Notes = append(manifest.Notes,
			"no main.go here, so check which package the build step names")
	}

	manifest.noteUnpinned(readErr == nil, "go", "go.mod")

	return manifest
}

// rustStarter is a compiled Rust binary, which serves its own HTTP like Go's.
//
// cargo installs it into the release's bin/, which is on PATH, and the image
// starts bin/server. Nothing is cached: cargo's registry and build directory
// are kept by the platform outside the checkout, and target/ declared as a
// cache would be copied into every release.
func rustStarter(dir string) *starter {
	raw, readErr := os.ReadFile(filepath.Join(dir, "Cargo.toml"))

	manifest := &starter{
		Type:       typeRust,
		VersionKey: "rust",
		Version:    rustVersion(dir),
		Build:      []string{"cargo install --locked --path . --root ."},
		Services:   []string{"postgres: '18'"},
	}

	// --locked builds what Cargo.lock says, and with no Cargo.lock there is
	// nothing to hold it to. The same bargain as uv without its lock.
	if _, err := os.Stat(filepath.Join(dir, "Cargo.lock")); err != nil {
		manifest.Build = []string{"cargo install --path . --root ."}
		manifest.Notes = append(manifest.Notes,
			"no Cargo.lock here, so every build resolves the dependencies again: run cargo generate-lockfile, commit it, and add --locked")
	}

	// The binary is named after the package, or its [[bin]]: written out
	// rather than left to the image's bin/server, because a package called
	// anything else serves nothing.
	switch binaries := rustBinaries(dir, raw); len(binaries) {
	case 0:
		manifest.Notes = append(manifest.Notes,
			"Cargo.toml here builds no binary — a workspace, or a library — so check which package the build step names and add a start: line")
	case 1:
		manifest.Start = "bin/" + binaries[0]
	default:
		manifest.Notes = append(manifest.Notes, fmt.Sprintf(
			"Cargo.toml builds %s, so add a start: line naming the one that serves, as bin/<name>",
			strings.Join(binaries, ", "),
		))
	}

	if manifest.Version == "" && readErr == nil {
		manifest.Notes = append(manifest.Notes,
			"nothing here states a Rust version — channel in rust-toolchain.toml — so none is pinned and the platform's default runs")
	}

	return manifest
}

// rustVersion is the toolchain the repository builds with.
//
// rust-toolchain.toml's channel, or the older rust-toolchain file, which the
// platform's build does not obey: the version it builds with is the one under
// runtime, so this is where that file's answer has to go. A channel such as
// stable or nightly names no version and pins nothing. Cargo.toml's
// rust-version is not read: it is the oldest the crate supports, and the
// platform offers the newest two.
func rustVersion(dir string) string {
	if raw, err := os.ReadFile(filepath.Join(dir, "rust-toolchain.toml")); err == nil {
		if channel := tomlValue(raw, "toolchain", "channel"); startsWithDigit(channel) {
			return firstVersion(channel, 2)
		}

		return ""
	}

	if raw, err := os.ReadFile(filepath.Join(dir, "rust-toolchain")); err == nil {
		line, _, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")

		if channel := strings.TrimSpace(line); startsWithDigit(channel) {
			return firstVersion(channel, 2)
		}
	}

	return ""
}

// rustBinaries names what `cargo install` puts in bin/.
//
// Each [[bin]], the package itself where src/main.rs exists, and each file
// under src/bin/, which cargo builds without being told.
func rustBinaries(dir string, raw []byte) []string {
	var names []string

	add := func(name string) {
		if name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}

	for _, name := range tomlTables(raw, "[bin]", "name") {
		add(name)
	}

	if _, err := os.Stat(filepath.Join(dir, "src", "main.rs")); err == nil {
		add(tomlValue(raw, "package", "name"))
	}

	if entries, err := os.ReadDir(filepath.Join(dir, "src", "bin")); err == nil {
		for _, entry := range entries {
			switch {
			case !entry.IsDir() && strings.HasSuffix(entry.Name(), ".rs"):
				add(strings.TrimSuffix(entry.Name(), ".rs"))
			case entry.IsDir():
				if _, err := os.Stat(filepath.Join(dir, "src", "bin", entry.Name(), "main.rs")); err == nil {
					add(entry.Name())
				}
			}
		}
	}

	return names
}

// tomlValue reads one string out of one table of a TOML file.
//
// Enough of TOML for the two files read here, which keep the values asked
// about as `key = "value"` under a plain [table] header, and no more: this is
// a starting file, and a value this cannot find is said rather than guessed.
func tomlValue(raw []byte, table, key string) string {
	values := tomlTables(raw, table, key)
	if len(values) == 0 {
		return ""
	}

	return values[0]
}

// tomlTables reads a key out of every table with a header, in order: once
// for [package], once per entry of an array of tables such as [[bin]].
//
// The table is named as its header is written, without the outer brackets:
// `package` for [package], `[bin]` for [[bin]].
func tomlTables(raw []byte, table, key string) []string {
	var (
		values  []string
		current string
	)

	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "[") {
			header, _, _ := strings.Cut(line, "#")
			current = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(header), "["), "]")
			continue
		}

		if current != table {
			continue
		}

		name, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(name) != key {
			continue
		}

		value, _, _ = strings.Cut(strings.TrimSpace(value), "#")
		values = append(values, strings.Trim(strings.TrimSpace(value), `"'`))
	}

	return values
}

func startsWithDigit(s string) bool {
	return s != "" && s[0] >= '0' && s[0] <= '9'
}

// pythonStarter is a Python application, Django or not, which serves its own
// HTTP through Gunicorn.
//
// Everything is run as `python -m …` or `python manage.py …`. The virtual
// environment is built in the checkout and moved into the release, so
// `python` is always the release's own, while a script such as
// .venv/bin/gunicorn still names the directory it was built in.
func pythonStarter(kind, dir string) *starter {
	manifest := &starter{
		Type:       kind,
		VersionKey: "python",
		Version:    pythonVersion(dir),
		Services:   []string{"postgres: '18'"},
	}

	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))

		return err == nil
	}

	// Whatever the repository locks with. uv with its lock, so a build
	// installs exactly what was tested; pip where requirements.txt is what
	// the project keeps; uv without a lock, resolving afresh, for a
	// pyproject.toml alone. Each keeps .venv between builds, so the next one
	// only brings it up to date.
	switch {
	case exists("uv.lock"):
		manifest.Build = []string{"uv sync --locked --no-dev --no-editable"}
		manifest.Cache = []string{".venv"}
	case exists("requirements.txt"):
		manifest.Build = []string{"python -m venv .venv", ".venv/bin/pip install -r requirements.txt"}
		manifest.Cache = []string{".venv"}
	case exists("pyproject.toml"):
		manifest.Build = []string{"uv sync --no-dev --no-editable"}
		manifest.Cache = []string{".venv"}
		manifest.Notes = append(manifest.Notes,
			"no uv.lock here, so every build resolves the dependencies again: run uv lock, commit it, and add --locked")
	default:
		manifest.Notes = append(manifest.Notes,
			"no uv.lock, pyproject.toml or requirements.txt here, so no build step is written")
	}

	if manifest.Version == "" {
		manifest.Notes = append(manifest.Notes,
			"nothing here states a Python version — .python-version, or requires-python in pyproject.toml — so none is pinned and the platform's default runs")
	}

	if kind != typeDjango {
		// Gunicorn reads gunicorn.conf.py where it starts, and the image
		// starts it with nothing else, so a project with one needs no line.
		// Otherwise the application's module and variable are its own, and a
		// guess would name one that is not there.
		if !exists("gunicorn.conf.py") {
			manifest.Notes = append(manifest.Notes,
				"add a start: line naming what serves the application, such as python -m gunicorn app:app")
		}

		return manifest
	}

	// Collected in the build, which can write, and served by the application
	// from the release. The step needs STATIC_ROOT, which only the settings
	// can say.
	manifest.Build = append(manifest.Build, "python manage.py collectstatic --noinput")
	manifest.Deploy = []string{"'python manage.py migrate --noinput'"}

	project, settings := djangoProject(dir)
	if project == "" {
		manifest.Notes = append(manifest.Notes,
			"no project package with a wsgi.py found, so add a start: line naming the WSGI application")
	} else {
		manifest.Start = "python -m gunicorn " + project + ".wsgi"
	}

	if raw, err := os.ReadFile(settings); err == nil && !strings.Contains(string(raw), "STATIC_ROOT") {
		name, _ := filepath.Rel(dir, settings)
		manifest.Notes = append(manifest.Notes,
			"the build runs collectstatic, which needs STATIC_ROOT in "+filepath.ToSlash(name))
	}

	return manifest
}

// rubyStarter is a Ruby application served by Puma from its release: Rails,
// or Sinatra, Hanami or any other Rack application.
//
// The bundle is installed into the checkout's vendor/bundle, held to
// Gemfile.lock and without the development and test groups — the image's own
// Bundler settings, so `bundle install` is the whole of the step — and travels
// in the release as node_modules does. With no start: line the image runs
// `bundle exec puma -p $PORT`, which reads config/puma.rb.
func rubyStarter(dir string) *starter {
	gemfile, gemfileErr := os.ReadFile(filepath.Join(dir, "Gemfile"))
	lock, lockErr := os.ReadFile(filepath.Join(dir, "Gemfile.lock"))

	manifest := &starter{
		Type:       typeRuby,
		VersionKey: "ruby",
		Version:    rubyVersion(dir, gemfile, lock),
		Build:      []string{"bundle install"},
		Cache:      []string{"vendor/bundle"},
		Services:   []string{"postgres: '18'"},
	}

	requires := func(name string) bool {
		return rubyRequires(lock, name) || rubyRequires(gemfile, name)
	}

	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))

		return err == nil
	}

	switch {
	case gemfileErr != nil:
		// Only through --type: a checkout with no Gemfile is not detected
		// as Ruby. `bundle install` with nothing to install fails the build.
		manifest.Build, manifest.Cache = nil, nil
		manifest.Notes = append(manifest.Notes,
			"no Gemfile here, so no build step is written and the repository is deployed as it is")
	case lockErr != nil:
		// The image installs held to the lock, so a build with none fails
		// rather than resolving afresh.
		manifest.Notes = append(manifest.Notes,
			"no Gemfile.lock here, and the build installs only what it locks: run bundle install and commit Gemfile.lock")
	default:
		if platforms := gemPlatforms(lock); len(platforms) > 0 && !slices.ContainsFunc(platforms, func(p string) bool {
			return p == "ruby" || strings.Contains(p, "linux")
		}) {
			// A lock made on a Mac names the Mac alone, and an install held
			// to it refuses to add the platform it is building on.
			manifest.Notes = append(manifest.Notes, fmt.Sprintf(
				"Gemfile.lock is locked for %s only, and the build runs on Linux: bundle lock --add-platform x86_64-linux",
				strings.Join(platforms, ", "),
			))
		}
	}

	if manifest.Version == "" {
		manifest.Notes = append(manifest.Notes,
			"nothing here states a Ruby version — .ruby-version, or ruby in the Gemfile — so none is pinned and the platform's default runs")
	}

	if !requires("puma") {
		manifest.Notes = append(manifest.Notes,
			"the image serves with Puma, which the Gemfile does not name: add gem \"puma\", or a start: line naming what serves the application")
	}

	if !requires("rails") {
		if !exists("config.ru") {
			manifest.Notes = append(manifest.Notes,
				"no config.ru here, which is what Puma serves: add one, or a start: line naming what serves the application")
		}

		return manifest
	}

	// Compiled in the build, which can write, and served by Rails from the
	// release. A build is given no environment, and SECRET_KEY_BASE_DUMMY is
	// what lets Rails 7.1 and later boot for it without the key. Only with
	// an asset pipeline in the bundle: an API application has no
	// assets:precompile, and the step would fail the build.
	if requires("propshaft") || requires("sprockets-rails") || requires("sprockets") {
		manifest.Build = append(manifest.Build, "SECRET_KEY_BASE_DUMMY=1 bin/rails assets:precompile")

		for _, bundler := range []string{"jsbundling-rails", "cssbundling-rails"} {
			if requires(bundler) {
				manifest.Notes = append(manifest.Notes,
					bundler+" builds with Node, which the Ruby image does not have: name node under services and build in its image before assets:precompile, as the Rails guide shows")

				break
			}
		}
	} else {
		manifest.Notes = append(manifest.Notes,
			"no asset pipeline in the Gemfile — Propshaft or Sprockets — so no assets:precompile step is written")
	}

	// db:prepare, not db:migrate: it creates the schema on the empty database
	// of a first release, which is when the control plane runs deploy steps
	// for Ruby, and migrates every release after.
	manifest.Deploy = []string{"'bin/rails db:prepare'"}

	return manifest
}

// rubyVersion is the Ruby the repository says it runs on.
//
// .ruby-version first, which rbenv, chruby and Rails's own generator write;
// then the Gemfile's `ruby` line; then the version Gemfile.lock was resolved
// with. The image's Ruby is the one that runs whatever these say, so a version
// one of them names is a version runtime has to be told. Two components:
// the images are tagged `4.0.7-slim-trixie-1`, and the catalogue matches
// `4.0` on that line.
func rubyVersion(dir string, gemfile, lock []byte) string {
	if raw, err := os.ReadFile(filepath.Join(dir, ".ruby-version")); err == nil {
		line, _, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")

		if version := firstVersion(line, 2); strings.Contains(version, ".") {
			return version
		}
	}

	for _, line := range strings.Split(string(gemfile), "\n") {
		line, _, _ = strings.Cut(strings.TrimSpace(line), "#")

		// `ruby "3.4.1"` and `ruby '~> 3.4'`, but not `ruby file:
		// ".ruby-version"`, which names a file read above.
		if rest, found := strings.CutPrefix(line, "ruby "); found && !strings.Contains(rest, "file:") {
			if version := firstVersion(rest, 2); strings.Contains(version, ".") {
				return version
			}
		}
	}

	// RUBY VERSION, then `   ruby 3.4.1p194` on the line after it.
	if _, after, found := strings.Cut(string(lock), "RUBY VERSION\n"); found {
		line, _, _ := strings.Cut(after, "\n")

		if version := firstVersion(line, 2); strings.Contains(version, ".") {
			return version
		}
	}

	return ""
}

// rubyRequires reports whether a Gemfile or Gemfile.lock names a gem.
//
// A Gemfile names one as `gem "rails", "~> 8.1"`, a lock as `    rails
// (8.1.0)` among its specs and `  rails (~> 8.1)` among its dependencies:
// in both, the name is the first word of the line, quoted in a Gemfile.
func rubyRequires(raw []byte, name string) bool {
	for _, line := range strings.Split(string(raw), "\n") {
		line, _, _ = strings.Cut(strings.TrimSpace(line), "#")

		if rest, found := strings.CutPrefix(line, "gem "); found {
			rest = strings.TrimSpace(rest)

			if len(rest) > 1 && (rest[0] == '"' || rest[0] == '\'') {
				if gem, _, closed := strings.Cut(rest[1:], rest[:1]); closed && gem == name {
					return true
				}
			}

			continue
		}

		word := strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == '(' || r == '!' })
		if len(word) > 0 && word[0] == name {
			return true
		}
	}

	return false
}

// gemPlatforms are the platforms a Gemfile.lock was resolved for.
func gemPlatforms(lock []byte) []string {
	_, after, found := strings.Cut(string(lock), "PLATFORMS\n")
	if !found {
		return nil
	}

	var platforms []string

	for _, line := range strings.Split(after, "\n") {
		// The section ends at the first line that is not indented under it.
		if !strings.HasPrefix(line, " ") {
			break
		}

		platforms = append(platforms, strings.TrimSpace(line))
	}

	return platforms
}

// The settings module manage.py names, as startproject writes it.
var djangoSettingsModule = regexp.MustCompile(`DJANGO_SETTINGS_MODULE['"]\s*,\s*['"]([A-Za-z_][\w.]*)['"]`)

// djangoProject finds the package holding the site's wsgi.py and settings.
//
// From manage.py where it says, which is the one answer the project itself
// gives. Otherwise the one directory here holding both files; with none, or
// with several, the answer is the project's to write rather than this
// command's to pick.
func djangoProject(dir string) (project, settings string) {
	if raw, err := os.ReadFile(filepath.Join(dir, "manage.py")); err == nil {
		if match := djangoSettingsModule.FindSubmatch(raw); match != nil {
			module := string(match[1])
			project, _, _ = strings.Cut(module, ".")
			settings = filepath.Join(dir, filepath.FromSlash(strings.ReplaceAll(module, ".", "/"))+".py")

			if _, err := os.Stat(filepath.Join(dir, project, "wsgi.py")); err == nil {
				return project, settings
			}

			return "", settings
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", ""
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		wsgi := filepath.Join(dir, entry.Name(), "wsgi.py")
		candidate := filepath.Join(dir, entry.Name(), "settings.py")

		if _, err := os.Stat(wsgi); err != nil {
			continue
		}

		if _, err := os.Stat(candidate); err != nil {
			continue
		}

		if project != "" {
			return "", ""
		}

		project, settings = entry.Name(), candidate
	}

	return project, settings
}

// pythonVersion is the Python the repository says it runs on.
//
// .python-version first, which uv and pyenv write and which names the
// interpreter actually in use; then requires-python, which is a floor rather
// than a choice but is the version the project has promised to work on. Two
// components, because the images are tagged `3.14.8-slim-trixie-1` and the
// catalogue matches a manifest's `3.14` on the version line.
func pythonVersion(dir string) string {
	if raw, err := os.ReadFile(filepath.Join(dir, ".python-version")); err == nil {
		line, _, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")

		if version := firstVersion(line, 2); strings.Contains(version, ".") {
			return version
		}
	}

	raw, err := os.ReadFile(filepath.Join(dir, "pyproject.toml"))
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(raw), "\n") {
		key, value, found := strings.Cut(line, "=")

		if found && strings.TrimSpace(key) == "requires-python" {
			return firstVersion(value, 2)
		}
	}

	return ""
}

// pythonRequires reports whether a dependency file asks for a package.
//
// Read the way all three spell a requirement: a requirements.txt line, a
// quoted entry in pyproject.toml's dependencies, uv.lock's `name = "…"`. Each
// starts with the package's name, which Python compares ignoring case and
// treating `-`, `_` and `.` as one character.
func pythonRequires(raw []byte, name string) bool {
	for _, line := range strings.Split(string(raw), "\n") {
		line, _, _ = strings.Cut(line, "#")
		candidates := []string{strings.TrimSpace(line)}

		for _, quote := range []string{`"`, "'"} {
			parts := strings.Split(line, quote)

			for i := 1; i < len(parts); i += 2 {
				candidates = append(candidates, parts[i])
			}
		}

		for _, candidate := range candidates {
			if pythonPackageName(candidate) == name {
				return true
			}
		}
	}

	return false
}

// pythonPackageName is the normalised name a requirement starts with.
func pythonPackageName(requirement string) string {
	end := strings.IndexFunc(requirement, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.')
	})
	if end < 0 {
		end = len(requirement)
	}

	return strings.ToLower(strings.NewReplacer("_", "-", ".", "-").Replace(requirement[:end]))
}

// noteUnpinned records that no version was pinned, and which file would have
// said one.
//
// Worth saying out loud, because an unpinned runtime is not a neutral state:
// it is the platform's current default, which moves when the catalogue does.
func (s *starter) noteUnpinned(stated bool, language, file string) {
	if s.Version != "" || !stated {
		return
	}

	s.Notes = append(s.Notes, fmt.Sprintf(
		"%s states no %s version, so none is pinned and the platform's default runs",
		file, language,
	))
}

// renderStarter turns a starter into the file.
func renderStarter(s *starter) string {
	var b strings.Builder

	b.WriteString("# Written by vallic init. Check it, then: vallic validate --local\n")
	fmt.Fprintf(&b, "version: %d\n", manifestSchemaVersion)
	fmt.Fprintf(&b, "type: %s\n", s.Type)

	if s.Version != "" {
		fmt.Fprintf(&b, "\nruntime:\n  %s: '%s'\n", s.VersionKey, s.Version)
	}

	b.WriteString(`
# What this application needs beside it, with the version the code was written
# against. Nothing is declared here because a checkout does not say, and naming
# a database an environment does not run refuses the deploy rather than warning
# about it. ` + "`vallic service list`" + ` prints what one runs, and its VERSION
# column is what goes here.
#
# services:
`)

	for _, service := range s.Services {
		fmt.Fprintf(&b, "#   - %s\n", service)
	}

	if len(s.Build) > 0 {
		b.WriteString("\nbuild:\n  steps:\n")

		for _, step := range s.Build {
			fmt.Fprintf(&b, "    - %s\n", step)
		}

		// Declared whole from the first build: the paths describe one
		// install between them, and adding one later starts the set again.
		if len(s.Cache) > 0 {
			b.WriteString("  cache:\n")

			for _, path := range s.Cache {
				fmt.Fprintf(&b, "    - %s\n", path)
			}
		}
	}

	if len(s.Deploy) > 0 {
		b.WriteString("\ndeploy:\n  steps:\n")

		for _, step := range s.Deploy {
			fmt.Fprintf(&b, "    - %s\n", step)
		}

		// A migration that fails half way through leaves new code running
		// against an old schema, and the release before it is the only thing
		// known to work.
		b.WriteString("  on_failure: rollback\n")
	}

	if s.Start != "" {
		fmt.Fprintf(&b, "\nstart: %s\n", s.Start)
	}

	return b.String()
}

// writeManifest writes the file, and refuses to lose one that is already there.
//
// O_EXCL rather than a Stat followed by a Create, so the check and the write
// are one call: between two calls the file somebody is about to lose can
// appear, and an overwritten manifest is not a mistake anybody recovers from
// by running the command again.
func writeManifest(path, contents string, force bool) error {
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}

	file, err := os.OpenFile(path, flags, 0o644)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s is already here\n  replace it: vallic init --force", filepath.Base(path))
	}
	if err != nil {
		return fmt.Errorf("cannot write %s: %w", path, err)
	}

	if _, err := file.WriteString(contents); err != nil {
		file.Close()

		return fmt.Errorf("cannot write %s: %w", path, err)
	}

	return file.Close()
}

// composerRequires reports whether a composer.json asks for a package.
//
// Both sections, because a site's own repository requires drupal/core while a
// module's requires it under require-dev, and both are Drupal. Matched on the
// prefix: drupal/core-recommended is how most sites spell it.
func composerRequires(raw []byte, prefix string) bool {
	var file struct {
		Require    map[string]string `json:"require"`
		RequireDev map[string]string `json:"require-dev"`
	}

	// A composer.json that does not parse is still a PHP project. Refusing
	// here would turn a broken dependency file into a reason to write no
	// manifest at all, which is two problems where there was one.
	if err := json.Unmarshal(raw, &file); err != nil {
		return false
	}

	for _, section := range []map[string]string{file.Require, file.RequireDev} {
		for name := range section {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
	}

	return false
}

// composerRequirement reads one constraint out of a composer.json.
func composerRequirement(raw []byte, name string) string {
	var file struct {
		Require map[string]string `json:"require"`
	}

	if err := json.Unmarshal(raw, &file); err != nil {
		return ""
	}

	return file.Require[name]
}

// packageEngine reads one entry of a package.json's engines block.
func packageEngine(raw []byte, name string) string {
	var file struct {
		Engines map[string]string `json:"engines"`
	}

	if err := json.Unmarshal(raw, &file); err != nil {
		return ""
	}

	return file.Engines[name]
}

// hasScript reports whether a package.json defines a named script.
func hasScript(raw []byte, name string) bool {
	var file struct {
		Scripts map[string]string `json:"scripts"`
	}

	if err := json.Unmarshal(raw, &file); err != nil {
		return false
	}

	_, ok := file.Scripts[name]

	return ok
}

// goDirective reads the language version out of a go.mod.
//
// The `go` line and not `toolchain`: the first is the version the module is
// written against, the second is whatever happens to be installed to build it.
func goDirective(raw []byte) string {
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)

		if len(fields) >= 2 && fields[0] == "go" {
			return fields[1]
		}
	}

	return ""
}

// firstVersion pulls a version out of whatever a package manager calls a
// constraint, keeping at most components of it.
//
// `^8.3`, `>=8.2 <9`, `22.x` and `1.27.0` are one question asked four ways,
// and each of them starts with the number being asked for. How much of it to
// keep is the caller's to say, because that depends on how the catalogue tags
// that language's images rather than on how the constraint was written.
func firstVersion(constraint string, components int) string {
	start := strings.IndexFunc(constraint, func(r rune) bool { return r >= '0' && r <= '9' })
	if start < 0 || components < 1 {
		return ""
	}

	var (
		parts   []string
		current strings.Builder
	)

	for _, r := range constraint[start:] {
		if r >= '0' && r <= '9' {
			current.WriteRune(r)
			continue
		}

		if r != '.' || len(parts) >= components-1 {
			break
		}

		parts = append(parts, current.String())
		current.Reset()
	}

	return strings.Join(append(parts, current.String()), ".")
}
