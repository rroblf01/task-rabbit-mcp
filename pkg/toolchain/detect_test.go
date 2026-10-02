package toolchain

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Each supported stack is a fixture. That is the only way a detection table
// stays true: a new language is a new directory here, and a rule that quietly
// stops firing shows up as a failing test rather than as a project whose lint
// mysteriously does not run.

func write(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root
}

func detect(t *testing.T, files map[string]string) *Toolchain {
	t.Helper()
	tc, err := Detect(write(t, files))
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	return tc
}

// roles maps a toolchain to "role=command line" for comparison in one go.
func roles(tc *Toolchain) map[string]string {
	out := map[string]string{}
	for _, check := range tc.Ordered() {
		out[string(check.Role)] = check.CommandLine()
	}
	return out
}

func assertRoles(t *testing.T, tc *Toolchain, want map[string]string) {
	t.Helper()
	got := roles(tc)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("roles differ\n got: %v\nwant: %v", got, want)
	}
}

func TestDetectMakefile(t *testing.T) {
	tc := detect(t, map[string]string{
		"Makefile": `SHELL := /bin/bash

.PHONY: test lint fmt build

test: ## run the tests
	go test ./...

lint:
	golangci-lint run

fmt:
	gofmt -w .

build:
	go build ./...
`,
	})

	assertRoles(t, tc, map[string]string{
		"format": "make fmt",
		"lint":   "make lint",
		"test":   "make test",
		"build":  "make build",
	})

	// `make fmt` conventionally rewrites files, and the agent is usually mid-edit.
	if !tc.Checks[RoleFormat].WriteMode {
		t.Error("make fmt was not marked as writing; it would rewrite the working tree unasked")
	}
	if tc.Checks[RoleLint].WriteMode {
		t.Error("make lint was marked as writing")
	}
}

// The rule that decides the whole design: parsing, not running. A makefile whose
// body has a $(shell) that deletes things must be safe to read.
func TestDetectMakefileNeverExecutesIt(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "executed")
	tc := detect(t, map[string]string{
		"Makefile": "MARKER := " + marker + "\n\nall:\n\t@touch " + marker + "\n",
	})

	if len(tc.Ordered()) > 0 && tc.Ordered()[0].Role != RoleCheck {
		t.Logf("roles: %v", roles(tc))
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("detection executed the makefile's recipe; the whole design depends on it not doing so")
	}
}

func TestDetectMakefileIgnoresRecipeLines(t *testing.T) {
	// A tab-indented line looks like a target to a regex that forgets about tabs,
	// and "go" would then become a runnable check.
	tc := detect(t, map[string]string{
		"Makefile": "test:\n\tgo test ./...\n\nbuild:\n\tgo build ./...\n",
	})
	for _, check := range tc.Ordered() {
		if check.Command[0] == "go" && len(tc.Checks[RoleLint].Command) > 0 &&
			check.CommandLine() == tc.Checks[RoleLint].CommandLine() {
			continue
		}
		if len(check.Command) > 0 && check.Command[0] == "go" && check.Role != RoleTest && check.Role != RoleBuild {
			t.Errorf("a recipe line was read as the %s target: %s", check.Role, check.CommandLine())
		}
	}
}

func TestDetectMakefileAggregateIsFlagged(t *testing.T) {
	tc := detect(t, map[string]string{
		"Makefile": "check: test lint\n\ntest:\n\techo t\n\nlint:\n\techo l\n",
	})
	aggregate, ok := tc.Checks[RoleCheck]
	if !ok {
		t.Fatal("no aggregate check detected")
	}
	// An aggregate's name never says what it runs, so the caller is told.
	if aggregate.Note == "" {
		t.Error("the aggregate target should say that it may run anything")
	}
}

