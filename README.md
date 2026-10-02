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

## "Realiza esta tarea"

This server exists to be run **next to**
[jirrabit-mcp](https://github.com/rroblf01/jirrabit-mcp) and driven by an agent
that has been told to work a jirrabit issue. Once both are registered, the whole
loop is one sentence to the agent:

> Realiza esta tarea

and it reads the issue, checks the repository, asks you for the branch name,
creates the branch, does the work, runs the project's checks and writes back. The
[workflow instructions](docs/agent-workflow.md) to paste into `CLAUDE.md` or
`AGENTS.md` are what turn that sentence into the loop — without them the agent has
the tools and no reason to use them in order.

### OpenCode

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "jirrabit": {
      "type": "remote",
      "url": "https://jirrabit-mcp.ricardorobles.es/mcp",
      "enabled": true,
      "headers": {
        "X-Jirrabit-Instance-Url": "https://jirrabit.ricardorobles.es",
        "X-Jirrabit-Api-Key": "{env:JIRRABIT_API_KEY}"
      }
    },
    "task-rabbit": {
      "type": "local",
      "command": ["/absolute/path/to/bin/task-rabbit-mcp"],
      "environment": {
        "TASK_RABBIT_URL": "http://localhost:8000",
        "TASK_RABBIT_API_KEY": "{env:JIRRABIT_API_KEY}",
        "TASK_RABBIT_WORKSPACE": "/tmp/task-rabbit"
      },
      "enabled": true
    }
  }
}
```

`task-rabbit` runs as a local process on purpose. It needs `git` and a checkout,
and the published container image deliberately ships neither — a `git` on the far
side of a tool that creates branches would turn any bug in the tool layer into
remote code execution.

`TASK_RABBIT_URL` and `TASK_RABBIT_API_KEY` give it a default jirrabit, so a call
names only the issue. Point them at your own instance and key; a local jirrabit on
`localhost:8000` is the normal case.

### Claude Code

```bash
# The tracker: the public instance, credentials as headers.
claude mcp add --transport http jirrabit https://jirrabit-mcp.ricardorobles.es/mcp --scope user \
  --header "X-Jirrabit-Instance-Url: https://jirrabit.ricardorobles.es" \
  --header "X-Jirrabit-Api-Key: $JIRRABIT_API_KEY"

# The task server: a process on this machine, because it runs git.
claude mcp add task-rabbit /absolute/path/to/bin/task-rabbit-mcp --scope user \
  -e TASK_RABBIT_URL=http://localhost:8000 \
  -e TASK_RABBIT_API_KEY="$JIRRABIT_API_KEY"
```

### What the agent needs to be told

The tools' own descriptions say what each one does. What they cannot say is the
order, and that some of the steps belong to the user rather than to the agent.
Copy this into `CLAUDE.md` or `AGENTS.md`:

```markdown
## Working a jirrabit task

When I say "Realiza esta tarea" (or "do this task") about a jirrabit issue, work
it end to end with the `task-rabbit` MCP server and `jirrabit-mcp`:

1. `loadTaskContext` with the issue key. Never start before this: the acceptance
   criteria, the detail in the comments and the screenshots are all in there.
2. `materializeTaskAttachments` for anything worth opening.
3. `inspectTaskRepo`. If it reports a blocker — a dirty tree, a detached HEAD —
   deal with it or tell me, do not work around it.
4. `listTaskChecks`, so you know how this project is validated.
5. Ask me for the branch name and show me the proposals from the brief. Then wait.
6. `startTaskBranch`, twice: the preview, then the preview again with `confirm`
   once I have agreed to it.
7. Do the work. You write the code; this server does not.
8. `runTaskChecks` for the roles that apply. Individual roles, not `check`.
9. `reportTaskProgress`, naming the checks you ran and their verdict.

