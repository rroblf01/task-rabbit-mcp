package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRepo builds a real repository in a temp directory.
//
// Real git rather than a mock: the preconditions this package enforces are git's
// rules, and a mock would only assert that this package calls what it thinks git
// calls. That distinction is the whole reason git check-ref-format is consulted
// instead of a regular expression.
func newRepo(t *testing.T, remote bool) string {
	t.Helper()
	dir := t.TempDir()

	git(t, dir, "init", "-q", "-b", "main")
	// A commit needs an identity, and CI has none by default.
	git(t, dir, "config", "user.email", "test@example.invalid")
	git(t, dir, "config", "user.name", "Test")

	write(t, dir, "README.md", "hello\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "first")

	if !remote {
		return dir
	}

	// A bare "origin", then a clone of it. Cloning rather than pushing matters:
	// `git push -u` creates the remote-tracking branch but not origin/HEAD, so a
	// fixture built that way cannot test the default-branch detection at all — and
	// the clone is the realistic setup anyway, since that is how a developer gets a
	// checkout.
	bare := filepath.Join(t.TempDir(), "origin.git")
	git(t, filepath.Dir(bare), "clone", "-q", "--bare", dir, bare)
	clone := filepath.Join(t.TempDir(), "work")
	git(t, filepath.Dir(clone), "clone", "-q", bare, clone)
	git(t, clone, "config", "user.email", "test@example.invalid")
	git(t, clone, "config", "user.name", "Test")
	return clone
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestInspectReadsACleanRepository(t *testing.T) {
	dir := newRepo(t, true)
	state, err := Inspect(context.Background(), dir)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}

	if !state.Available || state.GitVersion == "" {
		t.Errorf("git availability not reported: %+v", state)
	}
	if state.Branch != "main" {
		t.Errorf("branch = %q, want main", state.Branch)
	}
	if !state.Clean {
		t.Errorf("a fresh repository reported changes: %v", state.Changes)
	}
	if state.Detached {
		t.Error("a fresh repository reported a detached HEAD")
	}
	if len(state.Remotes) != 1 || state.Remotes[0].Name != "origin" {
		t.Errorf("remotes = %+v", state.Remotes)
	}
	if state.DefaultBranch != "main" {
		t.Errorf("defaultBranch = %q, want main", state.DefaultBranch)
	}
	if state.Upstream != "origin/main" {
		t.Errorf("upstream = %q, want origin/main", state.Upstream)
	}
	// The base a caller would branch from.
	if got := state.BaseRef(); got != "origin/main" {
		t.Errorf("BaseRef() = %q, want origin/main", got)
	}
}

func TestInspectReportsWhatIsDirty(t *testing.T) {
	dir := newRepo(t, false)
	write(t, dir, "new-file.txt", "x\n")

	state, err := Inspect(context.Background(), dir)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if state.Clean {
		t.Error("an untracked file was not reported")
	}
	// The refusal has to say what is in the way, or the user is left guessing.
	if len(state.Changes) == 0 || !strings.Contains(state.Changes[0], "new-file.txt") {
		t.Errorf("changes = %v, want the untracked file named", state.Changes)
	}
}

// A dirty tree is a refusal, not a warning. Stashing, or branching from whatever
// happens to be in the tree, silently attaches unrelated work to the task's branch.
func TestCreateBranchRefusesADirtyTree(t *testing.T) {
	dir := newRepo(t, true)
	write(t, dir, "in-progress.txt", "half done\n")

	err := CreateBranch(context.Background(), dir, "task-x", "origin/main")

	var dirty *ErrDirtyTree
	if !asErr(err, &dirty) {
		t.Fatalf("expected a dirty-tree refusal, got %v", err)
	}
	if !strings.Contains(err.Error(), "in-progress.txt") {
		t.Errorf("the refusal must name the files: %v", err)
	}
	// And nothing may have happened.
	state, _ := Inspect(context.Background(), dir)
	if state.Branch != "main" {
		t.Errorf("the branch changed anyway: %q", state.Branch)
	}
}

func TestCreateBranchFromARemoteBranch(t *testing.T) {
	dir := newRepo(t, true)

	if err := CreateBranch(context.Background(), dir, "demo-42-fix-login", "origin/main"); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}

	state, _ := Inspect(context.Background(), dir)
	if state.Branch != "demo-42-fix-login" {
		t.Errorf("branch = %q", state.Branch)
	}
	if !state.Clean {
		t.Errorf("a new branch is not clean: %v", state.Changes)
	}
	// Created from origin/main, so it holds that commit and not anything local.
	head := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
	remote := strings.TrimSpace(git(t, dir, "rev-parse", "origin/main"))
	if head != remote {
		t.Errorf("the branch does not point at origin/main: %s vs %s", head, remote)
	}
}

