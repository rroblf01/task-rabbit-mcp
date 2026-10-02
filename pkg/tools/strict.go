package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Strict argument checking.
//
// It exists because of a failure that repeats: an agent sends an argument the
// tool does not have — `issueId` where it wants `issueKey`, a Jira-shaped
// `commentIdOrKey`, a name invented from a sibling tool — and the server used to
// drop it without a word. The call then failed, or worse, succeeded while quietly
// discarding what was asked for.
//
// "I do not know that argument, here are the ones I do" turns a silent wrong
// result into a self-correcting one, which is the difference between a typo
// costing a round trip and costing a debugging session.
//
// The cost is deliberate: a call carrying a harmless extra argument is rejected
// rather than ignored. An agent that pads calls with fields it invented will
// retry.

// registrar is the server handle registration writes to. It is the real server
// with one thing added: every handler is wrapped in the argument check. Doing it
// here rather than in each handler means a new tool is checked by construction,
// which is the only way the guarantee survives.
type registrar struct {
	inner *server.MCPServer
}

func newRegistrar(s *server.MCPServer) *registrar {
	return &registrar{inner: s}
}

func (r *registrar) AddTool(tool mcp.Tool, handler server.ToolHandlerFunc) {
	r.inner.AddTool(tool, guardArguments(tool, handler))
}

// guardArguments rejects arguments the tool does not declare, before the handler
// sees them.
func guardArguments(tool mcp.Tool, next server.ToolHandlerFunc) server.ToolHandlerFunc {
	known := declaredArguments(tool)
	if len(known) == 0 {
		// A tool with no readable schema is left alone rather than made to reject
		// everything.
		return next
	}
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Arguments is typed any on the request: a JSON object in practice, nil
		// when the call carried none.
		args, _ := req.Params.Arguments.(map[string]any)
		if err := checkArguments(tool.Name, args, known); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return next(ctx, req)
	}
}

// declaredArguments reads the tool's own input schema, which is the single source
// of truth for what it accepts. Deriving the list from the schema rather than
// keeping a second one is what stops the two drifting apart.
func declaredArguments(tool mcp.Tool) map[string]bool {
	if len(tool.RawInputSchema) == 0 {
		return nil
	}
	var parsed struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(tool.RawInputSchema, &parsed); err != nil {
		return nil
	}
	if len(parsed.Properties) == 0 {
		return nil
	}
	known := make(map[string]bool, len(parsed.Properties))
	for name := range parsed.Properties {
		known[name] = true
	}
	return known
}

func checkArguments(toolName string, args map[string]any, known map[string]bool) error {
	if len(args) == 0 {
		return nil
	}
	var unknown []string
	for name := range args {
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)

	accepted := make([]string, 0, len(known))
	for name := range known {
		accepted = append(accepted, name)
	}
	sort.Strings(accepted)

	described := make([]string, 0, len(unknown))
	for _, name := range unknown {
		if near := nearest(name, accepted); near != "" {
			described = append(described, fmt.Sprintf("%q (did you mean %q?)", name, near))
			continue
		}
		described = append(described, fmt.Sprintf("%q", name))
	}

	return fmt.Errorf(
		"%s does not accept %s. Accepted arguments: %s. Nothing was changed, so you can call it again with "+
			"the right names",
		toolName, strings.Join(described, ", "), strings.Join(accepted, ", "))
}

// nearest returns the closest accepted name, or "" when nothing is close enough to
// be worth suggesting. Suggesting a wrong name is worse than suggesting none: it
// sends the agent off to try an argument that does not exist either.
func nearest(given string, candidates []string) string {
	if len(candidates) == 0 {
		return ""
	}
	// Seeded above every possible distance on purpose: starting at 0 makes
	// "distance < 0" never true, so no candidate is ever better and the
	// suggestion silently never fires.
	best, bestDistance := "", len(given)+len(candidates[0])+1
	for _, candidate := range candidates {
		if d := editDistance(strings.ToLower(given), strings.ToLower(candidate)); d < bestDistance {
			best, bestDistance = candidate, d
		}
	}
	// Half the length of the wrong word: "issueId" is corrected, "bananas" is not
	// sent looking like a typo of "issueKey".
	limit := len(given)/2 + 1
	if best == "" || bestDistance > limit || strings.EqualFold(best, given) {
		return ""
	}
	return best
}

// editDistance is the Levenshtein distance, restricted to the operations that
// matter for catching a misspelling.
func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	previous := make([]int, len(br)+1)
	current := make([]int, len(br)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		current[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			current[j] = min3(current[j-1]+1, previous[j]+1, previous[j-1]+cost)
		}
		copy(previous, current)
	}
	return previous[len(br)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
