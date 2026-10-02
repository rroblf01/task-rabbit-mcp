// Command task-rabbit-mcp is an MCP server that carries a jirrabit issue
// through to a validated branch: it reads the issue, checks the repository is
// ready, creates one branch, runs the project's own linters and tests, and
// reports back.
//
// It is a pure MCP server. It holds no AI provider credentials and makes no
// model calls: it registers tools and does the mechanical, checkable part of the
// work. Writing the code is the agent's job — see AGENTS.md for why that split
// is the design rather than a limitation.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/rroblf01/task-rabbit-mcp/pkg/creds"
)

const serverName = "task-rabbit-mcp"

// serverVersion is a var, not a const, so the release build can stamp it with
// -ldflags "-X main.serverVersion=…". A const is inlined at compile time and the
// linker silently ignores -X for it, which is how a Dockerfile can pass a
// VERSION and every published image still report 1.0.0.
var serverVersion = "0.1.0"

// instructions is what the client is told about this server, and it is the only
// documentation an agent is guaranteed to read. An agent that has these
// instructions and the tool schemas should be able to work without a README.
//
// The order below is the order of the work: read, check, ask, branch, validate,
// report. Stating what is NOT here matters as much: an agent that does not know
// a shell tool is absent will try to improvise one.
const instructions = `You are working with task-rabbit-mcp, which carries a jirrabit issue through to a
validated branch. It reads the issue, inspects the repository, creates one
branch, runs the project's own linters and tests, and reports back to the issue.

WHAT THIS SERVER DOES NOT DO
It does not write code, and it has no shell tool. Editing files is yours. The
server runs commands it found declared in the project — a Makefile target, a
package.json script, a pyproject.toml tool — and nothing else; there is no way to
hand it a command to run. It never commits, pushes, force-pushes or resets.

CHOOSING AN INSTANCE
Every tool accepts instanceUrl and apiKey, and how you must use them depends on
this server:

__INSTANCE_CHOICE__

An apiKey without an instanceUrl is rejected, so you cannot send one person's
credentials to another person's data.

WORKING A TASK
Read the issue first, and read it before you touch the repository: the issue is
where the acceptance criteria, the comments with the actual detail, and the
attached screenshots live, and a task started without them is a task started
wrong. Then inspect the repository, which tells you what is ready and what is
missing, and what this project checks.

Ask the user for what only they can decide — the branch name among them — and
wait for the answer. Do not invent one and do not start editing on a branch you
guessed.

ASKING QUESTIONS
This server cannot ask anybody anything: a tool returns a result and you decide
what to do with it. Anything the server reports as a missing decision is
something you must put to the user in your own words, and then wait. A server
instruction is not consent.

CREATING THE BRANCH
Branch creation takes two calls. The first, with no 'confirm', changes nothing:
it reports what it would create and returns a confirmation token. Show that to
the user, get their answer, then call the same tool again with confirm set to
that token. The token is bound to what the first call reported, so if the
repository changed in between the second call refuses and nothing happens.`

// placeholder is replaced at start-up with the concrete credential rule, which
// depends on how this deployment was configured.
const instanceChoicePlaceholder = "__INSTANCE_CHOICE__"

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetPrefix("[" + serverName + "] ")

	transport := flag.String("transport", envOr("TASK_RABBIT_TRANSPORT", "stdio"),
		"MCP transport: stdio or http")
	addr := flag.String("addr", envOr("TASK_RABBIT_ADDR", "127.0.0.1:8083"),
		"listen address for -transport http")
	path := flag.String("path", envOr("TASK_RABBIT_PATH", "/mcp"),
		"MCP endpoint path for -transport http")
	flag.Parse()

	if err := run(*transport, *addr, *path); err != nil {
		log.Fatalf("%v", err)
	}
}