func TestCreateBranchRefusesAnExistingBranch(t *testing.T) {
	dir := newRepo(t, true)
	git(t, dir, "branch", "already-here")

	err := CreateBranch(context.Background(), dir, "already-here", "origin/main")
	var exists *ErrBranchExists
	if !asErr(err, &exists) {
		t.Fatalf("expected an exists refusal, got %v", err)
	}
}

func TestCreateBranchRefusesAMissingBase(t *testing.T) {
	dir := newRepo(t, true)

	err := CreateBranch(context.Background(), dir, "task-x", "origin/nope")
	var missing *ErrBaseMissing
	if !asErr(err, &missing) {
		t.Fatalf("expected a missing-ref refusal, got %v", err)
	}
	if !strings.Contains(err.Error(), "origin/nope") {
		t.Errorf("the refusal must name the ref: %v", err)
	}
}

// Every one of these is refused by git itself. A regex that accepted any of them
// would fail later, in a confusing place.
func TestCheckBranchNameDefersToGit(t *testing.T) {
	dir := newRepo(t, false)

	refused := []string{
		"", "with space", "with~tilde", "with^caret", "with:colon", "with?question",
		"with*star", "with[bracket", "with\\backslash", "double..dots", "@{ref}",
		"ends-with-dot.", ".starts-with-dot", "-starts-with-dash",
		// HEAD is a file, not a name, and git says so itself.
		"HEAD",
	}
	for _, name := range refused {
		if err := CheckBranchName(context.Background(), dir, name); err == nil {
			t.Errorf("git would refuse %q but this accepted it", name)
		}
	}

	accepted := []string{
		"demo-42-fix-login", "feature/JIRA-123_login", "fix-2026-10-02",
		"wip_login-timeout_v2", "release-1.2.3",
		// `refs/heads/nope` and `@` are accepted in --branch mode, which is why this
		// package takes git's answer rather than a rule of its own.
		"refs/heads/nope",
		// A 300 character name is accepted by `check-ref-format --branch`, which is
		// worth pinning: this package defers to git rather than imposing a limit of
		// its own, and a test that assumed otherwise would have been asserting a rule
		// that does not exist. A host with a shorter limit still refuses it later,
		// which is the host's business.
		strings.Repeat("x", 300),
	}
	for _, name := range accepted {
		if err := CheckBranchName(context.Background(), dir, name); err != nil {
			t.Errorf("%q is a valid branch name and was refused: %v", name, err)
		}
	}
}

func TestInspectRefusesANonRepository(t *testing.T) {
	dir := t.TempDir()
	_, err := Inspect(context.Background(), dir)
	if err == nil {
		t.Fatal("a plain directory was reported as a repository")
	}
	// The message has to say what is wrong, or the caller checks the wrong thing.
	if !strings.Contains(err.Error(), "not inside a git working tree") {
		t.Errorf("error = %v", err)
	}
}

func TestInspectReportsADetachedHead(t *testing.T) {
	dir := newRepo(t, false)
	head := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
	git(t, dir, "checkout", "-q", head)

	state, err := Inspect(context.Background(), dir)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !state.Detached || state.Branch != "" {
		t.Errorf("a detached HEAD reported branch %q (detached=%v)", state.Branch, state.Detached)
	}
	// Branching from "HEAD" would be a confusing base, so BaseRef says nothing.
	if got := state.BaseRef(); got != "" {
		t.Errorf("BaseRef() on a detached HEAD = %q, want nothing", got)
	}
}

func TestInspectCountsAheadAndBehind(t *testing.T) {
	dir := newRepo(t, true)
	write(t, dir, "second.txt", "x\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "local work")

	state, _ := Inspect(context.Background(), dir)
	if state.Ahead != 1 || state.Behind != 0 {
		t.Errorf("ahead/behind = %d/%d, want 1/0", state.Ahead, state.Behind)
	}
}

func TestResolveRepo(t *testing.T) {
	dir := t.TempDir()
	resolved, err := ResolveRepo(dir)
	if err != nil {
		t.Fatalf("ResolveRepo: %v", err)
	}
	if !filepath.IsAbs(resolved) {
		t.Errorf("ResolveRepo returned a relative path: %q", resolved)
	}

	if _, err := ResolveRepo(""); err == nil {
		t.Error("an empty path was accepted")
	}
	if _, err := ResolveRepo(filepath.Join(dir, "nope")); err == nil {
		t.Error("a missing path was accepted")
	}
	file := filepath.Join(dir, "a-file")
	write(t, dir, "a-file", "x")
	if _, err := ResolveRepo(file); err == nil {
		t.Error("a file was accepted as a repository")
	}
}

// asErr is errors.As without importing errors into every assertion.
func asErr[T error](err error, target *T) bool {
	for err != nil {
		if typed, ok := err.(T); ok {
			*target = typed
			return true
		}
		unwrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapped.Unwrap()
	}
	return false
}
