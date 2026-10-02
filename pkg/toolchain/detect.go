// Package toolchain works out what a project expects to be run against it.
//
// This is the part that makes one server usable across every repository: the
// agent asks for `lint`, and this package answers with whatever this project calls
// its linter. A server with a fixed command list works in one repository and
// nowhere else.
//
// The one hard rule: detection reads files and executes nothing. That is not
// caution for its own sake — the obvious way to ask a Makefile what it can do is
// `make -qp`, and `$(shell …)` in a makefile body runs during that query. The
// makefile is a file in a repository this server was pointed at, so parsing target
// names with a regex costs thirty lines and removes the hole completely. Same for
// package.json: read the scripts object, do not shell out to npm.
package toolchain

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Role is a category of check. Roles are fixed; commands are discovered. An agent
// that has to know a project's linter before it can ask for it will get it wrong.
type Role string

const (
	RoleFormat    Role = "format"
	RoleLint      Role = "lint"
	RoleTypecheck Role = "typecheck"
	RoleTest      Role = "test"
	RoleBuild     Role = "build"
	// RoleCheck is an aggregate: the project's own "run everything" target, when it
	// has one. It is a convenience and never a substitute for the individual
	// roles, because its name never says what it actually runs.
	RoleCheck Role = "check"
)

// AllRoles is the order results are reported in: cheapest and most specific
// first, the aggregate last.
var AllRoles = []Role{RoleFormat, RoleLint, RoleTypecheck, RoleTest, RoleBuild, RoleCheck}

// Check is one runnable thing, as an argument vector rather than a shell string.
//
// An argv is what makes `make test` safe to run without a shell: there is no
// string to be interpolated into, so a makefile target called `test; rm -rf ~` is
// a target that does not exist rather than a command that runs.
type Check struct {
	Role Role `json:"role"`
	// Command is argv[0] followed by its arguments. Run with exec directly.
	Command []string `json:"command"`
	// Source is what declared it, in a form an operator can check against the
	// repository: "Makefile target test", "package.json script lint".
	Source string `json:"source"`
	// WriteMode means running this rewrites files. Such a check is only run when
	// the caller asks for it, because the agent is usually mid-edit and a
	// formatter that runs unasked changes work in progress.
	WriteMode bool `json:"writeMode"`
	// NonEmptyOutputFails marks the checks whose verdict is not their exit code.
	//
	// `gofmt -l .` prints the files it would change and exits 0 either way, which
	// is the correct design for a tool meant to be read by a person and a trap for
	// anything that judges on exit status: a repository with nine unformatted files
	// would report "passed". With this set, output counts as the verdict.
	NonEmptyOutputFails bool `json:"nonEmptyOutputFails,omitempty"`
	// Note says anything an agent needs before running it.
	Note string `json:"note,omitempty"`
}

// CommandLine renders the argv for display. Never for execution: a rendered
// command containing a space is exactly the ambiguity an argv exists to remove.
func (c Check) CommandLine() string { return strings.Join(c.Command, " ") }

// Toolchain is what one repository declares.
type Toolchain struct {
	Root string
	// Checks is the winner per role, resolved by precedence.
	Checks map[Role]Check
	// AlsoAvailable are checks a project declares that lost to a higher-precedence
	// source for the same role. They are reported rather than dropped: a silently
	// discarded check is a check an agent believes does not exist.
	AlsoAvailable []Check
	// Signals are files that told us something without producing a runnable check.
	Signals []string
}

// Ordered returns the winning checks in a stable order.
func (t *Toolchain) Ordered() []Check {
	out := make([]Check, 0, len(t.Checks))
	for _, role := range AllRoles {
		if check, ok := t.Checks[role]; ok {
			out = append(out, check)
		}
	}
	return out
}

