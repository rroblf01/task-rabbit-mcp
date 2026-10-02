package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rroblf01/task-rabbit-mcp/pkg/confirm"
	"github.com/rroblf01/task-rabbit-mcp/pkg/repos"
)

// A real repository, because the preconditions these tools enforce are git's rules
// and the handshake is signed over what git reports.
func repoWithOrigin(t *testing.T) string {
	t.Helper()
	source := t.TempDir()
	gitCmd(t, source, "init", "-q", "-b", "main")
	gitCmd(t, source, "config", "user.email", "test@example.invalid")
	gitCmd(t, source, "config", "user.name", "Test")
	writeFile(t, source, "README.md", "hello\n")
	gitCmd(t, source, "add", ".")
	gitCmd(t, source, "commit", "-q", "-m", "first")

	bare := filepath.Join(t.TempDir(), "origin.git")
	gitCmd(t, filepath.Dir(bare), "clone", "-q", "--bare", source, bare)

	clone := filepath.Join(t.TempDir(), "work")
	gitCmd(t, filepath.Dir(clone), "clone", "-q", bare, clone)
	gitCmd(t, clone, "config", "user.email", "test@example.invalid")
	gitCmd(t, clone, "config", "user.name", "Test")
	// A Go module, so the fixture is a project and not just a repository: detection
	// reads the working tree, and a bare clone with one README declares no checks.
	writeFile(t, clone, "go.mod", "module example.com/task\n\ngo 1.27\n")
	gitCmd(t, clone, "add", ".")
	gitCmd(t, clone, "commit", "-q", "-m", "add a module")
	return clone
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestInspectTaskRepoReportsAReadyRepository(t *testing.T) {
	repo := repoWithOrigin(t)
	fake := newFakeJira(t)
	d := fake.deps()

	var out repoReport
	call(t, "inspectTaskRepo", d.inspectTaskRepo, map[string]any{
		"repoPath":    repo,
		"instanceUrl": addressable(t, fake.server.URL),
		"apiKey":      "k",
	}, &out)

	if !out.Ready {
		t.Fatalf("a fresh clone reported not ready: %+v", out.Blockers)
	}
	if out.Git == nil || out.Git.Branch != "main" {
		t.Fatalf("git state = %+v", out.Git)
	}
	if out.Git.DefaultBranch != "main" {
		t.Errorf("defaultBranch = %q", out.Git.DefaultBranch)
	}
	if len(out.Checks) == 0 {
		t.Error("no checks reported for a Go repository")
	}
	// The one question that cannot be answered by anything but the user.
	if !mentionsBranchName(out.Questions) {
		t.Errorf("questions = %v, want one about the branch name", out.Questions)
	}
}

func TestInspectTaskRepoRefusesADirtyTreeAndNamesTheFiles(t *testing.T) {
	repo := repoWithOrigin(t)
	writeFile(t, repo, "half-done.txt", "x\n")
	fake := newFakeJira(t)
	d := fake.deps()

	var out repoReport
	call(t, "inspectTaskRepo", d.inspectTaskRepo, map[string]any{
		"repoPath":    repo,
		"instanceUrl": addressable(t, fake.server.URL),
		"apiKey":      "k",
	}, &out)

	if out.Ready {
		t.Error("a dirty working tree reported ready")
	}
	joined := strings.Join(out.Blockers, "; ")
	if !strings.Contains(joined, "half-done.txt") {
		t.Errorf("the blocker must name the files: %v", out.Blockers)
	}
}

// The handshake. Each of these is the property that makes it more than a delay.
func TestStartTaskBranchTakesTwoSteps(t *testing.T) {
	repo := repoWithOrigin(t)
	fake := newFakeJira(t)
	d := fake.deps()
	instance := map[string]any{"instanceUrl": addressable(t, fake.server.URL), "apiKey": "k"}

	t.Run("the first call changes nothing", func(t *testing.T) {
		var preview confirm.Preview
		call(t, "startTaskBranch", d.startTaskBranch, withArgs(instance, map[string]any{
			"repoPath": repo, "branchName": "demo-42-fix-login", "baseRef": "origin/main",
		}), &preview)

		if preview.Confirmed {
			t.Error("the first call created the branch")
		}
		if preview.ConfirmationToken == "" {
			t.Fatal("no token to confirm with")
		}
		if !preview.Reversible || preview.Undo == "" {
			t.Error("the preview must say whether this can be taken back")
		}
		// And nothing happened on disk.
		if branch := currentBranch(t, repo); branch != "main" {
			t.Fatalf("the branch is %q; the first call must change nothing", branch)
		}
	})

	t.Run("a valid token creates it", func(t *testing.T) {
		var first confirm.Preview
		call(t, "startTaskBranch", d.startTaskBranch, withArgs(instance, map[string]any{
			"repoPath": repo, "branchName": "demo-42-fix-login", "baseRef": "origin/main",
		}), &first)

		var second confirm.Preview
		call(t, "startTaskBranch", d.startTaskBranch, withArgs(instance, map[string]any{
			"repoPath": repo, "branchName": "demo-42-fix-login", "baseRef": "origin/main",
			"confirm": first.ConfirmationToken,
		}), &second)

		if !second.Confirmed {
			t.Fatalf("the confirmed call did not confirm: %+v", second)
		}
		if branch := currentBranch(t, repo); branch != "demo-42-fix-login" {
			t.Errorf("branch = %q", branch)
		}
	})

	t.Run("a token cannot create the branch twice", func(t *testing.T) {
		other := repoWithOrigin(t)
		var first confirm.Preview
		call(t, "startTaskBranch", d.startTaskBranch, withArgs(instance, map[string]any{
			"repoPath": other, "branchName": "task-one", "baseRef": "origin/main",
		}), &first)

		call(t, "startTaskBranch", d.startTaskBranch, withArgs(instance, map[string]any{
			"repoPath": other, "branchName": "task-one", "baseRef": "origin/main",
			"confirm": first.ConfirmationToken,
		}), nil)

		// Replaying the same token now hits an existing branch, and the signature
		// over the preview no longer matches what a second preview would produce.
		msg := callExpectingError(t, "startTaskBranch", d.startTaskBranch, withArgs(instance, map[string]any{
			"repoPath": other, "branchName": "task-one", "baseRef": "origin/main",
			"confirm": first.ConfirmationToken,
		}))
		if msg == "" {
			t.Fatal("a replayed token was accepted")
		}
	})
}

func TestStartTaskBranchRefusesADirtyTree(t *testing.T) {
	repo := repoWithOrigin(t)
	writeFile(t, repo, "uncommitted.txt", "x\n")
	fake := newFakeJira(t)
	d := fake.deps()
	instance := map[string]any{"instanceUrl": addressable(t, fake.server.URL), "apiKey": "k"}

	var first confirm.Preview
	call(t, "startTaskBranch", d.startTaskBranch, withArgs(instance, map[string]any{
		"repoPath": repo, "branchName": "task-x", "baseRef": "origin/main",
	}), &first)

	if len(first.Blockers) == 0 {
		t.Fatal("a dirty tree was not a blocker")
	}
	if !strings.Contains(strings.Join(first.Blockers, "; "), "uncommitted.txt") {
		t.Errorf("the blocker must name the files: %v", first.Blockers)
	}

	// Even with a token, a dirty tree is a refusal. The fix is the user's, not
	// another call.
	msg := callExpectingError(t, "startTaskBranch", d.startTaskBranch, withArgs(instance, map[string]any{
		"repoPath": repo, "branchName": "task-x", "baseRef": "origin/main",
		"confirm": first.ConfirmationToken,
	}))
	if !strings.Contains(msg, "uncommitted.txt") {
		t.Errorf("the refusal lost the reason: %s", msg)
	}
	if branch := currentBranch(t, repo); branch != "main" {
		t.Errorf("the branch changed anyway: %q", branch)
	}
}

// The repository changing between the two calls is the whole reason the token is
// signed over the preview rather than merely delayed.
func TestStartTaskBranchRefusesWhenTheRepositoryChanged(t *testing.T) {
	repo := repoWithOrigin(t)
	fake := newFakeJira(t)
	d := fake.deps()
	instance := map[string]any{"instanceUrl": addressable(t, fake.server.URL), "apiKey": "k"}

	var first confirm.Preview
	call(t, "startTaskBranch", d.startTaskBranch, withArgs(instance, map[string]any{
		"repoPath": repo, "branchName": "task-x", "baseRef": "origin/main",
	}), &first)

	// Something appears in the working tree after the user was shown the preview.
	writeFile(t, repo, "appeared.txt", "x\n")

	msg := callExpectingError(t, "startTaskBranch", d.startTaskBranch, withArgs(instance, map[string]any{
		"repoPath": repo, "branchName": "task-x", "baseRef": "origin/main",
		"confirm": first.ConfirmationToken,
	}))
	if !strings.Contains(msg, "uncommitted") {
		t.Errorf("the refusal should say what is in the way: %s", msg)
	}
	if branch := currentBranch(t, repo); branch != "main" {
		t.Errorf("the branch changed anyway: %q", branch)
	}
}

func TestStartTaskBranchAsksForANameRatherThanInventingOne(t *testing.T) {
	repo := repoWithOrigin(t)
	fake := newFakeJira(t)
	d := fake.deps()

	var preview confirm.Preview
	call(t, "startTaskBranch", d.startTaskBranch, map[string]any{
		"repoPath":    repo,
		"baseRef":     "origin/main",
		"instanceUrl": addressable(t, fake.server.URL),
		"apiKey":      "k",
	}, &preview)

	if len(preview.Questions) == 0 {
		t.Error("no question about the missing branch name")
	}
	if len(preview.Blockers) == 0 {
		t.Error("a missing name is not a blocker, so a confirm could proceed with no name")
	}
	if branch := currentBranch(t, repo); branch != "main" {
		t.Errorf("branch = %q", branch)
	}
}

func TestStartTaskBranchDefaultsTheBaseToTheUpstream(t *testing.T) {
	repo := repoWithOrigin(t)
	fake := newFakeJira(t)
	d := fake.deps()
	instance := map[string]any{"instanceUrl": addressable(t, fake.server.URL), "apiKey": "k"}

	var first confirm.Preview
	call(t, "startTaskBranch", d.startTaskBranch, withArgs(instance, map[string]any{
		"repoPath": repo, "branchName": "task-x",
	}), &first)

	base, _ := first.Detail["baseRef"].(string)
	if base != "origin/main" {
		t.Errorf("baseRef defaulted to %q, want the current branch's upstream", base)
	}

	var second confirm.Preview
	call(t, "startTaskBranch", d.startTaskBranch, withArgs(instance, map[string]any{
		"repoPath": repo, "branchName": "task-x", "confirm": first.ConfirmationToken,
	}), &second)
	if !second.Confirmed {
		t.Errorf("the confirmed call failed: %+v", second)
	}
}

// A token issued for one repository must not create a branch in another.
func TestStartTaskBranchTokenIsPinnedToItsRepository(t *testing.T) {
	first := repoWithOrigin(t)
	second := repoWithOrigin(t)
	fake := newFakeJira(t)
	d := fake.deps()
	instance := map[string]any{"instanceUrl": addressable(t, fake.server.URL), "apiKey": "k"}

	var preview confirm.Preview
	call(t, "startTaskBranch", d.startTaskBranch, withArgs(instance, map[string]any{
		"repoPath": first, "branchName": "task-x", "baseRef": "origin/main",
	}), &preview)

	callExpectingError(t, "startTaskBranch", d.startTaskBranch, withArgs(instance, map[string]any{
		"repoPath": second, "branchName": "task-x", "baseRef": "origin/main",
		"confirm": preview.ConfirmationToken,
	}))

	if branch := currentBranch(t, second); branch != "main" {
		t.Errorf("a token from another repository created a branch here: %q", branch)
	}
}

func TestInspectTaskRepoPreviewsTheSuggestedBranch(t *testing.T) {
	repo := repoWithOrigin(t)
	fake := newFakeJira(t)
	d := fake.deps()

	var out repoReport
	call(t, "inspectTaskRepo", d.inspectTaskRepo, map[string]any{
		"repoPath":        repo,
		"suggestedBranch": "demo-42-fix-login",
		"instanceUrl":     addressable(t, fake.server.URL),
		"apiKey":          "k",
	}, &out)

	if out.Preview == nil {
		t.Fatal("no preview for a suggested branch")
	}
	if out.Preview.ConfirmationToken == "" {
		t.Error("the preview carries no token, so the agent cannot show a plan it can then act on")
	}
	// With a branch named, the naming question is answered.
	if mentionsBranchName(out.Questions) {
		t.Errorf("still asking for a name after one was suggested: %v", out.Questions)
	}
}

func TestResolveRepoPrefersTheExplicitPath(t *testing.T) {
	mapped := repoWithOrigin(t)
	other := repoWithOrigin(t)
	index, err := repos.Parse(`{"DEMO":"` + mapped + `"}`)
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Repos: index}

	got, err := d.resolveRepo("DEMO", "")
	if err != nil {
		t.Fatalf("resolveRepo: %v", err)
	}
	if got.Path != mapped || got.How != "TASK_RABBIT_REPOS" {
		t.Errorf("resolveRepo(project) = %+v", got)
	}

	got, err = d.resolveRepo("DEMO", other)
	if err != nil {
		t.Fatalf("resolveRepo: %v", err)
	}
	if got.Path != other || got.How != "repoPath" {
		t.Errorf("repoPath must win over the mapping: %+v", got)
	}
}

