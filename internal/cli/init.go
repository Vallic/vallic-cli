package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
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
		Usage:   "init [--type drupal|laravel|wordpress|php|nodejs|go] [--force]",
		Long: `Writes vallic.yaml in the working directory, and nothing else. It
needs no credential and reaches nothing, so it works in a checkout of a
project that does not exist on the platform yet.

What kind of project this is comes from the files here: a composer.json
requiring drupal/core is Drupal, laravel/framework is Laravel, a WordPress
core package or a wp-config.php is WordPress, a composer.json on its own is
PHP, go.mod is Go, package.json is Node. The command says which file decided
it, and --type overrides the answer.

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

	if _, readErr := os.Stat(filepath.Join(dir, "package.json")); readErr == nil {
		return typeNode, "package.json", nil
	}

	// A usage error rather than a failure: the fix is an argument, and the
	// help printed with it lists the four that work.
	return "", "", Usagef(nil,
		"nothing here says what this project is: no composer.json, wp-config.php, go.mod or package.json\n"+
			"  name it: vallic init --type drupal|laravel|wordpress|php|nodejs|go",
	)
}

// knownProjectType refuses a --type this writes no file for.
func knownProjectType(kind string) error {
	switch kind {
	case typeDrupal, typeLaravel, typeWordPress, typePHP, typeNode, typeGo:
		return nil
	}

	return Usagef(nil, "--type takes drupal, laravel, wordpress, php, nodejs or go, not %q", kind)
}

// starterFor builds the starting file for one kind of project.
func starterFor(kind, dir string) *starter {
	switch kind {
	case typeNode:
		return nodeStarter(dir)
	case typeGo:
		return goStarter(dir)
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
		Services: []string{"mariadb: '11.8'"},
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
		manifest.Notes = append(manifest.Notes,
			"no composer.json here, so no build step is written and the repository is deployed as it is")
	}

	manifest.noteUnpinned(readErr == nil, "php", "composer.json")

	return manifest
}

// nodeStarter is a Node application, which serves its own HTTP.
func nodeStarter(dir string) *starter {
	raw, readErr := os.ReadFile(filepath.Join(dir, "package.json"))

	manifest := &starter{
		Type:       typeNode,
		VersionKey: "node",
		// The major on its own. Node images are tagged `24.20-1.76.0-1`, so
		// the minor a repository asks for is almost never the minor the
		// platform ships, and a pin matching no tag falls back to the default
		// with nothing in the file to say that it did.
		Version:  firstVersion(packageEngine(raw, "node"), 1),
		Build:    []string{"npm ci"},
		Services: []string{"postgres: '18'"},
	}

	if hasScript(raw, "build") {
		manifest.Build = append(manifest.Build, "npm run build")
	}

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
