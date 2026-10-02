package tools

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/rroblf01/task-rabbit-mcp/pkg/branch"
	"github.com/rroblf01/task-rabbit-mcp/pkg/jira"
	"github.com/rroblf01/task-rabbit-mcp/pkg/schema"
)

// Defaults for the brief. Chosen to answer "what is this task" in one call without
// filling the context: an issue with thirty comments is not read end to end by a
// model anyway, and the count plus the next page number is a better answer than a
// truncated wall of text.
const (
	defaultCommentLimit = 20
	// A worklog is a list of "logged 2h on Monday" rows. Ten says whether time is
	// being tracked and roughly how much; past that it is bookkeeping.
	defaultWorklogLimit = 10
	// Attachment metadata is cheap, so all of it comes back. The bytes do not:
	// they are a separate call that writes files.
	defaultAttachmentLimit = 100
	// The wiki is one blob per project and can be long. Eight KB is a page of
	// prose, which is enough for conventions and not enough for a novel. There is
	// deliberately no argument for it: the flag is includeWiki, and a second knob
	// for how much of it is one more thing an agent has to get right.
	defaultWikiBytes = 8 << 10
)

// taskContext is the brief loadTaskContext returns.
type taskContext struct {
	Issue jira.Issue `json:"issue"`

	Comments    sliceOfComments    `json:"comments"`
	Worklogs    sliceOfWorklogs    `json:"worklogs"`
	Links       []jira.Link        `json:"links"`
	Attachments sliceOfAttachments `json:"attachments"`
	// Branches are the branches already linked to this issue. Worth returning even
	// though it is usually empty, because "somebody already started this" is the
	// answer that stops a second agent duplicating the work.
	Branches     []jira.BranchLink `json:"branches"`
	CustomFields map[string]any    `json:"customFields,omitempty"`
	Wiki         *wikiExcerpt      `json:"wiki,omitempty"`
	// SuggestedBranches are proposals for the user to choose from. The agent asks;
	// it does not pick one.
	SuggestedBranches []string `json:"suggestedBranches,omitempty"`
	// Counts is what the issue itself claims, kept beside the lists so a truncated
	// list is visibly truncated rather than looking complete.
	Counts counts `json:"counts"`
	// NextPages names the pages that hold what did not fit.
	NextPages map[string]int `json:"nextPages,omitempty"`
}

type counts struct {
	Comments    int `json:"comments"`
	Worklogs    int `json:"worklogs"`
	Links       int `json:"links"`
	Attachments int `json:"attachments"`
	Branches    int `json:"branches"`
}

type sliceOfComments struct {
	Items     []jira.Comment `json:"items"`
	Returned  int            `json:"returned"`
	Total     int            `json:"total"`
	NextPage  *int           `json:"nextPage"`
	Truncated bool           `json:"truncated"`
}

type sliceOfWorklogs struct {
	Items     []jira.WorkLog `json:"items"`
	Returned  int            `json:"returned"`
	Total     int            `json:"total"`
	NextPage  *int           `json:"nextPage"`
	Truncated bool           `json:"truncated"`
}

type sliceOfAttachments struct {
	Items    []jira.Attachment `json:"items"`
	Returned int               `json:"returned"`
	Total    int               `json:"total"`
	NextPage *int              `json:"nextPage"`
	// Note tells the agent where the bytes are, because a listing with no way to
	// read the files would look like an absence of files.
	Note string `json:"note"`
}

type wikiExcerpt struct {
	Project   string `json:"project"`
	Body      string `json:"body"`
	UpdatedAt string `json:"updatedAt,omitempty"`
	UpdatedBy string `json:"updatedBy,omitempty"`
	Truncated bool   `json:"truncated"`
	Empty     bool   `json:"empty"`
}

