//go:build smoke

package smoke

import (
	"os"
	"path/filepath"
	"testing"
)

// firstEnvironment picks something to read, from whatever is there.
//
// Chosen rather than configured, so the same credential works against a
// developer's DDEV site and against a staging installation with different
// data in it. A smoke test that needed a named project would be a smoke test
// with a second thing to keep in step.
//
// Prefers a running environment: several of these read what a stack is doing,
// and a draft has never been built, so failing on one would be failing the
// fixture rather than the code.
func firstEnvironment(t *testing.T, c *cli) (project, environment string) {
	t.Helper()

	projects := c.decode("project", "list")["projects"].([]any)

	if len(projects) == 0 {
		t.Skip("this control plane has no projects to read")
	}

	environments := c.decode("env", "list", "--all")["environments"].([]any)

	if len(environments) == 0 {
		t.Skip("this control plane has no environments to read")
	}

	byID := map[float64]string{}

	for _, row := range projects {
		p := row.(map[string]any)
		byID[p["id"].(float64)] = p["machine_name"].(string)
	}

	// Two passes rather than one: a running environment is the one that
	// answers every read, so take one if there is one and settle for anything
	// only when there is not.
	for _, wanted := range []string{"running", ""} {
		for _, row := range environments {
			e := row.(map[string]any)

			if wanted != "" && e["state"] != wanted {
				continue
			}

			name, known := byID[e["project"].(float64)]
			if !known {
				// An environment of a project this credential cannot see,
				// which the commands below would be refused on.
				continue
			}

			return name, e["name"].(string)
		}
	}

	t.Skip("no environment here belongs to a project this credential can see")

	return "", ""
}

// writeFile puts a file in a scratch directory.
func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
