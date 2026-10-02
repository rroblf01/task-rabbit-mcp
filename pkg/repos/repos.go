// Package repos maps jirrabit projects onto local checkouts.
//
// The mapping is optional. With none configured, a tool call works on the
// repository the agent is standing in, or whatever repoPath the call names. It
// exists for the case that makes the tools pleasant: one person working across
// several checkouts, who should not have to repeat the same path on every call.
package repos

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Index is a read-only lookup from jirrabit project key to local path.
//
// Keys are compared case-insensitively because jirrabit issue keys are upper case
// and nobody types them that way in a path.
type Index struct {
	byProject map[string]string
}

// Parse reads a TASK_RABBIT_REPOS value.
//
// The format is JSON, because a map is a map:
//
//	{"WEB":"/home/you/code/web","OPS":"/home/you/code/ops"}
//
// JSON rather than KEY=path,KEY2=path2 because a Windows path or a path with a
// comma in it makes the flat format ambiguous, and this value is typed by a human
// into an env file.
func Parse(raw string) (*Index, error) {
	trimmed := strings.TrimSpace(raw)
	index := &Index{byProject: map[string]string{}}
	if trimmed == "" {
		return index, nil
	}

	// json.Unmarshal into a map rather than hand-parsing, so a malformed value is
	// one clear error instead of a silently empty index that then sends every call
	// to the current directory.
	var parsed map[string]string
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return nil, fmt.Errorf(
			"TASK_RABBIT_REPOS is not valid JSON: %w. Expected an object of project key to local path, "+
				`for example {"WEB":"/home/you/code/web"}`, err)
	}

	for project, path := range parsed {
		project = strings.ToUpper(strings.TrimSpace(project))
		path = strings.TrimSpace(path)
		if project == "" || path == "" {
			return nil, fmt.Errorf(
				"TASK_RABBIT_REPOS has an entry with an empty project key or path: %q -> %q", project, path)
		}
		index.byProject[project] = filepath.Clean(path)
	}
	return index, nil
}

// Lookup returns the configured path for a jirrabit project key.
func (i *Index) Lookup(project string) (string, bool) {
	if i == nil || i.byProject == nil {
		return "", false
	}
	path, ok := i.byProject[strings.ToUpper(strings.TrimSpace(project))]
	return path, ok
}

// Projects lists the configured keys, sorted, for a log line or an error message.
func (i *Index) Projects() []string {
	if i == nil {
		return nil
	}
	out := make([]string, 0, len(i.byProject))
	for project := range i.byProject {
		out = append(out, project)
	}
	sort.Strings(out)
	return out
}

// Describe renders the mapping for a start-up log.
//
// Paths are not secret, but an operator reading a log wants to confirm the mapping
// took effect, and "3 projects mapped" is not enough to catch a typo in one of
// them.
func (i *Index) Describe() string {
	if i == nil || len(i.byProject) == 0 {
		return "none (each call works in the current directory unless it names repoPath)"
	}
	parts := make([]string, 0, len(i.byProject))
	for _, project := range i.Projects() {
		parts = append(parts, fmt.Sprintf("%s=%s", project, i.byProject[project]))
	}
	return strings.Join(parts, ", ")
}