// Detect reads the repository at root.
//
// It never runs anything and never fails on a repository it does not recognise:
// an empty Toolchain with a signal saying so is a better answer than an error,
// because "this project declares nothing I can run" is information.
func Detect(root string) (*Toolchain, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &os.PathError{Op: "detect", Path: root, Err: os.ErrInvalid}
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}

	tc := &Toolchain{Root: absolute, Checks: map[Role]Check{}}
	finders := []func(*Toolchain){
		detectOverrides,
		detectMakefile,
		detectPackageJSON,
		detectPython,
		detectGo,
		detectRust,
		detectJVM,
		detectDotfiles,
	}
	for _, find := range finders {
		find(tc)
	}
	return tc, nil
}

// claim records a check for a role, or moves the newcomer to AlsoAvailable when a
// higher-precedence source already has that role.
//
// First claim wins, and the finders run in explicitness order: an operator's
// override file beats a Makefile target, which beats a language's own default.
// That is the order a person would use to decide which one is "really" the
// linter, so a project with both a `make lint` target and a `[tool.ruff]`
// configuration is answered with the target rather than whichever happened to be
// parsed last.
func (t *Toolchain) claim(check Check) {
	if _, taken := t.Checks[check.Role]; taken {
		// The newcomer loses; the incumbent moves to the reported list.
		t.AlsoAvailable = append(t.AlsoAvailable, check)
		return
	}
	t.Checks[check.Role] = check
}

func (t *Toolchain) signal(text string) {
	t.Signals = append(t.Signals, text)
}

func (t *Toolchain) exists(name string) bool {
	_, err := os.Stat(filepath.Join(t.Root, name))
	return err == nil
}

func (t *Toolchain) read(name string) ([]byte, bool) {
	raw, err := os.ReadFile(filepath.Join(t.Root, name))
	if err != nil {
		return nil, false
	}
	return raw, true
}

// --- .task-rabbit.json: the operator's escape hatch -------------------------

// overrideFile is the escape hatch for anything detection cannot guess.
//
// The shape is deliberately explicit about the one thing detection cannot know:
// whether a command rewrites files. Defaulting that to true would make
// `black` (which writes) and `black --check` (which does not) indistinguishable,
// and defaulting it to false would let a formatter rewrite the working tree
// because nobody mentioned it.
type overrideFile struct {
	Checks map[string]struct {
		Command   []string `json:"command"`
		WriteMode bool     `json:"writeMode"`
		Note      string   `json:"note"`
	} `json:"checks"`
}

func detectOverrides(t *Toolchain) {
	raw, ok := t.read(".task-rabbit.json")
	if !ok {
		return
	}
	var parsed overrideFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		// Malformed, and therefore ignored — but said out loud. A file that looks
		// like configuration and is not is worse than no file.
		t.signal(".task-rabbit.json exists but is not valid JSON, so it was ignored")
		return
	}
	if len(parsed.Checks) == 0 {
		t.signal(".task-rabbit.json declares no checks")
		return
	}
	for name, entry := range parsed.Checks {
		if len(entry.Command) == 0 {
			t.signal(".task-rabbit.json declares an empty command for " + name)
			continue
		}
		role := Role(strings.ToLower(strings.TrimSpace(name)))
		t.claim(Check{
			Role:      role,
			Command:   entry.Command,
			Source:    ".task-rabbit.json (" + name + ")",
			WriteMode: entry.WriteMode,
			Note:      entry.Note,
		})
	}
}

// --- Makefile ---------------------------------------------------------------

// makeTargetPattern matches a rule name at the start of a line, and captures what
// follows the colon.
//
// Go's regexp is RE2 and has no negative lookahead, so "not an assignment" cannot
// be expressed here: a variable assignment like `MARKER := /tmp/x` also has a
// colon followed by an equals sign, and `make MARKER` is not a check anybody wants.
// The remainder is captured instead and filtered in Go, which is clearer anyway.
var makeTargetPattern = regexp.MustCompile(`^([a-zA-Z0-9][a-zA-Z0-9_.\-/]*)[ \t]*:(.*)$`)