func registerContextTools(s *registrar, d Deps) {
	s.AddTool(mcp.NewTool("loadTaskContext",
		mcp.WithDescription(
			"Read everything about a jirrabit issue in one call: the issue itself, its comments, worklogs, "+
				"links to other issues, attachment metadata, any branches already linked to it, its custom field "+
				"values and — with includeWiki — the project wiki page.\n\n"+
				"Call this first, before touching the repository. The acceptance criteria, the detail that only "+
				"exists in the discussion, and the screenshots are all in here, and a task started without them "+
				"is a task started wrong.\n\n"+
				"What comes back is capped rather than complete: a truncated list says so and reports nextPage, "+
				"because a brief nobody can tell is truncated from is a brief that gets acted on as if it were "+
				"the whole issue. Attachment bytes are not in this response — the listing carries names, sizes and "+
				"content types, and materializeTaskAttachments is what turns them into files on disk.\n\n"+
				"suggestedBranches are proposals derived from the issue, not decisions. Show them to the user and "+
				"ask which to use; creating a branch is a separate, two-step call."),
		mcp.WithTitleAnnotation("Load task context"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithInputSchema[schema.LoadTaskContextArgs](),
	), loadTaskContext(d))
}

func loadTaskContext(d Deps) func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args schema.LoadTaskContextArgs
		if err := req.BindArguments(&args); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		client, err := d.target(ctx, args)
		if err != nil {
			return toolError(err)
		}
		issueKey := strings.ToUpper(strings.TrimSpace(args.IssueKey))
		if issueKey == "" {
			return toolErrorf("issueKey is required, such as WEB-142")
		}
		logCall("loadTaskContext", issueKey)

		out := taskContext{}
		escaped := url.PathEscape(issueKey)

		if err := client.Get(ctx, "issues/"+escaped+"/", &out.Issue); err != nil {
			return toolError(err)
		}

		comments, err := jira.List[jira.Comment](ctx, client,
			fmt.Sprintf("issues/%s/comments/", escaped),
			1, clampLimit(args.CommentLimit, defaultCommentLimit, 200))
		if err != nil {
			return toolError(err)
		}
		out.Comments = sliceOfComments{
			Items:     comments.Items,
			Returned:  len(comments.Items),
			Total:     comments.Count,
			NextPage:  comments.Next,
			Truncated: len(comments.Items) < comments.Count,
		}

		worklogs, err := jira.List[jira.WorkLog](ctx, client,
			fmt.Sprintf("issues/%s/worklogs/", escaped),
			1, clampLimit(args.WorklogLimit, defaultWorklogLimit, 200))
		if err != nil {
			return toolError(err)
		}
		out.Worklogs = sliceOfWorklogs{
			Items:     worklogs.Items,
			Returned:  len(worklogs.Items),
			Total:     worklogs.Count,
			NextPage:  worklogs.Next,
			Truncated: len(worklogs.Items) < worklogs.Count,
		}

		// Links and watchers are short lists with no paging in jirrabit, so they are
		// asked for in full and a failure is reported rather than swallowed: a brief
		// that silently dropped "this is blocked by WEB-9" is worse than an error.
		if err := client.Get(ctx, "issues/"+escaped+"/links/", &out.Links); err != nil {
			return toolError(err)
		}

		attachments, err := jira.List[jira.Attachment](ctx, client,
			fmt.Sprintf("issues/%s/attachments/", escaped),
			1, defaultAttachmentLimit)
		if err != nil {
			return toolError(err)
		}
		out.Attachments = sliceOfAttachments{
			Items:    attachments.Items,
			Returned: len(attachments.Items),
			Total:    attachments.Count,
			NextPage: attachments.Next,
			Note: "bytes are not in this listing; call materializeTaskAttachments to write them to disk " +
				"and get paths",
		}

		branches, err := jira.List[jira.BranchLink](ctx, client,
			fmt.Sprintf("issues/%s/branches/", escaped), 1, 50)
		if err != nil {
			return toolError(err)
		}
		out.Branches = branches.Items

		// Custom fields are an object keyed by slug, and a project with none
		// configured answers with an empty one rather than 404.
		var custom struct {
			CustomFields map[string]any `json:"customFields"`
		}
		if err := client.Get(ctx, "issues/"+escaped+"/custom-fields/", &custom); err != nil {
			return toolError(err)
		}
		out.CustomFields = custom.CustomFields

		if args.IncludeWiki {
			var wiki jira.Wiki
			if err := client.Get(ctx, "projects/"+url.PathEscape(out.Issue.Project)+"/wiki/", &wiki); err != nil {
				return toolError(err)
			}
			body, truncated := truncate(wiki.Body, defaultWikiBytes)
			out.Wiki = &wikiExcerpt{
				Project:   wiki.Project,
				Body:      body,
				UpdatedAt: wiki.UpdatedAt,
				UpdatedBy: wiki.UpdatedBy,
				Truncated: truncated,
				Empty:     strings.TrimSpace(wiki.Body) == "",
			}
		}

		out.SuggestedBranches = branch.SuggestBranchNames(out.Issue.Key, out.Issue.Summary)
		out.Counts = counts{
			Comments:    out.Comments.Total,
			Worklogs:    out.Worklogs.Total,
			Links:       len(out.Links),
			Attachments: out.Attachments.Total,
			Branches:    len(out.Branches),
		}
		if pages := nextPages(out); len(pages) > 0 {
			out.NextPages = pages
		}

		return jsonResult(out)
	}
}

func nextPages(out taskContext) map[string]int {
	pages := map[string]int{}
	if out.Comments.NextPage != nil {
		pages["comments"] = *out.Comments.NextPage
	}
	if out.Worklogs.NextPage != nil {
		pages["worklogs"] = *out.Worklogs.NextPage
	}
	if out.Attachments.NextPage != nil {
		pages["attachments"] = *out.Attachments.NextPage
	}
	return pages
}
