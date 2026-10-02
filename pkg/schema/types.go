// Package schema holds the typed argument structs every tool binds to.
//
// They are not decoration: mcp-go generates the tool's input schema from these
// types, so the struct and the schema an agent reads cannot drift apart. A tool
// that declares an argument it does not read is the failure mode to avoid — see
// strict.go — and the cheapest defence is not declaring it.
package schema

// Target names the jirrabit instance a call is about.
//
// Every argument struct embeds it, so multi-instance support is automatic for a
// new tool rather than something each handler remembers.
type Target struct {
	InstanceURL string `json:"instanceUrl,omitempty" jsonschema:"description,Base URL of the jirrabit instance, e.g. https://jirrabit.example.com"`
	APIKey      string `json:"apiKey,omitempty" jsonschema:"description,API key created in jirrabit under your profile"`
}

// GetTarget satisfies the interface the tool layer resolves instances through.
// Defined once here and promoted to every argument struct by embedding.
func (t Target) GetTarget() (instanceURL, apiKey string) {
	return t.InstanceURL, t.APIKey
}

// LoadTaskContextArgs is loadTaskContext's input.
type LoadTaskContextArgs struct {
	Target
	IssueKey string `json:"issueKey" jsonschema:"description,Issue key such as WEB-142"`
	// CommentLimit and WorklogLimit cap what comes back. The defaults are
	// declared here rather than applied in the handler so they are visible in the
	// schema an agent reads.
	CommentLimit int  `json:"commentLimit,omitempty" jsonschema:"description,Maximum comments to return, default 20"`
	WorklogLimit int  `json:"worklogLimit,omitempty" jsonschema:"description,Maximum worklog entries to return, default 10"`
	IncludeWiki  bool `json:"includeWiki,omitempty" jsonschema:"description,Include the project wiki page, which is where conventions and acceptance criteria often live"`
}

// MaterializeAttachmentsArgs is materializeTaskAttachments' input.
type MaterializeAttachmentsArgs struct {
	Target
	IssueKey string `json:"issueKey" jsonschema:"description,Issue key such as WEB-142"`
	// AttachmentIDs selects which files. Empty means every attachment on the
	// issue, because the common question is "what did they attach" rather than
	// "fetch this one by number".
	AttachmentIDs []int `json:"attachmentIds,omitempty" jsonschema:"description,Attachment ids to fetch. Omit for all of them"`
	// InlineText names files whose text should come back in the result too. A
	// path is not always enough: an agent asked to reproduce a bug from a log
	// needs the log.
	InlineText bool `json:"inlineText,omitempty" jsonschema:"description,Also return the contents of text files (md, txt, log, json, yaml) up to 32 KB each"`
}

// ListChecksArgs is listTaskChecks' input.
type ListChecksArgs struct {
	// ProjectKey is used to look the repository up in the operator's
	// TASK_RABBIT_REPOS mapping. Empty means the working directory.
	ProjectKey string `json:"projectKey,omitempty" jsonschema:"description,jirrabit project key, to look up the repository in the operator's mapping"`
	RepoPath   string `json:"repoPath,omitempty" jsonschema:"description,Path to the checkout. Overrides the mapping and the working directory"`
}

// RunChecksArgs is runTaskChecks' input.
type RunChecksArgs struct {
	// ProjectKey and RepoPath resolve which repository, the same way as
	// listTaskChecks.
	ProjectKey string `json:"projectKey,omitempty" jsonschema:"description,jirrabit project key, to look up the repository in the operator's mapping"`
	RepoPath   string `json:"repoPath,omitempty" jsonschema:"description,Path to the checkout. Overrides the mapping and the working directory"`
	// Roles are the categories to run. There is no command argument anywhere in
	// this server: it runs what the project declares and nothing else.
	Roles []string `json:"roles" jsonschema:"description,Roles to run: format, lint, typecheck, test, build or check"`
	// Fix permits a check that rewrites files to actually run.
	Fix bool `json:"fix,omitempty" jsonschema:"description,Allow checks that rewrite files to run. Without this they are skipped"`
}