func detectMakefile(t *Toolchain) {
	var name string
	switch {
	case t.exists("Makefile"):
		name = "Makefile"
	case t.exists("makefile"):
		name = "makefile"
	default:
		return
	}
	raw, ok := t.read(name)
	if !ok {
		return
	}

	targets := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "\t") {
			// A recipe line, not a rule.
			continue
		}
		match := makeTargetPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		// `NAME := value` is a variable, not a target.
		if strings.HasPrefix(strings.TrimSpace(match[2]), "=") {
			continue
		}
		targets[match[1]] = true
	}
	if len(targets) == 0 {
		t.signal(name + " declares no targets this server can see")
		return
	}

	// Candidates in preference order. The first one present wins the role.
	candidates := map[Role][]string{
		RoleTest:      {"test", "tests", "check-test", "unittest", "spec"},
		RoleLint:      {"lint", "clippy", "ruff", "check-lint"},
		RoleTypecheck: {"typecheck", "types", "check-types"},
		RoleFormat:    {"format", "fmt", "format-check", "fmt-check"},
		RoleBuild:     {"build", "compile"},
		RoleCheck:     {"check", "ci", "verify", "all"},
	}

	for _, role := range AllRoles {
		for _, candidate := range candidates[role] {
			if !targets[candidate] {
				continue
			}
			check := Check{Role: role, Command: []string{"make", candidate}, Source: name + " target " + candidate}
			if role == RoleFormat && !targets[string(role)+"-check"] {
				// `make fmt` conventionally rewrites files, and the only way to know
				// for sure is to read its recipe — which is where the commands
				// live, so they are parsed for the flag rather than executed.
				check.WriteMode = true
				check.Note = "make " + candidate + " rewrites files in place; it is only run when the " +
					"caller asks to fix"
			}
			if role == RoleCheck {
				check.Note = "the project's aggregate target; it may run anything, including the other " +
					"checks, so prefer running the individual roles when you want to know what failed"
			}
			t.claim(check)
			break
		}
	}
	t.signal(name + " declares " + strconv.Itoa(len(targets)) + " targets")
}

// --- package.json -----------------------------------------------------------

func detectPackageJSON(t *Toolchain) {
	raw, ok := t.read("package.json")
	if !ok {
		return
	}
	var parsed struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Scripts) == 0 {
		t.signal("package.json has no readable scripts")
		return
	}

	// The package manager is decided by the lockfile, because `npm run test` in a
	// pnpm project installs a second copy of everything and can run a different
	// test command than the project's own.
	runner := "npm"
	switch {
	case t.exists("pnpm-lock.yaml"):
		runner = "pnpm"
	case t.exists("yarn.lock"):
		runner = "yarn"
	case t.exists("bun.lockb") || t.exists("bun.lock"):
		runner = "bun"
	}
	if runner != "npm" {
		t.signal("package manager is " + runner + ", from its lockfile")
	}

	candidates := map[Role][]string{
		RoleTest:      {"test", "test:unit", "jest", "vitest"},
		RoleLint:      {"lint", "eslint", "lint:js"},
		RoleTypecheck: {"typecheck", "type-check", "types", "tsc"},
		RoleFormat:    {"format", "fmt", "prettier"},
		RoleBuild:     {"build"},
		RoleCheck:     {"check", "verify", "ci"},
	}

	for _, role := range AllRoles {
		for _, name := range candidates[role] {
			script, present := parsed.Scripts[name]
			if !present {
				continue
			}
			t.claim(Check{
				Role:    role,
				Command: []string{runner, "run", name},
				Source:  "package.json script " + name,
				// The script's own text is read, never executed, which is enough to
				// tell `prettier --write` from `prettier --check`.
				WriteMode: writesFiles(script),
			})
			break
		}
	}
}

// writesFiles reports whether a package.json script rewrites files.
//
// A keyword search rather than real parsing, and deliberately narrow: the cost of
// a false negative is that the caller has to ask twice for a format, and the cost
// of a false positive is that a check refuses to run unasked.
func writesFiles(script string) bool {
	lowered := strings.ToLower(script)
	if strings.Contains(lowered, "--check") || strings.Contains(lowered, "--dry-run") {
		return false
	}
	for _, keyword := range []string{"--write", "--fix", "format:write", "prettier"} {
		if strings.Contains(lowered, keyword) {
			return true
		}
	}
	return false
}

