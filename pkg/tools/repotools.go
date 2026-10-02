package tools

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/rroblf01/task-rabbit-mcp/pkg/confirm"
	"github.com/rroblf01/task-rabbit-mcp/pkg/git"
	"github.com/rroblf01/task-rabbit-mcp/pkg/jira"
	"github.com/rroblf01/task-rabbit-mcp/pkg/schema"
	"github.com/rroblf01/task-rabbit-mcp/pkg/toolchain"
)

// branchOperation and branchTargetKey name the handshake. The target includes the
// repository path, so a token issued for one checkout cannot authorise a branch in
// another.
const (
	// branchOpName and branchTargetOf pin a confirmation token to one operation on
	// one repository and one name. Without the path in the target, a token issued
	// for a checkout could authorise a branch in another.
	branchOpName      = "createBranch"
	branchTargetOf    = "%s|%s"
	branchLinkMessage = "opened by task-rabbit-mcp"
)

// expiry is when a preview's token stops being usable.
func expiry() int64 { return time.Now().Add(confirm.TTL).Unix() }

// registerRepoTools adds the two repository tools.
func (d Deps) registerRepoTools(s *registrar) {
	s.AddTool(mcp.NewTool("inspectTaskRepo",
		mcp.WithDescription(
			"Report whether this repository is ready for work, and what this project expects to be run.\n\n"+
				"Call it after loadTaskContext and before editing anything. It reads the git state — current "+
				"branch, what it tracks, whether the working tree is clean, the remotes, the default branch — and "+
				"runs the same discovery as listTaskChecks, so one call answers both \"is this ready\" and "+
				"\"how will I know it works\".\n\n"+
				"`ready` is false when the working tree is dirty or HEAD is detached, and `blockers` says which. "+
				"`questions` holds the decisions only a person can make — most of all the branch name, which "+
				"suggestedBranches proposes from the issue. Put those to the user in your own words; this "+
				"server cannot ask anybody anything.\n\n"+
				"Read-only. It also does not create the branch, even when you pass suggestedBranch: that previews "+
				"startTaskBranch so the plan can be shown with the real state attached."),
		mcp.WithTitleAnnotation("Inspect task repository"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithInputSchema[schema.InspectRepoArgs](),
	), d.inspectTaskRepo)

	s.AddTool(mcp.NewTool("startTaskBranch",
		mcp.WithDescription(
			"Create the task's branch, in two steps.\n\n"+
				"Call it with no confirm argument first. That call changes nothing: it reports the branch it "+
				"would create, the ref it would come from, what is in the way if anything, and returns a "+
				"confirmationToken. Show that to the user, get their answer, then call it again with confirm set "+
				"to that token, along with branchName and baseRef if the user chose different ones.\n\n"+
				"The token is signed over the preview, so if the repository changed in between — a file "+
				"modified, the branch taken, the base deleted — the second call refuses and nothing happens. It "+
				"is good for two minutes and it is signed with the API key, so a token from another instance "+
				"will not work.\n\n"+
				"What it runs is `git checkout -b <branch> <base>`, and nothing else. It never commits, never "+
				"pushes, never force-writes and never resets. A dirty working tree is a refusal rather than a "+
				"warning, because stashing or branching from whatever is in the tree attaches unrelated work to "+
				"the task's branch.\n\n"+
				"Passing issueKey also records the branch on the issue, so the work is findable from jirrabit. "+
				"That is the only write this tool performs outside the repository."),
		mcp.WithTitleAnnotation("Start task branch"),
		mcp.WithReadOnlyHintAnnotation(false), // it changes the working tree
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithInputSchema[schema.StartBranchArgs](),
	), d.startTaskBranch)
}

// repoReport is what inspectTaskRepo returns.
type repoReport struct {
	Repository     resolvedRepo `json:"repository"`
	RepositoryKind string       `json:"repositoryKind"`
	Git            *git.State   `json:"git"`
	Checks         []string     `json:"checks,omitempty"`
	Ready          bool         `json:"ready"`
	Blockers       []string     `json:"blockers,omitempty"`
	Questions      []string     `json:"questions,omitempty"`
	// SuggestedBranches are proposals from the issue, when the caller named a
	// project whose issue they have already read. They are proposals: the user picks.
	SuggestedBranches []string `json:"suggestedBranches,omitempty"`
	// Preview is what startTaskBranch would do for suggestedBranch, so a concrete
	// plan can be shown rather than a question about a name that might not work.
	Preview *confirm.Preview `json:"preview,omitempty"`
	// Notes is where git's absence, and anything else worth knowing, lands.
	Notes []string `json:"notes,omitempty"`
}

func (d Deps) inspectTaskRepo(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args schema.InspectRepoArgs
	if err := req.BindArguments(&args); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	repo, err := d.resolveRepo(args.ProjectKey, args.RepoPath)
	if err != nil {
		return toolErrorf("%v", err)
	}
	logCall("inspectTaskRepo", repo.Path)

	report := repoReport{
		Repository:     repo,
		RepositoryKind: kindOf(repo.Path),
		Ready:          true,
	}

	// The toolchain is reported even when git is missing, because "how do I check
	// this project" is a separate question from "is the branch ready".
	if tc, err := detectFor(repo); err == nil {
		report.Checks = checkLines(tc.Ordered())
	} else {
		report.Notes = append(report.Notes, err.Error())
	}

	state, gitErr := git.Inspect(ctx, repo.Path)
	report.Git = state
	if gitErr != nil {
		report.Ready = false
		report.Blockers = append(report.Blockers, gitErr.Error())
		report.Notes = append(report.Notes,
			"This server's own container image ships no git, because a git on the far side of a tool that "+
				"creates branches would turn any bug in the tool layer into remote code execution. Run the "+
				"server where git is installed, or mount a real toolchain.")
		return jsonResult(report)
	}

	if !state.Clean {
		report.Ready = false
		report.Blockers = append(report.Blockers,
			fmt.Sprintf("the working tree has %d uncommitted change(s): %s",
				len(state.Changes), strings.Join(state.Changes, ", ")))
	}
	if state.Detached {
		report.Ready = false
		report.Blockers = append(report.Blockers,
			"HEAD is detached, so there is no branch to branch from. Check out the branch you want to start from")
	}
	if len(state.Remotes) == 0 {
		report.Questions = append(report.Questions,
			"This repository has no remote configured, so there is nothing to branch from by name. Where "+
				"should this work come from — another checkout, or should a remote be added?")
	}

	if name := strings.TrimSpace(args.SuggestedBranch); name != "" {
		signer, err := d.signerFor(ctx, args)
		if err != nil {
			return toolErrorf("%v", err)
		}
		preview := d.preview(ctx, repo, state, name, state.BaseRef())
		preview.ConfirmationToken = signer.Issue(branchOpName, branchTarget(repo.Path, name), preview)
		report.Preview = &preview
	}

	// The two questions a branch cannot be created without.
	if strings.TrimSpace(args.SuggestedBranch) == "" {
		report.Questions = append(report.Questions,
			"What should the branch be called? A branch name cannot be derived from the code, and inventing "+
				"one is how work ends up somewhere nobody will look for it again")
	}
	if base := state.BaseRef(); base == "" && len(state.Remotes) > 0 {
		report.Questions = append(report.Questions,
			"Which ref should the branch come from? This repository's current branch has no upstream, and "+
				"origin's default branch could not be read")
	}

	return jsonResult(report)
}

func (d Deps) startTaskBranch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args schema.StartBranchArgs
	if err := req.BindArguments(&args); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	repo, err := d.resolveRepo(args.ProjectKey, args.RepoPath)
	if err != nil {
		return toolErrorf("%v", err)
	}

	state, gitErr := git.Inspect(ctx, repo.Path)
	if gitErr != nil {
		return toolErrorf("%v", gitErr)
	}
	signer, err := d.signerFor(ctx, args)
	if err != nil {
		return toolErrorf("%v", err)
	}

	name := strings.TrimSpace(args.BranchName)
	// The base is resolved once, here, and the same value goes into the preview and
	// into the checkout. Resolving it inside the preview alone meant an omitted
	// baseRef created a branch from the empty string, which git reports as a missing
	// ref — and the preview that promised origin/main was not what ran.
	base := strings.TrimSpace(args.BaseRef)
	if base == "" {
		base = state.BaseRef()
	}
	preview := d.preview(ctx, repo, state, name, base)

	if args.Confirm == "" {
		// The first call changes nothing. Always. Even with a name and a base, even
		// when everything is ready: the preview is where the consequences are
		// visible, and it is the user's turn in the conversation.
		preview.ConfirmationToken = signer.Issue(branchOpName, branchTarget(repo.Path, name), preview)
		preview.ExpiresAt = expiry()
		return jsonResult(preview)
	}

	if len(preview.Blockers) > 0 {
		// Refusing before verifying, because a token cannot make a dirty tree clean
		// and the fix is the user's, not another call.
		return toolErrorf(
			"the branch cannot be created: %s\n\nNothing was changed. Deal with that first, then call this "+
				"tool again without confirm.",
			strings.Join(preview.Blockers, "; "))
	}
	if err := signer.VerifyErr(args.Confirm, branchOpName, branchTarget(repo.Path, name), preview); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	logCall("startTaskBranch", fmt.Sprintf("%s from %s in %s", name, base, repo.Path))
	if err := git.CreateBranch(ctx, repo.Path, name, base); err != nil {
		return toolErrorf("%v", err)
	}

	preview.Confirmed = true
	preview.ConfirmationToken = ""
	preview.ExpiresAt = 0
	preview.Result = map[string]any{
		"branch":  name,
		"base":    base,
		"path":    repo.Path,
		"command": fmt.Sprintf("git checkout -b %s %s", name, base),
	}
	// The branch is recorded on the issue so the work is findable from jirrabit.
	// A failure here does not fail the branch: the checkout is already switched and
	// reporting it as a failure would send the caller looking for a branch that
	// exists.
	if issueKey := strings.ToUpper(strings.TrimSpace(args.IssueKey)); issueKey != "" {
		linked, err := d.linkBranch(ctx, args, issueKey, name, repo)
		if err != nil {
			preview.Blockers = []string{"the branch was created but could not be recorded on " + issueKey +
				": " + jira.Message(err)}
		} else {
			preview.Result.(map[string]any)["linkedToIssue"] = issueKey
			preview.Result.(map[string]any)["issueBranchLink"] = linked
		}
	}

	return jsonResult(preview)
}