// InspectRepoArgs is inspectTaskRepo's input.
//
// It embeds Target even though a preview needs no jirrabit of its own: the same
// tool previews startTaskBranch, and that preview carries the confirmation token,
// which is signed with the API key. A tool whose signing key depended on where it
// was called from would be a tool whose tokens were not portable.
type InspectRepoArgs struct {
	Target
	// ProjectKey and RepoPath resolve which repository, the same way as the check
	// tools: repoPath wins, then the operator's mapping for the project, then the
	// working directory.
	ProjectKey string `json:"projectKey,omitempty" jsonschema:"description,jirrabit project key, to look up the repository in the operator's mapping"`
	RepoPath   string `json:"repoPath,omitempty" jsonschema:"description,Path to the checkout. Overrides the mapping and the working directory"`
	// SuggestedBranch previews what startTaskBranch would do for that name, so the
	// user can be shown a concrete plan with the repository's real state attached.
	SuggestedBranch string `json:"suggestedBranch,omitempty" jsonschema:"description,Branch name to preview creating"`
}

// StartBranchArgs is startTaskBranch's input.
type StartBranchArgs struct {
	Target
	// ProjectKey, RepoPath and IssueKey say what this is for. IssueKey is what gets
	// the branch linked back to the issue in jirrabit.
	ProjectKey string `json:"projectKey,omitempty" jsonschema:"description,jirrabit project key, to look up the repository in the operator's mapping"`
	RepoPath   string `json:"repoPath,omitempty" jsonschema:"description,Path to the checkout. Overrides the mapping and the working directory"`
	IssueKey   string `json:"issueKey,omitempty" jsonschema:"description,Issue key this branch implements, so the branch is linked back to it"`
	// BranchName is the name to create. Omitting it on the first call is what makes
	// the preview ask for it rather than invent it.
	BranchName string `json:"branchName,omitempty" jsonschema:"description,Name for the new branch, e.g. demo-42-fix-login"`
	// BaseRef is what to branch from. Omitted means the current branch's upstream,
	// then origin's default branch, then the current branch.
	BaseRef string `json:"baseRef,omitempty" jsonschema:"description,Ref to branch from, e.g. origin/main. Omit to use the current branch's upstream"`
	// Confirm carries the token from the first call.
	Confirm string `json:"confirm,omitempty" jsonschema:"description,Token returned by the first call. Required to actually create the branch"`
}

// ReportProgressArgs is reportTaskProgress' input.
type ReportProgressArgs struct {
	Target
	// IssueKey is the issue being reported on.
	IssueKey string `json:"issueKey" jsonschema:"description,Issue key such as WEB-142"`
	// Summary is the body of the comment, in Markdown, written for whoever reads
	// the issue next.
	Summary string `json:"summary" jsonschema:"description,What was done, what was verified and what is left. Markdown"`
	// StatusID moves the issue, if the workflow allows the transition.
	StatusID *int `json:"statusId,omitempty" jsonschema:"description,Status id to move the issue to, if the workflow allows it"`
	// Branch is recorded on the issue if it is not linked there already.
	Branch string `json:"branch,omitempty" jsonschema:"description,Branch that implements the issue. Linked only if this issue has no link for it yet"`
	// CommitSha goes in the comment rather than into the branch link.
	CommitSha string `json:"commitSha,omitempty" jsonschema:"description,Commit that was verified, for the record"`
	// RepoURL is recorded alongside the branch when it is a real URL.
	RepoURL string `json:"repoUrl,omitempty" jsonschema:"description,Repository URL for the branch link"`
	// ProjectKey and RepoPath are accepted so this tool resolves a repository the
	// same way as the others, and can read one back.
	ProjectKey string `json:"projectKey,omitempty" jsonschema:"description,jirrabit project key, for resolving a repository"`
	RepoPath   string `json:"repoPath,omitempty" jsonschema:"description,Path to the checkout"`
}