func run(transport, addr, path string) error {
	defaultURL := strings.TrimSpace(os.Getenv("TASK_RABBIT_URL"))
	defaultKey := strings.TrimSpace(os.Getenv("TASK_RABBIT_API_KEY"))

	// Together or not at all. A URL with no key can only ever mean "use a key I
	// was never given", and a key with no URL can only ever mean "use it
	// somewhere", which is how one person's credential ends up in front of
	// another person's data.
	if (defaultURL == "") != (defaultKey == "") {
		return errors.New(
			"TASK_RABBIT_URL and TASK_RABBIT_API_KEY must be set together or not at all: " +
				"unset both to serve many instances, set both for a personal default")
	}

	// Over stdio the client spawned this process, so the only caller is the user
	// and there is nothing to guard. Over a network listener that stops being
	// true, and the two things below stop being safe.
	if publiclyReachable(transport, addr) {
		if defaultURL != "" && !truthy(os.Getenv("TASK_RABBIT_MCP_ALLOW_DEFAULT_INSTANCE")) {
			return fmt.Errorf(
				"refusing to serve a default jirrabit instance (%s) on a publicly-reachable listener (%s): "+
					"every caller who names no instance of their own would read %s's data. Either bind to "+
					"loopback and terminate TLS in a proxy, drop the default instance so callers pass their "+
					"own, or set TASK_RABBIT_MCP_ALLOW_DEFAULT_INSTANCE=1 to say you mean it",
				defaultURL, addr, defaultURL)
		}
		if repos := strings.TrimSpace(os.Getenv("TASK_RABBIT_REPOS")); repos != "" {
			return fmt.Errorf(
				"refusing to start: TASK_RABBIT_REPOS is set and this server is publicly reachable (%s). "+
					"This server can create branches and run the project's test suite, so a deployment strangers "+
					"can reach would be remote code execution on a checkout. Run it over stdio, bind to loopback, "+
					"or remove TASK_RABBIT_REPOS and let each call name its own repoPath",
				addr)
		}
	}

	srv := server.NewMCPServer(
		serverName,
		serverVersion,
		server.WithInstructions(buildInstructions(defaultURL)),
		server.WithRecovery(), // a panic in one handler must not take the server down
	)

	switch strings.ToLower(strings.TrimSpace(transport)) {
	case "stdio":
		return serveStdio(srv)
	case "http", "streamable-http", "streamablehttp":
		return serveHTTP(srv, addr, path)
	default:
		return fmt.Errorf("unknown transport %q: use stdio or http", transport)
	}
}

// buildInstructions fills in the one part of the instructions that depends on
// this deployment rather than on the tool set.
func buildInstructions(defaultURL string) string {
	choice := `This server has no default instance, so pass instanceUrl and apiKey on every call. Ask the
user for their jirrabit's URL and create an API key in their profile under API keys.`
	if defaultURL != "" {
		choice = `This server has a default jirrabit instance, so you may omit instanceUrl and apiKey. Pass
them anyway when the user names a different jirrabit — the arguments always win over the default.`
	}
	return strings.ReplaceAll(instructions, instanceChoicePlaceholder, choice)
}

// serveStdio speaks MCP over stdin/stdout. The client owns the process, so a
// clean shutdown is just a return.
func serveStdio(srv *server.MCPServer) error {
	log.Printf("ready on stdio")
	// ServeStdio installs its own signal handling and returns on SIGINT/SIGTERM.
	return server.ServeStdio(srv)
}

// serveHTTP speaks MCP over streamable HTTP, which is what a client connecting
// to a server running as a service needs.
func serveHTTP(srv *server.MCPServer, addr, path string) error {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	httpSrv := server.NewStreamableHTTPServer(srv,
		server.WithEndpointPath(path),
		// Per-call credentials may arrive as HTTP headers instead of tool
		// arguments. Stashed per request; the values are never logged.
		server.WithHTTPContextFunc(creds.HTTPContextFunc),
		// Stateless: every request is self-contained, so the server can run
		// behind a load balancer or scaled horizontally without sticky sessions,
		// and a dropped connection loses nothing.
		server.WithStateLess(true),
		// The anti-DNS-rebinding guard only accepts localhost by default, which
		// would reject every request on a real hostname. This server is meant to
		// sit behind the operator's own TLS terminator, so the guard is off and
		// the operator's proxy is the trust boundary.
		server.WithDisableLocalhostProtection(true),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errs := make(chan error, 1)
	go func() {
		log.Printf("ready on %s%s", displayAddr(addr), path)
		// Start reports http.ErrServerClosed after Shutdown; that is the
		// expected path, not a failure.
		if err := httpSrv.Start(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		log.Printf("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

// displayAddr renders a listen address for humans. ":8083" has no host part, and
// printing "http://:8083/mcp" reads like a broken URL rather than the wildcard
// bind it actually is.
func displayAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "http://0.0.0.0" + addr
	}
	return "http://" + addr
}

// publiclyReachable reports whether this process is a server on a network
// interface other hosts can open a connection to.
//
// The distinction decides whether this deployment is personal or exposed. Over
// stdio the client started this process and speaks to it over pipes, so there is
// exactly one caller and it is the user. An HTTP listener on a loopback address
// is reachable only from this machine, which is what a private server behind a
// same-host reverse proxy looks like. Anything else — the wildcard ":8083", a
// public IP, a container's bridge address — means strangers can reach it.
func publiclyReachable(transport, addr string) bool {
	if transport != "http" && transport != "streamable-http" && transport != "streamablehttp" {
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// An addr we cannot parse is one we should not assume is private.
		return true
	}
	if host == "" {
		// The wildcard bind: every interface, including the public one.
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// A hostname in the listen address. Loopback spelled as a name is the
		// only one that can be reasoned about here; anything else is a public
		// interface by assumption.
		return !strings.EqualFold(host, "localhost")
	}
	return !ip.IsLoopback()
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

// truthy is deliberately narrow. An unrecognised value is false, so a typo
// disables a feature rather than enabling it by accident.
func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
