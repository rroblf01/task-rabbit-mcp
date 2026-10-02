// Package tools registers this server's MCP tools.
//
// The vocabulary is deliberately not Jira's. This server is not an adapter for
// the Atlassian tool names — it is a companion to jirrabit-mcp, and pretending to
// be a Jira tool surface would make the tool list lie about what it is. The names
// say what they do: read a task, look at a repository, open a branch, run checks,
// report back.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/rroblf01/task-rabbit-mcp/pkg/creds"
	"github.com/rroblf01/task-rabbit-mcp/pkg/jira"
	"github.com/rroblf01/task-rabbit-mcp/pkg/repos"
)

// Deps is what every handler needs: a way to reach the jirrabit instance a call
// names, and the operator's repository configuration.
type Deps struct {
	Pool *jira.Pool
	// Repos maps jirrabit projects onto local checkouts. May be empty, in which
	// case a call works in the current directory or wherever repoPath points.
	Repos *repos.Index
}

// target resolves the instance a call is about.
//
// Credentials come from three places in this order: the tool arguments, the HTTP
// headers, and the operator's configured default. Explicit arguments always win,
// so a registration carrying headers can still reach a second instance per call.
//
// Headers are treated exactly like an argument, never like an operator default:
// a header URL is caller-supplied and gets the allowlist and SSRF checks. The
// sibling project's bug was allowing a header path to skip them.
func (d Deps) target(ctx context.Context, args interface {
	GetTarget() (string, string)
}) (*jira.Client, error) {
	url, key := args.GetTarget()
	if url == "" || key == "" {
		headerURL, headerKey := creds.FromContext(ctx)
		if url == "" {
			url = headerURL
		}
		if key == "" {
			key = headerKey
		}
	}
	return d.Pool.Resolve(ctx, url, key)
}

// Register wires the tool groups onto the server.
func (d Deps) Register(srv *server.MCPServer) {
	s := newRegistrar(srv)

	registerContextTools(s, d)
	registerAttachmentTools(s, d)
	registerCheckTools(s, d)
}

// jsonResult marshals a value as compact JSON and returns it as tool text.
//
// Compact rather than indented: this payload is read by a model, and whitespace is
// tokens spent on nothing.
func jsonResult(value any) (*mcp.CallToolResult, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encoding tool result: %w", err)
	}
	return mcp.NewToolResultText(string(raw)), nil
}

// toolError turns an error into a tool result an agent can act on.
//
// Expected failures become tool errors with a sentence to reason about. Only a
// genuine bug returns a Go error.
func toolError(err error) (*mcp.CallToolResult, error) {
	if err == nil {
		return nil, nil
	}
	return mcp.NewToolResultError(jira.Message(err)), nil
}

// toolErrorf is the same for a failure that did not come from jirrabit.
func toolErrorf(format string, args ...any) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultErrorf(format, args...), nil
}

// truncate shortens text to n bytes on a rune boundary, marking that it happened.
//
// Cutting mid-rune produces a replacement character in the middle of a sentence,
// which reads as corruption rather than as truncation.
func truncate(text string, n int) (string, bool) {
	if len(text) <= n {
		return text, false
	}
	cut := n
	for cut > 0 && !utf8Start(text[cut]) {
		cut--
	}
	return text[:cut] + "\n… [truncated, " + fmt.Sprint(len(text)-cut) + " more bytes]", true
}

// utf8Start reports whether b starts a rune. A byte below 0x80 or above 0xBF does.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// clampLimit keeps a caller-supplied cap within bounds.
func clampLimit(given, fallback, max int) int {
	if given <= 0 {
		return fallback
	}
	if given > max {
		return max
	}
	return given
}

// logCall records which tool ran against what, never with the credentials.
func logCall(tool, subject string) {
	log.Printf("[task-rabbit-mcp %s] %s", tool, strings.TrimSpace(subject))
}
