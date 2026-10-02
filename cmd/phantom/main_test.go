package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/rroblf01/task-rabbit-mcp/internal/probe"
)

func report(instructions string, tools ...string) probe.Report {
	r := probe.Report{Instructions: instructions, ToolsSupported: len(tools) > 0}
	for _, name := range tools {
		r.Tools = append(r.Tools, mcp.Tool{Name: name, Description: "does " + name})
	}
	return r
}

func TestScanPassesRegisteredTools(t *testing.T) {
	found := scan(report("call loadTaskContext then startTaskBranch", "loadTaskContext", "startTaskBranch"))
	if len(found) != 0 {
		t.Errorf("registered tools were reported as phantoms: %+v", found)
	}
}

func TestScanCatchesARegisteredToolNamedInTheProseThatIsGone(t *testing.T) {
	// The exact shape of the bug this exists for: a rename, or a tool behind a flag,
	// leaving the instructions pointing at nothing.
	found := scan(report("call transitionTaskIssue to change the status", "loadTaskContext"))
	if len(found) != 1 || found[0].name != "transitionTaskIssue" {
		t.Fatalf("found = %+v, want transitionTaskIssue", found)
	}
	if !strings.Contains(found[0].where, "instructions") {
		t.Errorf("where = %q, want it to name the instructions", found[0].where)
	}
}

func TestScanCatchesAToolNamedInADescriptionThatIsGone(t *testing.T) {
	r := probe.Report{ToolsSupported: true, Tools: []mcp.Tool{
		{Name: "loadTaskContext", Description: "Then call startTaskBranch to open the branch"},
	}}
	found := scan(r)
	if len(found) != 1 || found[0].name != "startTaskBranch" {
		t.Fatalf("found = %+v", found)
	}
	if !strings.Contains(found[0].where, "loadTaskContext") {
		t.Errorf("where = %q, want it to name the description", found[0].where)
	}
}

// The exemptions exist so the instructions can point at a real tool in the sibling
// server. They are a hole in the check by design, which is exactly why they need a
// test: a name added here that is neither registered nor genuinely in jirrabit-mcp
// would pass silently.
func TestScanAllowsTheForeignTools(t *testing.T) {
	for name := range foreignTools {
		found := scan(report("call "+name+" in jirrabit-mcp", "loadTaskContext"))
		if len(found) != 0 {
			t.Errorf("%s is exempt but was reported: %+v", name, found)
		}
		if strings.TrimSpace(foreignTools[name]) == "" {
			t.Errorf("%s is exempt with no reason; the list is only maintainable if each entry says why", name)
		}
	}
}

// Only the listed names are exempt. Everything else is still caught.
func TestScanDoesNotExemptJustBecauseItLooksJiraShaped(t *testing.T) {
	for _, name := range []string{
		"deleteJiraIssue", "listJiraIssues", "getJiraAttachment", "createJiraIssue",
	} {
		found := scan(report("call "+name, "loadTaskContext"))
		if len(found) != 1 {
			t.Errorf("%s was not reported as a phantom: %+v", name, found)
		}
	}
}

// The pattern is deliberately loose, so it must also catch a plausible invention.
func TestToolNamePatternIsLooseEnough(t *testing.T) {
	for _, text := range []string{
		"call startTaskBranch",
		"startTaskBranch, then reportTaskProgress",
		"see loadTaskContext's description",
		"use listTaskChecks before runTaskChecks",
	} {
		if len(toolNamePattern.FindAllString(text, -1)) == 0 {
			t.Errorf("nothing matched in %q; the pattern is too tight to catch a real reference", text)
		}
	}
	// And it must not match ordinary prose.
	for _, text := range []string{
		"the working tree is clean",
		"read the comments first",
		"run the tests",
		"ask the user for the branch name",
	} {
		if got := toolNamePattern.FindAllString(text, -1); len(got) != 0 {
			t.Errorf("%q matched %v; ordinary prose must not read as a tool name", text, got)
		}
	}
}

func TestScanIsDeterministic(t *testing.T) {
	r := report("call zzzTool then aaaTool then mmmTool", "loadTaskContext")
	first := scan(r)
	for range 5 {
		next := scan(r)
		if len(next) != len(first) {
			t.Fatalf("phantom count changed between runs: %d vs %d", len(first), len(next))
		}
		for i := range first {
			if first[i].name != next[i].name {
				t.Fatalf("order changed between runs: %v vs %v", first, next)
			}
		}
	}
}

// A documentation file naming a tool that does not exist is the same failure as an
// instruction doing it: the reader copies the name into a client and gets "tool not
// found", with no way to tell a typo from a missing feature.
func TestScanFilesCatchesADocumentedToolThatIsGone(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := os.WriteFile("README.md", []byte("call startTaskBranch\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	found := scanFiles(report("", "loadTaskContext", "startTaskBranch"))
	if len(found) != 0 {
		t.Errorf("a registered tool was reported: %+v", found)
	}

	if err := os.WriteFile("README.md", []byte("call startTaskBranch and addMigrationNote\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	found = scanFiles(report("", "loadTaskContext", "startTaskBranch"))
	if len(found) != 1 || found[0].name != "addMigrationNote" {
		t.Fatalf("found = %+v, want addMigrationNote", found)
	}
	if !strings.Contains(found[0].where, "README.md") {
		t.Errorf("where = %q, want the file named", found[0].where)
	}
}

// A document that does not exist is not a failure: the command runs from the
// repository root and should not require every document to be present.
func TestScanFilesSkipsMissingDocuments(t *testing.T) {
	chdir(t, t.TempDir())
	if found := scanFiles(report("", "loadTaskContext")); len(found) != 0 {
		t.Errorf("found = %+v with no documents present", found)
	}
}

// The exemption list is a hole in the check, and a hole that grows by hand. Each
// entry must be a real tool in the sibling project, or it is hiding a typo.
func TestEveryForeignToolExistsInTheSiblingProject(t *testing.T) {
	repo := os.Getenv("JIRRABIT_MCP_REPO")
	if repo == "" {
		t.Skip("set JIRRABIT_MCP_REPO to a jirrabit-mcp checkout to check the exemptions against it")
	}
	entries, err := os.ReadDir(filepath.Join(repo, "pkg", "tools"))
	if err != nil {
		t.Fatalf("cannot read the sibling's tools: %v", err)
	}
	registered := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(repo, "pkg", "tools", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range siblingToolPattern.FindAllStringSubmatch(string(raw), -1) {
			registered[match[1]] = true
		}
	}
	for name := range foreignTools {
		if !registered[name] {
			t.Errorf("%s is exempt but jirrabit-mcp does not register it, so the exemption is hiding a typo",
				name)
		}
	}
}

var siblingToolPattern = regexp.MustCompile(`NewTool\("([A-Za-z0-9]+)"`)

func chdir(t *testing.T, dir string) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
}
