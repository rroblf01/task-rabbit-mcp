package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/rroblf01/task-rabbit-mcp/pkg/jira"
	"github.com/rroblf01/task-rabbit-mcp/pkg/schema"
)

func (d Deps) registerReportTools(s *registrar) {
	s.AddTool(mcp.NewTool("reportTaskProgress",
		mcp.WithDescription(
			"Write back to the jirrabit issue: post what was done, optionally move it to another status, and "+
				"make sure the branch is recorded on it.\n\n"+
				"Call this when the work is finished, or when a human reviewing the issue needs to know where "+
				"things stand. A task whose issue never hears about it is a task nobody can close, and the "+
				"branch link is what makes the code findable from the tracker months later.\n\n"+
				"The summary is Markdown and should be for whoever reads the issue next: what changed, what "+
				"was verified and how, and anything left undone or worth knowing. Name the checks you ran and "+
				"their verdict — \"make lint and make test pass\" is a claim somebody will check.\n\n"+
				"statusId moves the issue. jirrabit validates it against the workflow and refuses a "+
				"transition the workflow does not allow, so if it is rejected, read the error: that is a "+
				"property of the instance, not a mistake to work around.\n\n"+
				"The branch is recorded only if this issue has no link for it yet. jirrabit's branch link is "+
				"unique per issue, branch and commit, so reporting the same branch repeatedly with a different "+
				"commit each time would leave a row per commit — which is a commit log in the wrong place. The "+
				"commit sha goes in the comment, where it reads."),
		mcp.WithTitleAnnotation("Report task progress"),
		mcp.WithReadOnlyHintAnnotation(false), // it writes to the issue
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithInputSchema[schema.ReportProgressArgs](),
	), d.reportTaskProgress)
}

type progressReport struct {
	Issue    string        `json:"issue"`
	Comment  int           `json:"commentId"`
	Previous string        `json:"previousStatus,omitempty"`
	Status   string        `json:"status,omitempty"`
	Branch   branchOutcome `json:"branch"`
	// Notes is where anything that did not happen lands, so a caller can tell the
	// difference between "recorded" and "not recorded, and here is why".
	Notes []string `json:"notes,omitempty"`
}

type branchOutcome struct {
	// Requested is the branch the caller named.
	Requested string `json:"requested,omitempty"`
	// LinkedTo is the issue the link was created on, empty when it was already there.
	LinkedTo string `json:"linkedTo,omitempty"`
	// ExistingLink is the id of the link that was already there.
	ExistingLink int  `json:"existingLinkId,omitempty"`
	AlreadyThere bool `json:"alreadyLinked"`
}

func (d Deps) reportTaskProgress(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args schema.ReportProgressArgs
	if err := req.BindArguments(&args); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	issueKey := strings.ToUpper(strings.TrimSpace(args.IssueKey))
	if issueKey == "" {
		return toolErrorf("issueKey is required, such as WEB-142")
	}
	summary := strings.TrimSpace(args.Summary)
	if summary == "" {
		return toolErrorf(
			"summary is required. It is what the issue will say happened — an empty comment tells the next " +
				"reader nothing and is worse than no comment")
	}

	client, err := d.target(ctx, args)
	if err != nil {
		return toolError(err)
	}
	logCall("reportTaskProgress", issueKey)

	// The issue is read first, so the previous status is known before it changes and a
	// refused transition can be reported against what it was.
	var before jira.Issue
	if err := client.Get(ctx, "issues/"+issueKey+"/", &before); err != nil {
		return toolError(err)
	}

	report := progressReport{Issue: issueKey, Previous: before.Status}

	body := buildProgressComment(summary, args.Branch, args.CommitSha)

	var comment jira.Comment
	if err := client.Post(ctx, fmt.Sprintf("issues/%s/comments/", issueKey),
		jira.CommentIn{Body: body}, &comment); err != nil {
		return toolError(err)
	}
	report.Comment = comment.ID

	if args.StatusID != nil {
		patch := jira.IssueIn{StatusID: args.StatusID}
		var updated jira.Issue
		if err := client.Patch(ctx, "issues/"+issueKey+"/", patch, &updated); err != nil {
			// The comment is already posted and the work really happened, so this is
			// a note rather than a failure. Reporting it as a failure would send the
			// caller back to post the comment again.
			report.Notes = append(report.Notes, fmt.Sprintf(
				"the comment was posted but the status was not changed: %s. The issue is still %q.",
				jira.Message(err), before.Status))
		} else {
			report.Status = updated.Status
		}
	}

	if branch := strings.TrimSpace(args.Branch); branch != "" {
		report.Branch.Requested = branch
		existing, err := existingBranchLink(ctx, client, issueKey, branch)
		if err != nil {
			report.Notes = append(report.Notes, "the branch was not recorded: "+jira.Message(err))
		} else if existing != nil {
			report.Branch.AlreadyThere = true
			report.Branch.ExistingLink = existing.ID
		} else if id, err := d.postBranchLink(ctx, client, issueKey, branch, args.RepoURL); err != nil {
			report.Notes = append(report.Notes, "the branch was not recorded: "+jira.Message(err))
		} else {
			report.Branch.LinkedTo = issueKey
			report.Branch.ExistingLink = id
		}
	}

	return jsonResult(report)
}

// buildProgressComment assembles the body.
//
// The checks are part of the comment rather than left to the caller to remember,
// because a report that omits how the work was verified is the single most common
// thing that makes a reviewer re-do it.
func buildProgressComment(summary, branch, commitSha string) string {
	var b strings.Builder
	b.WriteString(summary)
	b.WriteString("\n\n---\n")

	if commitSha != "" {
		b.WriteString("Commit: `")
		b.WriteString(commitSha)
		b.WriteString("`\n")
	}
	if branch != "" {
		b.WriteString("Branch: `")
		b.WriteString(branch)
		b.WriteString("`\n")
	}
	b.WriteString("\n_Posted by task-rabbit-mcp._")
	return b.String()
}

func existingBranchLink(ctx context.Context, client *jira.Client, issueKey, branch string) (*jira.BranchLink, error) {
	page, err := jira.List[jira.BranchLink](ctx, client, fmt.Sprintf("issues/%s/branches/", issueKey), 1, 50)
	if err != nil {
		return nil, err
	}
	for i := range page.Items {
		if page.Items[i].Branch == branch {
			return &page.Items[i], nil
		}
	}
	return nil, nil
}

func (d Deps) postBranchLink(ctx context.Context, client *jira.Client, issueKey, branch, repoURL string) (int, error) {
	var link jira.BranchLink
	body := jira.BranchLinkIn{
		Branch:  branch,
		RepoURL: firstWebURL([]string{repoURL}),
		Message: branchLinkMessage,
	}
	if err := client.Post(ctx, fmt.Sprintf("issues/%s/branches/", issueKey), body, &link); err != nil {
		return 0, err
	}
	return link.ID, nil
}
