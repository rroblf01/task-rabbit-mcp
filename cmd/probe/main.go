// Command probe lists the tools a task-rabbit-mcp server exposes, by connecting
// to it as a real MCP client over stdio. It is the quickest way to confirm the
// server starts and registers what it should.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/rroblf01/task-rabbit-mcp/internal/probe"
)

func main() {
	binary := flag.String("server", "./bin/task-rabbit-mcp", "path to the server binary")
	timeout := flag.Duration("timeout", 20*time.Second, "how long to wait for the server to answer")
	flag.Parse()

	report, err := probe.Inspect(*binary, *timeout)
	if err != nil {
		fail("%v", err)
	}

	if !report.ToolsSupported {
		fmt.Println("no tools registered yet")
		return
	}
	fmt.Printf("%d tools, %d bytes of instructions\n", len(report.Tools), len(report.Instructions))
	for _, t := range report.Tools {
		fmt.Printf("  %-32s %s\n", t.Name, t.Description)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "probe: "+format+"\n", args...)
	os.Exit(1)
}
