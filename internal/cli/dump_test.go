package cli

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vallic/vallic-cli/internal/api"
	vssh "github.com/vallic/vallic-cli/internal/ssh"
)

// A dump needs nothing but the environment, so the name has to say whose it
// is: two environments of one project, or two projects' productions, never
// land on the same file.
func TestDumpFilenameNamesTheProjectAndEnvironment(t *testing.T) {
	target := &Target{
		Project:     &api.Project{MachineName: "acme-site"},
		Environment: &api.Environment{Name: "production", Slug: "acme-site-production"},
	}
	at := time.Date(2026, 10, 18, 14, 30, 5, 0, time.FixedZone("CEST", 2*60*60))

	cases := []struct {
		gzipped, stamped bool
		want             string
	}{
		{false, false, "acme-site--production--dump.sql"},
		{true, false, "acme-site--production--dump.sql.gz"},
		// UTC, whatever the machine's zone, so names sort as dumps were taken.
		{false, true, "acme-site--production--20261018-123005-UTC--dump.sql"},
	}

	for _, c := range cases {
		if got := dumpFilename(target, dumpPart{}, c.gzipped, c.stamped, at); got != c.want {
			t.Errorf("dumpFilename(gzip %v, timestamp %v) = %q, want %q", c.gzipped, c.stamped, got, c.want)
		}
	}

	// An environment named after its branch carries the branch's slash,
	// which must not become a directory.
	branch := &Target{Project: &api.Project{MachineName: "acme"}, Environment: &api.Environment{Name: "feature/checkout"}}
	if got := dumpFilename(branch, dumpPart{}, false, false, at); got != "acme--feature-checkout--dump.sql" {
		t.Errorf("dumpFilename() = %q, want no slash in it", got)
	}
}

// The file appears only once the dump is whole. A dump that fails half way
// must leave nothing behind that looks like a good one — not the name, and
// not the partial file beside it.
func TestWriteDumpLeavesNothingWhenTheDumpFails(t *testing.T) {
	env, _, _ := followEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "acme--production--dump.sql")

	failing := exec.Command("sh", "-c", "printf 'CREATE TABLE half'; exit 3")

	err := writeDump(env, failing, path, false, "acme-production")

	var passthrough *PassthroughError
	if !errors.As(err, &passthrough) || passthrough.Code != 3 {
		t.Fatalf("writeDump() = %v, want the far side's exit status passed through", err)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("left behind %v after a failed dump", names(entries))
	}

	working := exec.Command("sh", "-c", "printf 'CREATE TABLE whole;\\n'")

	if err := writeDump(env, working, path, false, "acme-production"); err != nil {
		t.Fatalf("writeDump() = %v", err)
	}

	if raw, _ := os.ReadFile(path); string(raw) != "CREATE TABLE whole;\n" {
		t.Errorf("the dump is %q", raw)
	}

	entries, _ = os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("the directory holds %v, want the dump alone", names(entries))
	}

	// Nobody to ask, so the dump already there is kept.
	again := exec.Command("sh", "-c", "printf 'newer'")
	if err := writeDump(env, again, path, false, "acme-production"); err == nil || !strings.Contains(err.Error(), "--timestamp") {
		t.Errorf("writeDump() over an existing dump = %v, want a refusal naming --timestamp", err)
	}

	if raw, _ := os.ReadFile(path); string(raw) != "CREATE TABLE whole;\n" {
		t.Errorf("the refused dump replaced the file: %q", raw)
	}
}

// What db dump --gzip writes, db import reads back, from a file or from
// standard input, without being told it is compressed.
func TestADumpIsReadBackCompressedOrNot(t *testing.T) {
	env, _, _ := followEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "dump.sql.gz")

	if err := writeDump(env, exec.Command("sh", "-c", "printf 'INSERT INTO t VALUES (1);\\n'"), path, true, "acme-production"); err != nil {
		t.Fatalf("writeDump() = %v", err)
	}

	raw, _ := os.ReadFile(path)
	if len(raw) < 2 || raw[0] != 0x1f || raw[1] != 0x8b {
		t.Fatalf("the .gz dump is not gzip: %q", raw)
	}

	reader, described, closer, err := openDump(env, path)
	if err != nil {
		t.Fatalf("openDump() = %v", err)
	}
	defer closer()

	if got, _ := io.ReadAll(reader); string(got) != "INSERT INTO t VALUES (1);\n" {
		t.Errorf("read back %q", got)
	}

	if !strings.Contains(described, "decompressed") {
		t.Errorf("described as %q, want it said that it was decompressed", described)
	}

	// The same bytes on standard input.
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	writer.Write([]byte("SELECT 1;\n"))
	writer.Close()

	env.In = &compressed

	reader, _, closer, err = openDump(env, "")
	if err != nil {
		t.Fatalf("openDump(stdin) = %v", err)
	}
	defer closer()

	if got, _ := io.ReadAll(reader); string(got) != "SELECT 1;\n" {
		t.Errorf("read back %q from standard input", got)
	}

	// And a plain dump passes through untouched.
	env.In = strings.NewReader("SELECT 2;\n")

	reader, described, closer, err = openDump(env, "")
	if err != nil {
		t.Fatalf("openDump(plain) = %v", err)
	}
	defer closer()

	if got, _ := io.ReadAll(reader); string(got) != "SELECT 2;\n" || described != "standard input" {
		t.Errorf("read back %q as %q", got, described)
	}
}

