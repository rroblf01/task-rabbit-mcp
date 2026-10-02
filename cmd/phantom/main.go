// Command phantom fails when the server's prose names a tool that is not
// registered.
//
// The prose is part of the interface. The server instructions are the only
// documentation an agent is guaranteed to read, and a tool description is what
// an agent reads when deciding between two tools. If either names a tool that
// does not exist, the agent calls it, gets "tool not found", and has no way to
// recover — it does not know whether it misspelled a tool or whether the feature
// is unavailable.
//
// This is not hypothetical. In the sibling project, editJiraIssue's description
// told agents to use transitionJiraIssue to change status, and the README
// repeated it, for a release, while no such tool existed. Nothing caught it,
// because no test read the prose.
//
// It needs a binary and nothing else — no jirrabit, no git, no network — so it
// runs in CI on every push.
package main

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
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

	phantoms := scan(report)
	fmt.Printf("checking %d tools and %d bytes of instructions\n",
		len(report.Tools), len(report.Instructions))

	if len(phantoms) == 0 {
		if !report.ToolsSupported {
			fmt.Println("ok: the server registers no tools yet, and its instructions name none")
		} else {
			fmt.Println("ok: the prose names no unregistered tool")
		}
		return
	}
	for _, p := range phantoms {
		fmt.Fprintf(os.Stderr, "%s is named in %s but no such tool is registered\n", p.name, p.where)
	}
	os.Exit(1)
}

type phantom struct{ name, where string }

// toolNamePattern matches the shape of a tool name: a verb followed by a
// capitalised subject.
//
// It is deliberately loose. The cost of a false positive is a red build with one
// word to exempt; the cost of a miss is a broken agent. A tighter pattern would
// be wrong in exactly the case that matters — a prose reference to a tool
// somebody renamed, which is the bug this exists to catch.
var toolNamePattern = regexp.MustCompile(
	`\b(?:load|inspect|start|list|run|report|materialize|find|check|get)[A-Z][A-Za-z]*\b`)

func scan(report probe.Report) []phantom {
	registered := report.Names()
	found := map[string]string{}

	note := func(where, text string) {
		for _, candidate := range toolNamePattern.FindAllString(text, -1) {
			if !registered[candidate] {
				found[candidate] = where
			}
		}
	}

	note("the server instructions", report.Instructions)
	for _, t := range report.Tools {
		note("the description of "+t.Name, t.Description)
	}

	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]phantom, 0, len(names))
	for _, name := range names {
		out = append(out, phantom{name: name, where: found[name]})
	}
	return out
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "phantom: "+format+"\n", args...)
	os.Exit(1)
}
