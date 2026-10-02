# Working a jirrabit task

The instructions to paste into `CLAUDE.md` or `AGENTS.md`, so that "Realiza esta
tarea" means something specific to an agent that has both
[jirrabit-mcp](https://github.com/rroblf01/jirrabit-mcp) and `task-rabbit-mcp`
registered.

Each tool's own description says what it does. This file covers the two things
descriptions cannot: the **order**, and the steps that belong to the **user**
rather than to the agent.

The short version is in the [README](../README.md#realiza-esta-tarea); this is the
version with the failure paths.

---

## The loop

Nine steps. The order is the point, and so is which ones stop and wait.

### 1. `loadTaskContext(issueKey)`

Always first, always before the repository. The brief carries the summary, the
description, the comments, the worklogs, the links, the attachment listing, the
branches already linked to the issue, its custom fields and — with
`includeWiki: true` — the project wiki.

- **The brief is capped and says so.** A list with `truncated: true` and a
  `nextPage` is information. Do not act on a short list as if it were the whole
  issue; fetch the rest if it matters.
- **`suggestedBranches` are proposals.** Not decisions. Step 5.
- A 404 does not prove the key is wrong: jirrabit answers 404 for an issue the
  caller cannot see. Check whether the key's owner is a member of the project
  before telling anyone an issue does not exist.

### 2. `materializeTaskAttachments(issueKey)`

For anything worth opening — a screenshot to read, a log to reproduce a failure
from. Pass `inlineText: true` for text files you expect to read straight away;
the path is returned either way.

The files land in the server's work directory, outside the checkout. They are not
in the repository, so if the task needs one committed, copy it yourself and say
so.

### 3. `inspectTaskRepo()`

Answers two questions: is this checkout ready, and how does this project expect to
be validated.

- **`ready: false` is not a formality.** A dirty working tree or a detached HEAD
  means the branch cannot be created, and it is a refusal rather than a warning:
  branching from whatever happens to be in the tree attaches unrelated work to the
  task's branch. Deal with it — commit, stash, or tell the user — rather than
  looking for a way past it.
- **`questions` are for the user.** The branch name above all. Do not answer them
  yourself and do not pick a name from the proposals without asking.
- Passing `suggestedBranch` returns a `preview` with a confirmation token, which is
  the cleanest way to show the user a concrete plan with the real state attached.

### 4. `listTaskChecks()`

Usually not needed separately — `inspectTaskRepo` already reports the checks — but
worth calling when the project is unfamiliar, to see which role maps to which
command.

### 5. Ask the user

**This is the step most easily skipped and the one that matters most.**

Put it to them in your own words, with the proposals from the brief as concrete
options rather than as an open question. Wait for the answer.

You cannot ask anybody anything yourself: a tool returns a result and you decide
what to do with it. Anything the server reported as a question is yours to raise.

### 6. `startTaskBranch` — twice

```text
startTaskBranch(issueKey, branchName, baseRef)          → preview + confirmationToken
startTaskBranch(issueKey, branchName, baseRef, confirm) → the branch exists
```

The first call **never** creates anything, even when everything is ready and you
already know the name. Show the preview to the user, get their agreement, then
send the token back.

- A refused second call means the repository changed between the two calls, or the
  token expired. Read the error; do not retry the same token.
- The token is signed over the preview, so it cannot authorise a different branch,
  a different repository, or a repository whose state has moved on.

### 7. Do the work

Yours. This server has no shell tool and never edits a file: it runs commands it
found declared in the project, and nothing else. Use your own editing tools.

### 8. `runTaskChecks(checks: [...])`

By **role** — `format`, `lint`, `typecheck`, `test`, `build`, `check` — never by
command. There is no way to hand the server a command to run.

- Prefer individual roles over `check`: the aggregate is the project's own target
  and its name never says what it runs.
- `ran: false` means the tool is not installed. That is a different problem from a
  failing check and a different fix — do not retry it.
- A `writeMode` check is skipped unless you pass `fix: true`. Only pass it when
  you mean to reformat, and say that you are.
- A failing check's output is capped at both ends: a linter's findings fit, and a
  test failure is at the end.

### 9. `reportTaskProgress(issueKey, summary, ...)`

Write the summary for whoever reads the issue next: what changed, what was
verified **and how**, what is left. Name the checks you ran and their verdict — a
report that omits how the work was verified is the thing that makes a reviewer do
it again.

- `statusId` moves the issue. If jirrabit refuses it, that is the workflow, not a
  mistake: read the error and tell the user which transition is forbidden. Use
  `listJiraStatuses` and `listJiraTransitions` on jirrabit-mcp to resolve an id
  rather than guessing one.
- The branch is linked to the issue automatically if you pass `branch`. The commit
  sha goes in the comment.

---

## Which server for what

| | |
|---|---|
| `task-rabbit-mcp` | the loop: read the issue, inspect the repo, open the branch, run the checks, report back |
| `jirrabit-mcp` | the tracker itself: create issues, transitions, comments, worklogs, sprints, searches, anything that is not the report at the end |

They are companions, not layers. If you find yourself reaching for
`jirrabit-mcp`'s `getJiraIssue` where `loadTaskContext` would have answered the
question with the comments and the attachments as well, use `loadTaskContext`.

---

## When a step refuses

| Refusal | What it means | What to do |
|---|---|---|
| `the working tree has N uncommitted changes` | Someone has work in progress here | Tell the user which files. Do not stash or discard anything yourself |
| `HEAD is detached` | There is no current branch to branch from | Ask which branch to start from |
| `the confirmation token expired` | Two minutes passed | Preview again and re-ask |
| `the repository changed between the preview and this call` | Something moved under you | Read the state again, re-ask the user |
| `git is not installed` | The server is running without git — a container, normally | This only works where the binary runs directly. Tell the user |
| `ran: false` on a check | The tool is not installed | Not a failing check. Say which tool |
| `a default instance is not served on a public listener` | Deployment configuration | Not yours to fix. Tell the user |
| 404 on an issue | It does not exist, or the key cannot see it | Check membership before reporting it missing |

---

## Things not to do

- **Do not create the branch in one step.** The two calls exist so the user sees
  what is about to happen.
- **Do not invent a branch name.** The proposals are a starting point for a
  question, not a default.
- **Do not push, commit, or reset.** This server never does and neither should you
  on its behalf without being asked.
- **Do not work around a dirty tree.** Commit it, stash it, or stop and say so.
- **Do not report work as verified without naming the check.** "Tests pass" with no
  command attached is a claim, not evidence.