Use jirrabit-mcp for the tracker's own operations: creating issues, transitions,
comments and worklogs that are not the report at the end.
```

Full version, with what to do when a step refuses:
[docs/agent-workflow.md](docs/agent-workflow.md).

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

## The tools

| Tool | What it does |
|---|---|
| `loadTaskContext` | One call for everything an issue knows: the issue, comments, worklogs, links, attachment metadata, branches already linked to it, custom fields, and the project wiki on request. Capped, and it says when it is cut |
| `materializeTaskAttachments` | Writes an issue's attachments to disk and returns paths, so files are read as files instead of as megabytes of base64 |
| `inspectTaskRepo` | Whether a checkout is ready (git state, blockers) and how this project expects to be validated, in one call |
| `startTaskBranch` | Creates the branch, in two steps: a preview with a token, then the token plus the user's answers |
| `listTaskChecks` | The checks this project declares, by role, with the file each came from |
| `runTaskChecks` | Runs the checks for the roles you name. No command argument exists |
| `reportTaskProgress` | Posts what was done to the issue, moves the status if the workflow allows, and links the branch |

Roles are fixed across every repository — `format`, `lint`, `typecheck`, `test`,
`build`, `check` — so "the lint" is the same request whatever language the project
is in. Only the commands differ, and each result names the file that declared it.

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

## The image

```bash
docker run -d --name task-rabbit-mcp -p 127.0.0.1:8083:8083 \
  -e TASK_RABBIT_MCP_ALLOWED_HOSTS='.example.com' \
  ghcr.io/rroblf01/task-rabbit-mcp:latest
```

The image is published to GitHub Container Registry when a `vX.Y.Z` tag is
pushed, for `linux/amd64` and `linux/arm64` — built `FROM scratch`, running as
`nobody`. Pin a version tag rather than `latest`. `-p 127.0.0.1:8083:8083` binds
the published port to loopback on purpose: the reverse proxy on the same host
reaches it, and nothing else does.

Attachments are written under `/tmp/task-rabbit` in the container, which is
created in the image with mode 1777 so the `nobody` user can write there. Point
`TASK_RABBIT_WORKSPACE` at a mounted volume to keep them past the container's
lifetime.

The image ships **no git**, so the git tools — `inspectTaskRepo` and
`startTaskBranch` — will refuse with a message saying so. That is deliberate: a
`git` binary on the far side of a tool that creates branches turns any bug in the
tool layer into remote code execution. Run the binary directly over stdio, which
is the shape this server is built for:

```json
{
  "mcp": {
    "task-rabbit": {
      "type": "local",
      "command": ["/absolute/path/to/bin/task-rabbit-mcp"],
      "environment": {
        "TASK_RABBIT_URL": "https://jirrabit.example.com",
        "TASK_RABBIT_API_KEY": "…"
      },
      "enabled": true
    }
  }
}
```

To build the image yourself instead, `docker build -t task-rabbit-mcp .` from a
checkout.

> The first published package is **private** by default even when the repository
> is public, so `docker pull` fails for everyone but you until you set it to
> public once: the package's page on GitHub → Settings → Change visibility.

## Deploying

The container image is the service shape; the binary is the shape that can do
everything.

```bash
docker compose up -d
```

The compose file publishes `127.0.0.1:8083` on the host and the server listens on
all interfaces inside the container, which is the arrangement that works: a
container bound to its own loopback is unreachable through a published port.

1. **Put it behind a reverse proxy if it should not be reachable directly.** The
   streamable HTTP endpoint is unauthenticated — authorisation happens per tool
   call through the caller's `apiKey`, but nothing stops an unauthenticated client
   from making calls, and anyone who reaches the endpoint can consume it.
2. **Require HTTPS at the proxy.** The transport speaks plain HTTP; terminate TLS in
   front of it, and forward the client's address if you rate limit.
3. **Pin to a release** rather than tracking `latest`.

Two things the container cannot do, by design:

- **The git tools.** The image ships no `git`, so `inspectTaskRepo` and
  `startTaskBranch` refuse with a message saying so. Run the binary directly over
  stdio for those — which is also the better shape for the whole server, since
  there is then exactly one caller and it is the user.
- **A default jirrabit instance or `TASK_RABBIT_REPOS` on a public listener.** The
  server refuses to start rather than run those where strangers can reach them.
  Callers pass their own credentials per call, or headers.

If jirrabit is on the same host and reachable by name, point `TASK_RABBIT_URL` at
it. If jirrabit is in a container, add `host.docker.internal` to jirrabit's
`JIRRABIT_ALLOWED_HOSTS` or Django answers 400.

### Behind a reverse proxy

```nginx
location /mcp {
    proxy_pass http://127.0.0.1:8083/mcp;
    proxy_http_version 1.1;
    # The streamable HTTP transport needs the upgrade headers, or an
    # initialize request never completes.
    proxy_set_header Upgrade    $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host       $host;
    proxy_buffering off;
}
```

## License

MIT. See [LICENSE](LICENSE).