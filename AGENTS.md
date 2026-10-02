# task-rabbit-mcp

MCP server that carries a jirrabit issue through to a validated branch: it reads
the issue, checks the repository is ready, creates one branch, runs the project's
own formatters/linters/tests, and reports back to the issue.

It runs **alongside** [jirrabit-mcp](https://github.com/rroblf01/jirrabit-mcp),
which stays the CRUD surface over the issue tracker. This server is deliberately
*not* a Jira-vocabulary adapter: it has its own vocabulary, because pretending to
be something it is not would make the tool list lie to an agent.

This process is a **pure MCP server**: no AI provider credentials, no model
calls.

## Build & run

```bash
go build -o bin/task-rabbit-mcp ./cmd/task-rabbit-mcp/

./bin/task-rabbit-mcp                  # stdio (default)
./bin/task-rabbit-mcp -transport http  # streamable HTTP on :8083
```

No Makefile. `go build` / `go vet` / `go test` are the whole toolchain.

## The split of work, and why it is here

| | |
|---|---|
| The agent | decides what the task is, writes the code, asks the user |
| This server | reads the issue, inspects the repo, refuses an unsafe start, creates one branch, runs the checks, reports back |

A tool that edited source files would be a worse editor than the model and one
more thing running unattended on the machine. Everything the server does is
mechanical and checkable; everything judgement-shaped is the agent's.

## Architecture

```
cmd/task-rabbit-mcp/     # entrypoint: env parsing, transport, tool registration, instructions
cmd/healthcheck/         # sends a real MCP initialize, for the container HEALTHCHECK
cmd/phantom/             # fails if the prose names a tool that is not registered
cmd/probe/               # lists what a running server serves, as a real MCP client
internal/probe/          # the stdio client those two share
pkg/jira/                # jirrabit REST client: auth, typed errors, pool, allowlist
pkg/creds/               # per-call credentials carried from the HTTP headers
pkg/toolchain/           # DETECTION: reads files, executes nothing
pkg/checks/              # runs one declared check: no shell, a timeout, capped output
pkg/git/                 # exec.Command("git", …) wrapper
pkg/branch/              # branch name proposals derived from an issue
pkg/repos/               # jirrabit project key -> local checkout
pkg/confirm/             # two-step HMAC handshake for the branch creation
pkg/schema/              # typed argument structs
pkg/tools/               # one file per tool group
```

Request flow: a handler binds its arguments → resolves the jirrabit instance the
call names (arguments, then HTTP headers, then the operator default) → the client
calls `/api/v1/` → jirrabit answers with its own DTO → returned as MCP text
content. The repository a call acts on is resolved separately, and reported:
`repoPath`, then the operator's mapping by project key, then the working directory.

**jirrabit's DTO is returned as-is.** There is no Jira re-shaping layer here, no
ADF conversion and no cursor synthesis. An agent working on a task wants the
issue as jirrabit stores it — Markdown in, Markdown out — and every translation
between vocabularies is a place for a payload to mean something other than what
the tracker holds.

## The tools

| Tool | Reads | Writes |
|---|---|---|
| `loadTaskContext` | issue, comments, worklogs, links, attachment metadata, linked branches, custom fields, wiki | — |
| `materializeTaskAttachments` | attachment bytes | files, outside any checkout |
| `inspectTaskRepo` | git state, detected checks | — |
| `startTaskBranch` | git state | one branch, and a branch link on the issue |
| `listTaskChecks` | project files | — |
| `runTaskChecks` | — | whatever the project declares, never a shell |
| `reportTaskProgress` | the issue | a comment, an optional status, an optional branch link |

Adding one means: a `pkg/schema` struct, a registration in `pkg/tools.Register`, a
description that says what it does **not** do, and a phantom check that stays
green. The prose is part of the interface and no test other than `cmd/phantom`
reads it.

## Design decisions that are not obvious from the code

- **Detection reads files; it never runs them.** The tempting way to ask a
  Makefile what it can do is `make -qp`, and it is wrong: `$(shell …)` inside a
  makefile body executes during that query. The makefile is a file in a
  repository the server was pointed at, so parsing target names with a regex
  costs thirty lines and removes the hole entirely. Same reasoning for
  `package.json`: read the `scripts` object, do not shell out to npm.
- **Roles are fixed, commands are discovered.** The agent asks for `lint`, not
  for `ruff check .`. That is the difference between a server that works in five
  projects and one that has to be reconfigured in each, and it is why the
  detection tables are the real design here rather than the tools.
- **Detection says what exists, not whether it passes.** Two different questions,
  answered by two different tools, so a failing suite is never confused with a
  project that has no suite.
- **A formatter in write mode is opt-in per call.** `ruff format .` rewrites
  files, and the agent is usually mid-edit; a formatter that runs unasked can
  change work in progress. So a role that writes is marked `writeMode` and only
  runs when the caller says `fix: true`. `--check` variants are preferred at
  detection time wherever the tool has one.
- **Two steps for the branch, with the token signed over the preview.** Same
  reasoning as the destructive deletes in jirrabit-mcp. The first call must
  change nothing and must be the place where the consequences are visible. The
  signing key is derived with HKDF from the API key, so rotating the key
  invalidates outstanding tokens and a token cannot be replayed against another
  instance.
- **A dirty working tree is a refusal, not a warning.** The alternative —
  stashing, branching from whatever is in the tree — silently attaches
  unrelated changes to the task's branch.
- **`git check-ref-format` is the authority on branch names.** Not a regex. Git
  has rules about `~`, `^`, `:`, `?`, `*`, `[`, `\`, leading dots and the
  255-byte limit, and a hand-rolled subset is a subset.
- **Never push, commit, force or reset.** The server creates one branch from
  `origin/<base>`. Anything that touches history or a remote is the user's or the
  agent's decision, and a tool that can be talked into it is a tool that will be.
- **A published server must not know where the repository is.** Over stdio the
  client spawned this process and there is exactly one caller: the user. Over a
  network listener that stops being true, so `TASK_RABBIT_REPOS` plus `-transport
  http` is refused at start-up rather than documented as a warning.
- **The allowlist applies to caller-supplied URLs only.** An operator who set
  `TASK_RABBIT_URL` meant it, and their own jirrabit may be reachable at an
  address the allowlist would refuse. The bug that shaped this in the sibling
  project: the allowlist was assigned inside a conditional, so it silently
  vanished for any pool built without a validator. An allowlist that can be
  switched off by accident is not an allowlist.
- **Clients are cached, keys are not logged.** The pool keys on
  `url|sha256(key)` and sweeps on a TTL. A method that returned the raw key
  would undo that for the sake of a signature.
- **Credentials arrive three ways and the guards do not care which.** Tool
  arguments, `X-Task-Rabbit-Instance-Url` / `X-Task-Rabbit-Api-Key` headers
  (HTTP only) and the operator's env default, in that precedence order. A new
  credential source plugs into the resolution step, never into handlers. A key
  supplied without an instance is rejected rather than applied to the default,
  so one user's credentials can never be sent to another user's data.
- **jirrabit answers 404 for what you may not see.** Project and issue visibility
  is a membership rule, and it is enforced by hiding the row. "Not found" must be
  reported as "not found, or not yours", because the alternative teaches an agent
  that missing keys are common.
- **The prose is part of the interface.** A tool named in the server
  instructions, or in another tool's description, that is not registered makes an
  agent call it, get "tool not found", and stop. It already happened in the
  sibling project: a description pointed at a tool that did not exist for a
  release. `cmd/phantom` scans the instructions and every description and fails;
  it runs in CI and needs no jirrabit.
- **An argument a tool accepts and ignores is worse than one it refuses.** When
  the backend cannot honour an argument, leave it out of the schema so it is
  rejected as unknown, or refuse it in the handler naming what is missing. Do not
  declare it and drop it: that returns success for a write that never happened.
- **Tests are hermetic.** A fake jirrabit over `httptest` and real git
  repositories in `t.TempDir()`. Nothing in CI needs a running instance, which is
  stricter than the sibling project's flow tests and is the only way the
  destructive paths can be tested honestly on every push.
- **Attachments never touch the working tree.** A filename comes from whoever
  uploaded the file, so it is cleaned, checked to be under the target directory,
  and written outside the repository — otherwise every task with a screenshot
  shows up as untracked noise in `git status`. The tool has no argument that names
  a destination at all, which is a stronger version of the same guarantee than
  validating one.
- **A tool's verdict is not always its exit code.** `gofmt -l` prints the files it
  would change and exits 0 either way, which is right for a tool meant to be read
  by a person and a trap for anything judging on exit status: a repository with
  nine unformatted files reports "passed". `Check.NonEmptyOutputFails` marks the
  checks whose verdict is their output, and `Result.Verdict` says which was used.
  Found by running the checks against a deliberately misformatted repository, not
  by reading the code.
- **An idempotent endpoint is not idempotent under repetition.** jirrabit's branch
  link is unique per issue, branch **and commit**, so posting the same branch with a
  new sha each time is a new row every time. `reportTaskProgress` therefore links a
  branch only when the issue has no link for it, and puts the sha in the comment.
- **A remote is not necessarily a URL.** A local clone's `origin` is often a
  filesystem path, and jirrabit validates `repo_url` as a URL: sending it turned
  "the branch is now in your checkout" into a 400 and a link that silently failed.
  Only a remote with a real scheme is sent.
- **Ask git, do not guess.** `git check-ref-format --branch` is the authority on
  branch names. Writing the test against real git corrected two assumptions: it
  accepts a 300-character name and `refs/heads/nope` in that mode, while refusing
  `HEAD`. A hand-rolled regex would have been wrong in both directions.
- **An exemption list needs its own test.** `cmd/phantom` allows the instructions
  to name jirrabit-mcp's tools, since an agent running both servers has them and
  prose that never names a tool produces useless instructions. That is a hole in
  the check by design, so the list is tested for the reason each entry exists, and
  the verb pattern covers both vocabularies — otherwise prose naming a
  jirrabit-mcp tool that no longer existed would pass, which is the same failure
  wearing a different tool's name.
- **A map in a struct is shared by a shallow copy.** `confirm.Preview.Detail` is a
  map, so `changed := preview` and then `changed.Detail["x"] = 1` mutates the
  original and the digest stops describing what the user saw. Callers that need a
  second version build a new `Preview`, and the digest doc says so.

## Conventions

- Go 1.27, one dependency: `github.com/mark3labs/mcp-go v1.0.0`. Prefer the
  standard library over a second module; `net/http` is sufficient.
- `request.BindArguments(&args)` with a `pkg/schema` struct, `request.GetString`
  for a flat tool.
- Tool descriptions, server instructions, documentation and commit messages are
  in English.
- Return `mcp.NewToolResultError(...)` for expected failures (not found, no
  permission, dirty tree, unknown role). Return a Go `error` only for genuine
  bugs.
- Announce safety with `mcp.WithReadOnlyHintAnnotation`,
  `mcp.WithDestructiveHintAnnotation`, `mcp.WithIdempotentHintAnnotation` and
  `mcp.WithOpenWorldHintAnnotation`. Agents read these before calling.
- Every tool description says what the tool does **not** do where that is a trap
  an agent would otherwise fall into.
- The `docs/` and `AGENTS.md` files are tracked. Do **not** add a `*.md` line to
  `.gitignore`.
- The image is `FROM scratch` and the Dockerfile is the multi-arch contract.
  `ARG TARGETOS`/`ARG TARGETARCH` are declared **without a default**, because a
  declared default beats the value BuildKit injects and produces an image whose
  config says arm64 around an amd64 binary, which then fails with "exec format
  error" on every ARM host. `HEALTHCHECK` is exec-form, because scratch has no
  `/bin/sh` and a shell-form check reports healthy without running. `main.
  serverVersion` stays a `var`: `-X` is silently ignored for a `const`.

## Documentation

`README.md` has the quickstart: both servers configured together for OpenCode and
for Claude Code, and the instruction block to paste into `CLAUDE.md`/`AGENTS.md`
so "Realiza esta tarea" resolves to a specific loop rather than to improvisation.
`docs/agent-workflow.md` is the long version: the order, the steps that belong to
the user, and what to do when each step refuses.

Both are kept in step with the tools by hand, and the failure is the same one the
phantom check exists for — a documented tool that does not exist. If you register a
tool or change a name, update them.

## Verification

```bash
go test ./...
go vet ./...
gofmt -l .                                  # must print nothing

# No jirrabit and no git repository needed. Scans the instructions and every tool
# description for tool-shaped names that are not registered, and prints the count.
go run ./cmd/phantom -server ./bin/task-rabbit-mcp

# What a running server actually serves, as a real MCP client.
go run ./cmd/probe -server ./bin/task-rabbit-mcp
```

The hermetic tests are the coverage that matters: a fake jirrabit over `httptest`,
real repositories built with `git init -b main` and a bare origin, and one fixture
directory per supported toolchain so a detection table cannot rot without a test
noticing. Nothing in CI needs a jirrabit, a network or a git identity.

Two things the fakes have to work around, both of which are the guards behaving
correctly rather than obstacles to route around:

- `ValidateTarget` refuses a caller-supplied loopback URL, so the fake binds every
  interface and the tests reach it through a real address.
- The makefile-detection test plants a recipe that touches a file and fails if the
  file appears. That is the test that pins "detection never executes".

Against a real instance, drive the binary as a client rather than the packages:

```bash
TASK_RABBIT_URL=… TASK_RABBIT_API_KEY=… \
  ./bin/task-rabbit-mcp            # stdio, then send JSON-RPC lines to it
```

The full loop was verified this way: `loadTaskContext` → `inspectTaskRepo` →
two-step `startTaskBranch` → a commit → `runTaskChecks` → `reportTaskProgress`,
against a live jirrabit and a real clone, checking afterwards that the issue had
one comment, the new status and exactly one branch link.