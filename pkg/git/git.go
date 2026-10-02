// Package git is a small wrapper over the git binary.
//
// It shells out rather than using a Go git library, for one reason: branch
// creation is the only write this server performs, and it must be the same git
// that configured the repository. A library re-implementing the plumbing would
// disagree with the user's git about hooks, filters, worktrees and the
// filesafe-directory rules their distribution enforces — and it would create the
// branch without the hooks the project expects to run.
//
// Every command is an argv. No shell, ever, so nothing in the repository's own
// text can change what runs.
package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// defaultTimeout bounds a single git invocation. Git is local and fast; a minute
// is not a performance budget, it is the point at which something is wrong — a
// credential prompt waiting on a terminal, or a hook that asks a question.
const defaultTimeout = time.Minute

// Remote is one configured remote.
type Remote struct {
	Name string   `json:"name"`
	URLs []string `json:"urls"`
}

// State is everything this server needs to know about a checkout to decide whether
// a task can start, and to say why when it cannot.
type State struct {
	// Root is the repository's top level, which is not always the path passed in.
	Root string `json:"root"`
	// Branch is the current branch, empty when HEAD is detached.
	Branch string `json:"branch"`
	// Detached says HEAD is not on a branch.
	Detached bool `json:"detached"`
	// Upstream is the branch this one tracks, "origin/main" or empty.
	Upstream string `json:"upstream,omitempty"`
	// Ahead and Behind count commits against the upstream.
	Ahead  int `json:"ahead"`
	Behind int `json:"behind"`
	// Clean is true when nothing is modified, staged or untracked.
	Clean bool `json:"clean"`
	// Changes are the porcelain entries, unfiltered, so a refusal can show exactly
	// what is in the way.
	Changes []string `json:"changes,omitempty"`
	// Remotes and DefaultBranch answer "where does this branch come from".
	Remotes       []Remote `json:"remotes"`
	DefaultBranch string   `json:"defaultBranch,omitempty"`
	// GitVersion is what answered, or empty when git is not installed.
	GitVersion string `json:"gitVersion,omitempty"`
	// Available is false when the binary is missing, which is the case for this
	// server's own container image.
	Available bool `json:"available"`
}

// ErrNoGit means the binary is not installed.
var ErrNoGit = errors.New("git is not installed")

// Available reports whether a usable git is on PATH, and which one.
func Available(ctx context.Context) (string, bool) {
	out, err := run(ctx, "", "--version")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

// Inspect reads the state of the repository containing dir.
func Inspect(ctx context.Context, dir string) (*State, error) {
	state := &State{}

	version, ok := Available(ctx)
	if !ok {
		state.Available = false
		return state, ErrNoGit
	}
	state.Available = true
	state.GitVersion = version

	root, err := run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		// Not a repository, or a bare one. Either way there is no working tree to
		// branch in, and the caller is told which.
		return state, fmt.Errorf("%s is not inside a git working tree: %s", dir, firstLine(err))
	}
	state.Root = strings.TrimSpace(root)

	branch, err := run(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err == nil {
		state.Branch = strings.TrimSpace(branch)
	}
	// "HEAD" is what a detached HEAD reports, and it is not a branch name.
	if state.Branch == "HEAD" || state.Branch == "" {
		state.Detached = true
		state.Branch = ""
	}

	porcelain, err := run(ctx, dir, "status", "--porcelain=v1")
	if err == nil {
		entries := strings.Split(strings.TrimRight(porcelain, "\n"), "\n")
		if porcelain == "" {
			state.Clean = true
		} else {
			state.Changes = entries
		}
	}

	if remotes, err := run(ctx, dir, "remote", "-v"); err == nil {
		state.Remotes = parseRemotes(remotes)
	}
	// origin/HEAD is the only place the default branch is recorded, and it does not
	// exist on a repository that has never been pushed.
	if head, err := run(ctx, dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		state.DefaultBranch = strings.TrimPrefix(strings.TrimSpace(head), "origin/")
	}
	if upstream, err := run(ctx, dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); err == nil {
		state.Upstream = strings.TrimSpace(upstream)
	}
	if counts, err := run(ctx, dir, "rev-list", "--left-right", "--count", "@{upstream}...HEAD"); err == nil {
		state.Ahead, state.Behind = parseCounts(counts)
	}

	return state, nil
}

// parseRemotes reads `git remote -v`, which prints two lines per remote.
func parseRemotes(output string) []Remote {
	seen := map[string]int{}
	var remotes []Remote
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// Fetch and push lines are both printed; the fetch URL leads.
		index, ok := seen[fields[0]]
		if !ok {
			remotes = append(remotes, Remote{Name: fields[0], URLs: []string{fields[1]}})
			index = len(remotes) - 1
			seen[fields[0]] = index
			continue
		}
		if fields[1] != remotes[index].URLs[0] {
			remotes[index].URLs = append(remotes[index].URLs, fields[1])
		}
	}
	return remotes
}