// linkBranch records the branch on the issue.
func (d Deps) linkBranch(ctx context.Context, args schema.StartBranchArgs,
	issueKey, name string, repo resolvedRepo) (int, error) {

	client, err := d.target(ctx, args)
	if err != nil {
		return 0, err
	}
	var link jira.BranchLink
	body := jira.BranchLinkIn{
		Branch:  name,
		Message: branchLinkMessage,
	}
	// The remote URL is what makes the link useful months later: a branch name with
	// no repository is ambiguous the moment there is more than one.
	//
	// Only a real URL is sent. A checkout with no remote has nothing, and a local
	// clone's origin is often a filesystem path — which jirrabit validates as a URL
	// and rejects with a 400, so passing it turned "the branch is now in your
	// checkout" into "the branch was created but could not be recorded".
	body.RepoURL = firstWebURL(remoteURLs(repo))
	if err := client.Post(ctx, fmt.Sprintf("issues/%s/branches/", issueKey), body, &link); err != nil {
		return 0, err
	}
	return link.ID, nil
}

// preview builds what startTaskBranch would do, and everything standing in the way.
//
// It is also what the token is signed over, so everything a user is told has to be
// in here — which is why the blockers are part of the preview rather than a separate
// answer.
// preview builds the report. The caller passes the base already resolved, so the
// preview and the checkout cannot disagree about what the branch comes from.
func (d Deps) preview(ctx context.Context, repo resolvedRepo, state *git.State, name, base string) confirm.Preview {
	preview := confirm.Preview{
		Operation: branchOpName,
		Target:    branchTarget(repo.Path, name),
		Detail: map[string]any{
			"repository":      repo.Path,
			"repositoryBy":    repo.How,
			"currentBranch":   state.Branch,
			"currentUpstream": state.Upstream,
			"defaultBranch":   state.DefaultBranch,
			"branchName":      name,
			"baseRef":         base,
			"clean":           state.Clean,
		},
		Reversible: true,
		Undo:       "delete the branch again — nothing is committed to it by this server",
	}

	if name == "" {
		preview.Questions = append(preview.Questions,
			"What should the branch be called? Pass branchName with your answer.")
		preview.Blockers = append(preview.Blockers,
			"no branch name was given. loadTaskContext proposes some from the issue")
	} else if err := git.CheckBranchName(ctx, repo.Path, name); err != nil {
		preview.Blockers = append(preview.Blockers, err.Error())
	}

	if !state.Clean {
		preview.Blockers = append(preview.Blockers,
			fmt.Sprintf("the working tree has %d uncommitted change(s): %s. Commit them, stash them, or pass "+
				"repoPath for a clean checkout — this server will not branch from a dirty tree",
				len(state.Changes), strings.Join(state.Changes, ", ")))
	}
	if state.Detached {
		preview.Blockers = append(preview.Blockers,
			"HEAD is detached, so there is no current branch to branch from")
	}
	if base == "" {
		preview.Blockers = append(preview.Blockers,
			"no base ref could be resolved. Pass baseRef explicitly, such as origin/main")
	}

	switch {
	case name != "" && len(preview.Blockers) == 0:
		preview.Summary = fmt.Sprintf("create branch %q in %s from %s, and nothing else",
			name, repo.Path, base)
	case len(preview.Blockers) > 0:
		preview.Summary = "cannot create the branch yet: " + strings.Join(preview.Blockers, "; ")
	default:
		preview.Summary = "waiting for a branch name"
	}
	return preview
}

