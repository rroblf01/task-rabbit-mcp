package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rroblf01/task-rabbit-mcp/pkg/toolchain"
)

// Repo resolution, shared by every tool that touches a checkout.
//
// The order is fixed and reported, because "which repository did this run in?" is
// the first question when a check fails somewhere unexpected:
//
//  1. repoPath on the call, for the agent that knows where it is standing.
//  2. The operator's TASK_RABBIT_REPOS mapping, by jirrabit project key.
//  3. The working directory.
//
// Nothing is guessed beyond that. A tool that went looking for a repository would
// eventually pick one, and the wrong checkout is a bug that only shows up as
// strange test results much later.
type resolvedRepo struct {
	Path string `json:"path"`
	// How says which of the three rules matched, so the caller can see why.
	How string `json:"resolvedBy"`
	// Project is the jirrabit project key the path came from, when it came from
	// the mapping.
	Project string `json:"project,omitempty"`
}

func (d Deps) resolveRepo(project, repoPath string) (resolvedRepo, error) {
	if trimmed := strings.TrimSpace(repoPath); trimmed != "" {
		absolute, err := filepath.Abs(trimmed)
		if err != nil {
			return resolvedRepo{}, fmt.Errorf("repoPath %q cannot be resolved: %w", trimmed, err)
		}
		info, err := os.Stat(absolute)
		if err != nil {
			return resolvedRepo{}, fmt.Errorf("repoPath %q does not exist", trimmed)
		}
		if !info.IsDir() {
			return resolvedRepo{}, fmt.Errorf("repoPath %q is a file, not a directory", trimmed)
		}
		return resolvedRepo{Path: absolute, How: "repoPath"}, nil
	}

	if key := strings.TrimSpace(project); key != "" {
		if path, ok := d.Repos.Lookup(key); ok {
			absolute, err := filepath.Abs(path)
			if err != nil {
				return resolvedRepo{}, fmt.Errorf(
					"the repository configured for project %s (%s) cannot be resolved: %w", key, path, err)
			}
			if _, err := os.Stat(absolute); err != nil {
				return resolvedRepo{}, fmt.Errorf(
					"the repository configured for project %s does not exist: %s", key, absolute)
			}
			return resolvedRepo{Path: absolute, How: "TASK_RABBIT_REPOS", Project: key}, nil
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return resolvedRepo{}, fmt.Errorf("cannot determine the working directory: %w", err)
	}
	return resolvedRepo{Path: cwd, How: "the working directory"}, nil
}

// checkReport is what listTaskChecks returns.
type checkReport struct {
	Repository resolvedRepo `json:"repository"`
	// RepositoryKind is what is actually at that path, so a wrong repoPath shows
	// up here rather than as a confusing failure later.
	RepositoryKind string `json:"repositoryKind"`
	// Checks are the winners, one per role.
	Checks []toolchain.Check `json:"checks"`
	// AlsoAvailable are checks the project declares that lost a role to a
	// higher-precedence source. Reported because a silently dropped check is a
	// check an agent believes does not exist.
	AlsoAvailable []toolchain.Check `json:"alsoAvailable,omitempty"`
	// Signals are things that told us something without producing a check.
	Signals []string `json:"signals,omitempty"`
	// Roles is the vocabulary, so an agent can ask for one it does not see.
	Roles []string `json:"roles"`
	Note  string   `json:"note,omitempty"`
}

func detectFor(repo resolvedRepo) (*toolchain.Toolchain, error) {
	tc, err := toolchain.Detect(repo.Path)
	if err != nil {
		return nil, fmt.Errorf("cannot read the repository at %s: %w", repo.Path, err)
	}
	return tc, nil
}

// hasCheck reports whether any check is runnable, so a caller can say "this
// project declares nothing" instead of returning an empty list that reads like
// "everything is fine".
func hasCheck(tc *toolchain.Toolchain) bool { return len(tc.Ordered()) > 0 }

func noChecksNote() string {
	return "this project declares no checks this server can run. It may have none, or its commands may " +
		"live somewhere this server does not read — a task runner in a non-standard file, or a CI pipeline " +
		"that runs commands from a repository's own scripts. If you know the command, it can be declared " +
		"in .task-rabbit.json at the repository root, which this server does read."
}