func parseCounts(output string) (ahead, behind int) {
	fields := strings.Fields(strings.TrimSpace(output))
	if len(fields) != 2 {
		return 0, 0
	}
	// `rev-list --left-right --count upstream...HEAD` prints behind-then-ahead.
	behind, _ = strconv.Atoi(fields[0])
	ahead, _ = strconv.Atoi(fields[1])
	return ahead, behind
}

// ErrDirtyTree is returned by CreateBranch when the working tree is not clean.
type ErrDirtyTree struct{ Changes []string }

func (e *ErrDirtyTree) Error() string {
	return fmt.Sprintf("the working tree has %d uncommitted change(s): %s",
		len(e.Changes), strings.Join(e.Changes, ", "))
}

// ErrBranchExists is returned when the name is already taken.
type ErrBranchExists struct{ Branch string }

func (e *ErrBranchExists) Error() string {
	return fmt.Sprintf("the branch %q already exists locally, so creating it would either fail or "+
		"silently switch to it. Pick another name, or delete the branch yourself if you meant to reuse it",
		e.Branch)
}

// ErrBaseMissing is returned when the base ref does not exist.
type ErrBaseMissing struct{ Ref string }

func (e *ErrBaseMissing) Error() string {
	return fmt.Sprintf("%s does not exist in this repository. Fetch it, or name a different origin branch", e.Ref)
}

// CreateBranch runs `git checkout -b <branch> <base>` after checking every
// precondition, in this order:
//
//  1. the working tree is clean;
//  2. the name is one git accepts;
//  3. the name is not already taken;
//  4. the base ref resolves.
//
// The order matters because each refusal has a different fix, and the cheapest to
// explain is the one about a dirty tree: it is the one a caller is most likely to
// hit and least likely to have noticed.
//
// Nothing here is retried. A checkout that reported failure may still have changed
// the working tree, and repeating it is how a half-applied branch becomes two.
func CreateBranch(ctx context.Context, dir, branch, base string) error {
	state, err := Inspect(ctx, dir)
	if err != nil {
		return err
	}
	if !state.Clean {
		return &ErrDirtyTree{Changes: state.Changes}
	}

	if err := CheckBranchName(ctx, dir, branch); err != nil {
		return err
	}
	if exists, err := branchExists(ctx, dir, branch); err != nil {
		return err
	} else if exists {
		return &ErrBranchExists{Branch: branch}
	}
	if _, err := run(ctx, dir, "rev-parse", "--verify", "--quiet", base+"^{commit}"); err != nil {
		return &ErrBaseMissing{Ref: base}
	}

	_, err = run(ctx, dir, "checkout", "-b", branch, base)
	return err
}

// CheckBranchName asks git whether a name is usable.
//
// git check-ref-format is the authority here, not a regular expression. Git has
// rules about ~, ^, :, ?, *, [, backslash, leading dots, a trailing dot, "..", the
// 255-byte limit and the "@{" syntax; a hand-rolled subset is a subset, and the
// failures arrive as confusing errors from a later command.
func CheckBranchName(ctx context.Context, dir, branch string) error {
	if branch == "" {
		return errors.New("a branch name is required")
	}
	// --branch makes git apply the same rules it would for `git checkout -b`, and
	// print the reason on failure.
	if out, err := run(ctx, dir, "check-ref-format", "--branch", branch); err != nil {
		message := strings.TrimSpace(out)
		if message == "" {
			message = firstLine(err)
		}
		return fmt.Errorf("git will not accept %q as a branch name: %s", branch, message)
	}
	return nil
}

func branchExists(ctx context.Context, dir, branch string) (bool, error) {
	_, err := run(ctx, dir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}
	// A non-zero exit from --quiet means "no such ref", which is the answer.
	return false, nil
}

// BaseRef is what a caller would pass as the base of a new branch: the current
// branch's upstream, the origin default branch, or the current branch itself.
func (s *State) BaseRef() string {
	switch {
	case s.Upstream != "":
		return s.Upstream
	case s.DefaultBranch != "" && len(s.Remotes) > 0:
		return s.Remotes[0].Name + "/" + s.DefaultBranch
	case s.Branch != "":
		return s.Branch
	default:
		return ""
	}
}

// run executes one git command and returns its stdout.
//
// stderr is folded into the error rather than discarded: git's own message is
// almost always the useful part of a failure, and an agent shown "exit status 128"
// has nothing to act on.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// The repository's own configuration is honoured: a user's safe.directory, their
	// hooks, their filters. This server has no opinion about how a checkout is
	// configured, and overriding it would be a surprising way to lose a user's
	// signing settings.
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return string(out), errors.New(detail)
	}
	return string(out), nil
}

func firstLine(err error) string {
	if err == nil {
		return ""
	}
	line, _, _ := strings.Cut(err.Error(), "\n")
	return strings.TrimSpace(line)
}

// ResolveRepo is a convenience for callers that were handed a possibly relative
// path: it returns an absolute one and fails clearly when there is nothing there.
func ResolveRepo(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("no repository path was given")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("repository path %q cannot be resolved: %w", path, err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("repository path %q does not exist", path)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("repository path %q is a file, not a directory", path)
	}
	return absolute, nil
}