// --- python -----------------------------------------------------------------

// pythonSections are the pyproject.toml tables that mean a tool is configured.
var pythonSections = []string{
	"[tool.ruff", "[tool.mypy]", "[tool.ty]", "[tool.pytest", "[tool.black]",
	"[tool.isort]", "[tool.pdm", "[tool.hatch",
}

func detectPython(t *Toolchain) {
	raw, hasPyproject := t.read("pyproject.toml")
	pyproject := string(raw)
	// sectioned reports whether pyproject.toml configures a tool. The comparison is
	// on the table header only, with its newline: "[tool.mypy]" must not match a
	// "[tool.mypy.overrides]" table, and matching on the prefix alone would.
	sectioned := func(prefix string) bool {
		if !hasPyproject {
			return false
		}
		return strings.HasPrefix(pyproject, prefix) ||
			strings.Contains(pyproject, "\n"+prefix)
	}

	// uv run is a prefix rather than a replacement: it puts the project's own
	// virtualenv on PATH, which is what makes `uv run ruff` find the pinned ruff
	// instead of whatever is installed globally.
	prefix := []string{}
	if t.exists("uv.lock") {
		prefix = []string{"uv", "run"}
		t.signal("uv.lock present, so commands run through `uv run`")
	} else if t.exists(".venv") || t.exists("venv") {
		t.signal("a virtualenv directory is present; commands assume its bin is on PATH")
	}
	command := func(name ...string) []string {
		return append(append([]string{}, prefix...), name...)
	}

	ruff := sectioned("[tool.ruff") || t.exists("ruff.toml") || t.exists(".ruff.toml")
	switch {
	case ruff:
		// --check first: `ruff format` writes, and detection should offer the
		// reading variant by default.
		t.claim(Check{Role: RoleFormat, Command: command("ruff", "format", "--check", "."),
			Source: "ruff configuration"})
		t.claim(Check{Role: RoleLint, Command: command("ruff", "check", "."),
			Source: "ruff configuration"})
	case sectioned("[tool.black"):
		t.claim(Check{Role: RoleFormat, Command: command("black", "--check", "."),
			Source: "pyproject.toml [tool.black]"})
		t.claim(Check{Role: RoleLint, Command: command("black", "--check", "."),
			Source: "pyproject.toml [tool.black]"})
	}

	if sectioned("[tool.mypy]") || t.exists("mypy.ini") || t.exists(".mypy.ini") {
		t.claim(Check{Role: RoleTypecheck, Command: command("mypy", "."),
			Source: "mypy configuration"})
	}
	// ty is the type checker this very repository uses, and it configures itself in
	// pyproject.toml like any other.
	if sectioned("[tool.ty") {
		t.claim(Check{Role: RoleTypecheck, Command: command("ty", "check", "."),
			Source: "pyproject.toml [tool.ty]"})
	}
	if sectioned("[tool.isort]") {
		t.claim(Check{Role: RoleFormat, Command: command("isort", "--check-only", "."),
			Source: "pyproject.toml [tool.isort]"})
	}

	// Django before pytest: a project with manage.py runs its tests through Django,
	// and a `pytest` command that does not exist is worse than the one that does.
	switch {
	case t.exists("manage.py"):
		t.claim(Check{Role: RoleTest, Command: command("python", "manage.py", "test"),
			Source: "manage.py (Django)",
			Note: "run it with a database engine the test settings understand; a project that sets " +
				"one through an environment variable needs it set here too"})
	case sectioned("[tool.pytest") || t.exists("pytest.ini") || t.exists("tox.ini") ||
		t.exists("conftest.py"):
		t.claim(Check{Role: RoleTest, Command: command("pytest"), Source: "pytest configuration"})
	}

	if t.exists("noxfile.py") {
		t.signal("noxfile.py present; its sessions are not parsed, so declare them in .task-rabbit.json " +
			"if they are what you want run")
	}
	for _, section := range pythonSections {
		if sectioned(section) {
			t.signal("pyproject.toml configures " + strings.TrimPrefix(section, "[tool."))
			break
		}
	}
}