// signerFor builds the handshake signer for this call's jirrabit instance.
//
// The signing key comes from the API key, not from a configured secret: a signature
// whose key sits in a file on disk is not a signature, and a key derived from the
// API key is invalidated by rotating it.
func (d Deps) signerFor(ctx context.Context, args interface{ GetTarget() (string, string) }) (confirm.Signer, error) {
	client, err := d.target(ctx, args)
	if err != nil {
		return confirm.Signer{}, err
	}
	return confirm.NewSigner(client.ConfirmSecret(), client.ConfirmPrincipal(), client.BaseURL()), nil
}

func branchTarget(path, name string) string {
	return fmt.Sprintf(branchTargetOf, path, name)
}

func checkLines(checks []toolchain.Check) []string {
	lines := make([]string, 0, len(checks))
	for _, check := range checks {
		line := string(check.Role) + ": " + check.CommandLine() + "  (" + check.Source + ")"
		if check.WriteMode {
			line += "  [rewrites files]"
		}
		lines = append(lines, line)
	}
	return lines
}

// firstWebURL returns the first remote URL that is actually a URL.
//
// A remote can be a filesystem path ("/home/you/code/web") or an scp-style SSH
// reference ("git@github.com:you/web.git"). The first is not a URL at all and the
// second has no scheme, so neither is sent to a field that validates URLs — and
// that validation is jirrabit's, which is a reasonable rule for a field meant to
// be clickable.
func firstWebURL(urls []string) string {
	for _, candidate := range urls {
		parsed, err := url.Parse(candidate)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			continue
		}
		switch parsed.Scheme {
		case "http", "https", "ssh", "git":
			return candidate
		}
	}
	return ""
}

// remoteURLs returns the fetch URLs a repository knows about, for the issue link.
func remoteURLs(repo resolvedRepo) []string {
	ctx := context.Background()
	state, err := git.Inspect(ctx, repo.Path)
	if err != nil {
		return nil
	}
	var urls []string
	for _, remote := range state.Remotes {
		urls = append(urls, remote.URLs...)
	}
	return urls
}