func TestDetectPackageJSON(t *testing.T) {
	tc := detect(t, map[string]string{
		"package.json": `{
  "name": "web",
  "scripts": {
    "test": "vitest run",
    "lint": "eslint .",
    "format": "prettier --write .",
    "typecheck": "tsc --noEmit",
    "build": "vite build"
  }
}`,
		"pnpm-lock.yaml": "lockfileVersion: 9\n",
	})

	assertRoles(t, tc, map[string]string{
		"format":    "pnpm run format",
		"lint":      "pnpm run lint",
		"typecheck": "pnpm run typecheck",
		"test":      "pnpm run test",
		"build":     "pnpm run build",
	})
	if !tc.Checks[RoleFormat].WriteMode {
		t.Error("prettier --write should be marked as writing")
	}
	if tc.Checks[RoleTest].WriteMode {
		t.Error("vitest run should not be marked as writing")
	}
}

func TestDetectPackageJSONFormatCheckVariantDoesNotWrite(t *testing.T) {
	tc := detect(t, map[string]string{
		"package.json": `{"scripts": {"lint": "prettier --check ."}}`,
	})
	if tc.Checks[RoleLint].WriteMode {
		t.Error("prettier --check was marked as writing; it only reads")
	}
}

func TestDetectPythonWithUv(t *testing.T) {
	tc := detect(t, map[string]string{
		"pyproject.toml": `[project]
name = "thing"

[tool.ruff]
line-length = 100

[tool.mypy]
strict = true

[tool.pytest.ini_options]
testpaths = ["tests"]
`,
		"uv.lock": "version = 1\n",
	})

	// Every command goes through uv run, which is what finds the pinned ruff
	// instead of a global one of a different version.
	assertRoles(t, tc, map[string]string{
		"format":    "uv run ruff format --check .",
		"lint":      "uv run ruff check .",
		"typecheck": "uv run mypy .",
		"test":      "uv run pytest",
	})
}

// This repository's own project shape: Django with uv, configured ruff and ty, and
// tests through manage.py rather than pytest.
func TestDetectDjangoPrefersManagePyOverPytest(t *testing.T) {
	tc := detect(t, map[string]string{
		"pyproject.toml":  "[project]\nname = \"web\"\n\n[tool.ruff]\nline-length = 110\n\n[tool.ty.src]\n",
		"uv.lock":         "version = 1\n",
		"manage.py":       "#!/usr/bin/env python\nimport os\n",
		"tests/test_x.py": "def test_nothing():\n    pass\n",
	})

	// A `pytest` command that does not exist is worse than the one that does, and
	// both signals are present here.
	if got := tc.Checks[RoleTest].CommandLine(); got != "uv run python manage.py test" {
		t.Errorf("test = %q, want the Django runner", got)
	}
	if tc.Checks[RoleTest].Note == "" {
		t.Error("the Django test check should say a database engine may need setting")
	}
}

func TestDetectGo(t *testing.T) {
	tc := detect(t, map[string]string{
		"go.mod":  "module github.com/example/thing\n\ngo 1.27\n",
		"main.go": "package main\n",
	})

	assertRoles(t, tc, map[string]string{
		"format": "gofmt -l .",
		"lint":   "go vet ./...",
		"test":   "go test ./...",
		"build":  "go build ./...",
	})
	// gofmt -l exits 0 whether or not anything is unformatted. A caller who waits
	// for a non-zero exit will wait forever.
	if tc.Checks[RoleFormat].Note == "" {
		t.Error("the gofmt check must say its exit code is not the verdict")
	}
	if tc.Checks[RoleFormat].WriteMode {
		t.Error("gofmt -l was marked as writing; it only lists")
	}
}

func TestDetectRust(t *testing.T) {
	tc := detect(t, map[string]string{"Cargo.toml": "[package]\nname = \"x\"\n"})
	assertRoles(t, tc, map[string]string{
		"format": "cargo fmt --check",
		"lint":   "cargo clippy --all-targets",
		"test":   "cargo test",
		"build":  "cargo build",
	})
}