// --- go ---------------------------------------------------------------------

func detectGo(t *Toolchain) {
	raw, ok := t.read("go.mod")
	if !ok {
		return
	}
	module := modulePath(string(raw))
	// gofmt -l lists files and exits 0 whether or not anything is unformatted, so
	// its exit code says nothing; the caller reads stdout. Said here so nobody waits
	// for a non-zero exit that will not come.
	t.claim(Check{Role: RoleFormat, Command: []string{"gofmt", "-l", "."},
		Source: "go.mod (gofmt -l " + module + "/...)",
		Note: "gofmt lists the files it would change and exits 0 either way, so the verdict is the output, " +
			"not the exit code",
		NonEmptyOutputFails: true})
	t.claim(Check{Role: RoleLint, Command: []string{"go", "vet", "./..."}, Source: "go.mod"})
	t.claim(Check{Role: RoleTest, Command: []string{"go", "test", "./..."}, Source: "go.mod"})
	t.claim(Check{Role: RoleBuild, Command: []string{"go", "build", "./..."}, Source: "go.mod"})
}

var modulePattern = regexp.MustCompile(`(?m)^module\s+(\S+)`)

func modulePath(goMod string) string {
	if match := modulePattern.FindStringSubmatch(goMod); match != nil {
		return match[1]
	}
	return "the module"
}

// --- rust -------------------------------------------------------------------

func detectRust(t *Toolchain) {
	if !t.exists("Cargo.toml") {
		return
	}
	t.claim(Check{Role: RoleFormat, Command: []string{"cargo", "fmt", "--check"},
		Source: "Cargo.toml"})
	t.claim(Check{Role: RoleLint, Command: []string{"cargo", "clippy", "--all-targets"},
		Source: "Cargo.toml"})
	t.claim(Check{Role: RoleTest, Command: []string{"cargo", "test"}, Source: "Cargo.toml"})
	t.claim(Check{Role: RoleBuild, Command: []string{"cargo", "build"}, Source: "Cargo.toml"})
}

// --- jvm --------------------------------------------------------------------

func detectJVM(t *Toolchain) {
	switch {
	case t.exists("mvnw"):
		t.claim(Check{Role: RoleTest, Command: []string{"./mvnw", "test"},
			Source: "mvnw wrapper",
			Note:   "run it as ./mvnw so the wrapper's own Maven version is used"})
		t.claim(Check{Role: RoleBuild, Command: []string{"./mvnw", "-q", "package"},
			Source: "mvnw wrapper"})
	case t.exists("gradlew"):
		t.claim(Check{Role: RoleTest, Command: []string{"./gradlew", "test"},
			Source: "gradlew wrapper"})
		t.claim(Check{Role: RoleBuild, Command: []string{"./gradlew", "build"},
			Source: "gradlew wrapper"})
	case t.exists("pom.xml"):
		t.claim(Check{Role: RoleTest, Command: []string{"mvn", "test"}, Source: "pom.xml"})
	case t.exists("build.gradle") || t.exists("build.gradle.kts"):
		t.claim(Check{Role: RoleTest, Command: []string{"gradle", "test"}, Source: "build.gradle"})
	}
}

// --- signals ----------------------------------------------------------------

func detectDotfiles(t *Toolchain) {
	if t.exists(".pre-commit-config.yaml") {
		// Named as a signal rather than a check: its hooks are the project's own
		// definition of what to run, and parsing that faithfully is a bigger job
		// than reading a scripts object.
		t.signal(".pre-commit-config.yaml present; its hooks are not run by this server — " +
			"declare them in .task-rabbit.json if you want them")
	}
}