func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(gitCmd(t, dir, "rev-parse", "--abbrev-ref", "HEAD"))
}

func mentionsBranchName(questions []string) bool {
	for _, question := range questions {
		if strings.Contains(question, "branch be called") {
			return true
		}
	}
	return false
}

// withArgs merges the jirrabit credentials into a call's arguments.
func withArgs(shared, own map[string]any) map[string]any {
	merged := map[string]any{}
	for key, value := range shared {
		merged[key] = value
	}
	for key, value := range own {
		merged[key] = value
	}
	return merged
}

// A local clone's origin is a filesystem path, and a field jirrabit validates as a
// URL turns that into a 400 — so the branch is created and the link silently fails.
// Anything without a scheme is simply not sent.
func TestFirstWebURLSkipsWhatIsNotAURL(t *testing.T) {
	tests := map[string]string{
		"/home/you/code/web":                  "",
		"git@github.com:you/web.git":          "",
		"../sibling":                          "",
		"https://github.com/you/web.git":      "https://github.com/you/web.git",
		"git@github.com:you/web.git\nssh://x": "ssh://x",
		"ssh://git@github.com/you/web.git":    "ssh://git@github.com/you/web.git",
	}
	for input, want := range tests {
		got := firstWebURL(strings.Split(input, "\n"))
		if got != want {
			t.Errorf("firstWebURL(%q) = %q, want %q", input, got, want)
		}
	}
	if got := firstWebURL(nil); got != "" {
		t.Errorf("firstWebURL(nil) = %q", got)
	}
}
