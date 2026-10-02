# task-rabbit-mcp

An MCP server that turns a [jirrabit](https://github.com/rroblf01/jirrabit)
issue into a working branch: it gathers everything the issue knows, checks the
repository is actually ready for the work, asks for the decisions only a human
can make, creates the branch, and runs that project's own linters and tests.

It is a companion to
[jirrabit-mcp](https://github.com/rroblf01/jirrabit-mcp), not a replacement. Run
both and an agent can do the whole loop: `jirrabit-mcp` is the CRUD surface over
the issue tracker, `task-rabbit-mcp` is the one that reads an issue, opens a
branch and validates the result.

```text
    ┌─────────────────┐     ┌──────────────────────┐
    │  jirrabit-mcp   │     │  task-rabbit-mcp     │
    │                 │     │                      │
    │  create, edit,  │     │  read the issue,     │
    │  transition,    │     │  inspect the repo,   │
    │  comment, log   │     │  create the branch,  │
    │  time, search   │     │  run the checks,     │
    │                 │     │  report back         │
    └────────┬────────┘     └──────────┬───────────┘
             └──────── jirrabit ───────┘
```

This process is a **pure MCP server**. It holds no AI provider credentials and
makes no model calls; it registers tools and does the work. The MCP client
(Claude Code, OpenCode, …) owns the model.

## What it does, and what it deliberately does not

The server's job is the part of "do this task" that is *mechanical and
verifiable*: read the issue, read the repository, refuse to start when the repo
is not ready, create one branch, run the project's own checks, report back.

Writing the code is the agent's job, and the server never does it. That split is
the whole design: a tool that edited source files would be a worse editor than
the model, and it would be one more thing running unattended on your machine.

**There is no shell tool.** The server executes commands it found declared in
your project — a `Makefile` target, a `package.json` script, a `pyproject.toml`
tool — and nothing else. It never takes a command as an argument.

## Build & run

```bash
go build -o bin/task-rabbit-mcp ./cmd/task-rabbit-mcp/

./bin/task-rabbit-mcp                  # stdio (default)
./bin/task-rabbit-mcp -transport http  # streamable HTTP on :8083
```

Go 1.27 and one dependency, `github.com/mark3labs/mcp-go`. No Makefile:
`go build`, `go vet` and `go test` are the whole toolchain.

## Configuration

Every setting is an environment variable; there is no config file. See
`.env.example` for the annotated list.

**This server is multi-tenant by default.** One deployed instance serves any
number of jirrabits: every tool takes `instanceUrl` and `apiKey`, and the caller
supplies the credentials for their own instance. The server holds no credentials
of its own.

`TASK_RABBIT_URL` and `TASK_RABBIT_API_KEY` are an optional *default* instance,
for a server you run alone. They must be set together or not at all: a URL with
no key can only mean "use a key I was never given". On a publicly-reachable
listener, setting them also requires `TASK_RABBIT_MCP_ALLOW_DEFAULT_INSTANCE=1`,
because a default instance is then served to every caller who names none of
their own.

`TASK_RABBIT_ALLOWED_HOSTS` is the control that makes a *published* server safe.
It is checked per call, not once at start-up. Unset means any host that is not
loopback or link-local, which is right for a trusted LAN.

## Safety

The server can create a git branch and run your project's test suite. That is a
meaningfully larger surface than a proxy that only reads an issue tracker, so the
boundaries are explicit:

- **Nothing runs that the project did not declare.** Detection reads files; it
  never executes them. See `AGENTS.md` for why that rules out `make -q`.
- **A dirty working tree stops everything.** No branch is created while you have
  uncommitted work, and the preview lists the files that are in the way.
- **No `push`, no `commit`, no `--force`, no `reset --hard`.** The server creates
  one branch from `origin/<base>` and nothing else. Committing and pushing are
  yours and the agent's.
- **Creating a branch takes two calls.** The first changes nothing and reports
  what it would do; it returns a signed, two-minute token. Show it to the user,
  get their answer, then call it again. If the repository changed in between, the
  token no longer verifies and nothing happens.
- **HTTP transport plus a configured repository is refused at start-up.** A
  published server that knows where your checkout lives, and can run your test
  suite, is remote code execution. Over stdio the client spawned the process, so
  that combination does not arise.
- **Attachments are written inside the work directory, never inside the repo.**
  Filenames come from whoever uploaded them, so every path is cleaned and
  checked to be under the target before a byte is written.

## License

MIT. See [LICENSE](LICENSE).