// --stdout writes no file, so the flags that shape one are refused rather
// than ignored — before anything is asked of the platform.
func TestDumpRefusesFlagsThatContradictEachOther(t *testing.T) {
	for _, args := range [][]string{
		{"db", "dump", "--stdout", "--file", "x.sql"},
		{"db", "dump", "-o", "-t"},
		{"db", "dump", "--file", "x.sql", "--directory", "dumps"},
		{"db", "dump", "staging", "production"},
	} {
		env, _, _ := followEnv(t)

		err := Execute(t.Context(), Root(), env, args)

		var usage *UsageError
		if !errors.As(err, &usage) {
			t.Errorf("%q = %v, want a usage error", args, err)
		}
	}
}

func names(entries []os.DirEntry) []string {
	var out []string
	for _, entry := range entries {
		out = append(out, entry.Name())
	}

	return out
}

// A part of the database is asked for only where the environment takes it.
// A machine whose agent reads no options would run `db-export --table=users`
// as a command and answer "not found", so that is refused here, in a sentence.
func TestAPartOfTheDatabaseIsAskedForOnlyWhereItIsOffered(t *testing.T) {
	part := dumpPart{tables: tableList{"users", "orders"}, excluded: tableList{"sessions"}, schemaOnly: true}

	old := &vssh.Target{SSHTarget: api.SSHTarget{DBExport: "db-export"}}
	if _, err := part.verb(old); err == nil || !strings.Contains(err.Error(), "does not offer --schema-only") {
		t.Errorf("verb() on an agent with no options = %v, want a refusal", err)
	}

	// The whole database is still there to dump.
	if verb, err := (&dumpPart{}).verb(old); err != nil || verb != "db-export" {
		t.Errorf("verb() for the whole database = %q, %v", verb, err)
	}

	current := &vssh.Target{SSHTarget: api.SSHTarget{
		DBExport:        "db-export",
		DBExportOptions: []string{"--schema-only", "--table", "--exclude-table"},
	}}

	verb, err := part.verb(current)
	if err != nil {
		t.Fatalf("verb() = %v", err)
	}

	if want := "db-export --schema-only --table=users --table=orders --exclude-table=sessions"; verb != want {
		t.Errorf("verb() = %q, want %q", verb, want)
	}

	// And the file says which part it holds, as Upsun names one.
	target := &Target{Project: &api.Project{MachineName: "acme"}, Environment: &api.Environment{Name: "production"}}
	if got := dumpFilename(target, part, true, false, time.Now()); got != "acme--production--users,orders--excl-sessions--schema--dump.sql.gz" {
		t.Errorf("dumpFilename() = %q", got)
	}
}

// What the agent would refuse is refused before anything connects.
func TestATableNameIsCheckedBeforeConnecting(t *testing.T) {
	for _, part := range []dumpPart{
		{tables: tableList{"cache_*"}},
		{tables: tableList{"-r/tmp/x"}},
		{excluded: tableList{"a;b"}},
		{tables: tableList{"users"}, excluded: tableList{"users"}},
	} {
		var usage *UsageError
		if err := part.check(); !errors.As(err, &usage) {
			t.Errorf("check(%+v) = %v, want a usage error", part, err)
		}
	}

	var list tableList
	list.Set("users, orders")
	list.Set("users")
	list.Set("public.audit")

	if list.String() != "users,orders,public.audit" {
		t.Errorf("tableList = %q, want each once, commas and repeats alike", list.String())
	}

	if err := (&dumpPart{tables: list}).check(); err != nil {
		t.Errorf("check() refused %q: %v", list, err)
	}
}

// db-size's answer: the total alone, then a table and its size on each line.
func TestTheDatabaseSizeIsReadAsTheAgentWritesIt(t *testing.T) {
	size, err := parseDatabaseSize("104857600\ncache_render\t62914560\nusers\t1048576\n")
	if err != nil {
		t.Fatal(err)
	}

	if size.Bytes != 104857600 || len(size.Tables) != 2 || size.Tables[0].Name != "cache_render" || size.Tables[1].Bytes != 1048576 {
		t.Errorf("read as %+v", size)
	}

	if _, err := parseDatabaseSize("ERROR 1045 (28000): Access denied\n"); err == nil {
		t.Error("an answer that is not a size was read as one")
	}

	empty, err := parseDatabaseSize("0\n")
	if err != nil || empty.Bytes != 0 || empty.Tables == nil {
		t.Errorf("an empty database read as %+v, %v; want zero and an empty list", empty, err)
	}
}
