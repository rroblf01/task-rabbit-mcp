// Package probe connects to a task-rabbit-mcp server as a real MCP client over
// stdio and reports what it serves: the instructions it gave and the tools it
// registered.
//
// Both facts are needed without a jirrabit, a git repository or a network, which
// is what lets the checks in CI run on every push. The server is spawned exactly
// the way an editor would spawn it, so a registration path that panics or a
// transport that fails to start shows up here rather than in front of a user.
package probe

import (
	"context"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// Report is what a server told a client about itself.
type Report struct {
	Instructions string
	Tools        []mcp.Tool
	// ToolsSupported is whether the server advertised the tools capability at
	// all. A server with nothing registered does not advertise it, and asking it
	// for the list is answered with "method not found" — which is a true answer,
	// not a broken server, and has to be reported as such rather than as a
	// failure of the probe.
	ToolsSupported bool
}

// Names returns the registered tool names, which is the set everything else is
// checked against.
func (r Report) Names() map[string]bool {
	names := make(map[string]bool, len(r.Tools))
	for _, t := range r.Tools {
		names[t.Name] = true
	}
	return names
}

// Inspect starts the server binary and collects its report.
func Inspect(binary string, timeout time.Duration) (Report, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	proc := transport.NewStdio(binary, nil)
	defer proc.Close()

	session := client.NewClient(proc)
	if err := session.Start(ctx); err != nil {
		return Report{}, fmt.Errorf("start: %w", err)
	}
	defer session.Close()

	init, err := session.Initialize(ctx, mcp.InitializeRequest{})
	if err != nil {
		return Report{}, fmt.Errorf("initialize: %w", err)
	}

	tools, err := session.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		// A server that advertises the capability and then cannot list is a bug
		// worth failing on. One that never advertised it simply has no tools, and
		// "no tools" is a legitimate state for a server mid-build.
		if init.Capabilities.Tools == nil {
			return Report{Instructions: init.Instructions}, nil
		}
		return Report{}, fmt.Errorf("list tools: %w", err)
	}

	return Report{
		Instructions:   init.Instructions,
		Tools:          tools.Tools,
		ToolsSupported: true,
	}, nil
}