func TestDetectJVM(t *testing.T) {
	maven := detect(t, map[string]string{"mvnw": "#!/bin/sh\n"})
	if got := maven.Checks[RoleTest].CommandLine(); got != "./mvnw test" {
		t.Errorf("test = %q, want the wrapper", got)
	}
	gradle := detect(t, map[string]string{"gradlew": "#!/bin/sh\n"})
	if got := gradle.Checks[RoleTest].CommandLine(); got != "./gradlew test" {
		t.Errorf("test = %q", got)
	}
}

func TestOverrideFileWins(t *testing.T) {
	tc := detect(t, map[string]string{
		"Makefile": "test:\n\techo t\n",
		".task-rabbit.json": `{"checks": {
  "test": {"command": ["pytest", "-q"], "note": "faster subset"}
}}`,
	})
	if got := tc.Checks[RoleTest].CommandLine(); got != "pytest -q" {
		t.Errorf("test = %q; an operator's override file must win over a Makefile target", got)
	}
	if tc.Checks[RoleTest].Note != "faster subset" {
		t.Errorf("the override's note was lost: %+v", tc.Checks[RoleTest])
	}
	// The Makefile target is not silently dropped: a check that vanishes is a check
	// an agent believes does not exist.
	if len(tc.AlsoAvailable) == 0 {
		t.Error("the shadowed Makefile target was dropped instead of reported")
	}
}

func TestOverrideWriteModeMustBeDeclared(t *testing.T) {
	// Detection cannot know that `black` writes and `black --check` does not, so
	// the override file has to say so. It is not inferred from the words.
	tc := detect(t, map[string]string{
		".task-rabbit.json": `{"checks": {"format": {"command": ["black", "."]}}}`,
	})
	if tc.Checks[RoleFormat].WriteMode {
		t.Error("writeMode was guessed from the command text; it has to be declared")
	}
}

func TestMalformedOverrideIsReportedNotIgnoredSilently(t *testing.T) {
	tc := detect(t, map[string]string{".task-rabbit.json": "{not json"})
	found := false
	for _, signal := range tc.Signals {
		if contains(signal, ".task-rabbit.json") && contains(signal, "not valid JSON") {
			found = true
		}
	}
	if !found {
		t.Errorf("a file that looks like configuration but is not was ignored quietly; signals: %v", tc.Signals)
	}
}

// A repository with nothing recognisable is an answer, not a failure: "this
// project declares nothing I can run" is information an agent needs.
func TestDetectUnknownProjectIsEmptyNotAnError(t *testing.T) {
	tc := detect(t, map[string]string{"README.md": "# hi\n", "notes.txt": "x\n"})
	if len(tc.Ordered()) != 0 {
		t.Errorf("found checks in a project with no toolchain: %v", roles(tc))
	}
}

func TestDetectRefusesANonDirectory(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Detect(file); err == nil {
		t.Error("Detect accepted a file")
	}
	if _, err := Detect(filepath.Join(root, "nope")); err == nil {
		t.Error("Detect accepted a path that does not exist")
	}
}

func TestDetectIsDeterministic(t *testing.T) {
	files := map[string]string{
		"Makefile": "test:\n\techo t\n\nlint:\n\techo l\n",
		"go.mod":   "module x\n\ngo 1.27\n",
	}
	first := roles(detect(t, files))
	second := roles(detect(t, files))
	if !reflect.DeepEqual(first, second) {
		t.Errorf("two runs over the same repository disagree: %v vs %v", first, second)
	}
	// The order results come back in is part of the contract an agent reads:
	// the present roles, in AllRoles order, however many are missing.
	tc := detect(t, files)
	position := map[Role]int{}
	for i, role := range AllRoles {
		position[role] = i
	}
	previous := -1
	for _, check := range tc.Ordered() {
		if position[check.Role] <= previous {
			t.Errorf("%s came after a later role; results are %v", check.Role, roles(tc))
		}
		previous = position[check.Role]
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
