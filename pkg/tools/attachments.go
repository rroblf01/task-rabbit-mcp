package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/rroblf01/task-rabbit-mcp/pkg/jira"
	"github.com/rroblf01/task-rabbit-mcp/pkg/schema"
)

// Attachment materialisation.
//
// jirrabit stores attachments base64-encoded in the database, and the API hands
// the bytes back as a data: URL. That is right for the web UI, which needs a src
// attribute, and wrong for an agent: megabytes of base64 in a tool result is
// context spent on something the model cannot read as a file anyway.
//
// So this tool writes the bytes to disk and returns paths. The agent reads them
// with whatever file tools it has, and only pays for the bytes it actually opens.
//
// Two invariants, both deliberate:
//
//   - There is no argument that names a destination. The workspace comes from the
//     operator's configuration or the system temp directory, so no call can point
//     the write at a checkout. An issue with screenshots on it would otherwise
//     leave untracked files in `git status` that the agent then has to reason
//     about, or worse, commit.
//   - Every filename is cleaned and checked to be under the target directory.
//     A filename is whatever the uploader typed, which makes "../.ssh/authorized_keys"
//     a filename like any other to the API.

const (
	// defaultMaxAttachmentBytes matches jirrabit's own 5 MB cap. A larger file
	// cannot come from jirrabit, so anything above it is a sign the data URL was
	// not what it claimed to be.
	defaultMaxAttachmentBytes = 5 << 20
	// inlineTextBytes is how much of a text file comes back inline. Enough for a
	// log or a spec, not enough to fill the context with a dump.
	inlineTextBytes = 32 << 10
)

// textExtensions are the ones whose contents are worth returning inline.
//
// Extensions rather than content types: a browser sends text/plain for a shell
// script and application/octet-stream for plenty of text, and the thing that
// decides whether a human would want it inlined is what it is called.
var textExtensions = map[string]bool{
	".md": true, ".markdown": true, ".txt": true, ".log": true,
	".json": true, ".yaml": true, ".yml": true, ".csv": true,
	".sh": true, ".py": true, ".go": true, ".js": true, ".ts": true,
	".html": true, ".xml": true, ".sql": true, ".toml": true, ".ini": true,
	".diff": true, ".patch": true, ".env.example": true,
}

type materialised struct {
	ID       int    `json:"id"`
	Filename string `json:"filename"`
	// Path is where it was written. This is the answer the tool exists for.
	Path string `json:"path"`
	// StoredFilename is what it was actually called on disk, which can differ from
	// the original when two attachments share a name or the name was unsafe.
	StoredFilename string `json:"storedFilename"`
	ContentType    string `json:"contentType,omitempty"`
	Size           int    `json:"size"`
	// Skipped says why a file was not written, rather than leaving it out of the
	// list so the caller cannot tell an attachment that was dropped from one that
	// does not exist.
	Skipped string `json:"skipped,omitempty"`
	// InlineText is set for text files when the caller asked for it.
	InlineText string `json:"inlineText,omitempty"`
	// InlineTruncated says the inline copy was cut.
	InlineTruncated bool `json:"inlineTruncated,omitempty"`
}

type materialisedReport struct {
	Issue   string         `json:"issue"`
	Dir     string         `json:"dir"`
	Items   []materialised `json:"items"`
	Written int            `json:"written"`
	Skipped int            `json:"skipped"`
	Note    string         `json:"note"`
}

func registerAttachmentTools(s *registrar, d Deps) {
	s.AddTool(mcp.NewTool("materializeTaskAttachments",
		mcp.WithDescription(
			"Write the files attached to a jirrabit issue to disk and return their paths, so they can be read "+
				"as files instead of as base64 in the conversation.\n\n"+
				"Call this when the brief lists attachments and you need to look at them — a screenshot to read, "+
				""+
				"a log to reproduce a failure from, a spec to check against. Omit attachmentIds to fetch all of "+
				"them.\n\n"+
				"With inlineText, text files (md, txt, log, json, yaml, source files) also come back in the "+
				"result, capped at 32 KB each. That is usually enough to read a log without a second tool call; "+
				"the path is still returned for anything longer.\n\n"+
				"You cannot choose where the files go. They land in the server's work directory, never inside a "+
				"git checkout, so a task with screenshots on it leaves nothing untracked in the repository."),
		mcp.WithTitleAnnotation("Materialize attachments"),
		mcp.WithReadOnlyHintAnnotation(false), // it writes files, though only outside the repository
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithInputSchema[schema.MaterializeAttachmentsArgs](),
	), materializeTaskAttachments(d))
}

func materializeTaskAttachments(d Deps) func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args schema.MaterializeAttachmentsArgs
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
		logCall("materializeTaskAttachments", issueKey)

		// The listing first: it is the only place the original filename lives, and
		// it is how an id that is not on this issue is caught before any request
		// for it.
		listing, err := jira.List[jira.Attachment](ctx, client,
			fmt.Sprintf("issues/%s/attachments/", url.PathEscape(issueKey)), 1, defaultAttachmentLimit)
		if err != nil {
			return toolError(err)
		}
		if len(listing.Items) == 0 {
			return toolErrorf("%s has no attachments, so there is nothing to materialise", issueKey)
		}

		wanted := map[int]bool{}
		for _, id := range args.AttachmentIDs {
			wanted[id] = true
		}

		base := workspaceDir()
		dir := filepath.Join(base, sanitizeSegment(issueKey))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return toolErrorf("cannot create the work directory %s: %v.%s",
				dir, err, workspaceHint(base, err))
		}

		report := materialisedReport{
			Issue: issueKey,
			Dir:   dir,
			Note: "these files are in the server's work directory, outside any git checkout; " +
				"copy one into the repository yourself if the task needs it",
		}

		used := map[string]bool{}
		for _, attachment := range listing.Items {
			if len(wanted) > 0 && !wanted[attachment.ID] {
				continue
			}
			item, err := fetchOne(ctx, client, attachment, dir, used, args.InlineText)
			if err != nil {
				// One unreadable attachment must not lose the others: a task with a
				// corrupt screenshot should still get the log.
				item = materialised{
					ID:       attachment.ID,
					Filename: attachment.Filename,
					Skipped:  jira.Message(err),
				}
			}
			if item.Skipped != "" {
				report.Skipped++
			} else {
				report.Written++
			}
			report.Items = append(report.Items, item)
		}

		if report.Written == 0 && len(report.Items) == 0 {
			return toolErrorf("no attachment on %s matched the ids %v", issueKey, args.AttachmentIDs)
		}
		return jsonResult(report)
	}
}

func fetchOne(ctx context.Context, client *jira.Client, attachment jira.Attachment,
	dir string, used map[string]bool, inline bool) (materialised, error) {

	var payload jira.AttachmentData
	if err := client.Get(ctx, fmt.Sprintf("attachments/%d/", attachment.ID), &payload); err != nil {
		return materialised{}, err
	}

	item := materialised{
		ID:             attachment.ID,
		Filename:       attachment.Filename,
		StoredFilename: safeFilename(attachment.ID, attachment.Filename, used),
		ContentType:    attachment.ContentType,
		Size:           attachment.Size,
	}

	raw, err := decodeDataURL(payload.DataURL, maxAttachmentBytes())
	if err != nil {
		item.Skipped = err.Error()
		return item, nil
	}

	path := filepath.Join(dir, item.StoredFilename)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		item.Skipped = fmt.Sprintf("cannot write %s: %v", item.StoredFilename, err)
		return item, nil
	}
	item.Path = path

	if inline && textExtensions[strings.ToLower(filepath.Ext(item.StoredFilename))] {
		item.InlineText, item.InlineTruncated = truncate(string(raw), inlineTextBytes)
	}
	return item, nil
}

// decodeDataURL pulls the bytes out of jirrabit's data: URL.
//
// A bare base64 payload is accepted too, because the shape is the same thing with
// the prefix stripped and a client that assembles the URL by hand should not fail
// on the prefix alone.
func decodeDataURL(dataURL string, max int) ([]byte, error) {
	payload := dataURL
	if after, found := strings.CutPrefix(dataURL, "data:"); found {
		comma := strings.Index(after, ",")
		if comma < 0 {
			return nil, fmt.Errorf("the attachment's data URL is malformed: no comma separating the header")
		}
		header := after[:comma]
		if !strings.Contains(header, "base64") {
			return nil, fmt.Errorf(
				"the attachment is not base64-encoded (header %q); jirrabit always sends base64, so this "+
					"response is not what it claims to be", header)
		}
		payload = after[comma+1:]
	}
	if strings.TrimSpace(payload) == "" {
		return nil, fmt.Errorf("the attachment has no contents")
	}

	// Limited while decoding rather than after: a hostile or broken payload would
	// otherwise make the server allocate whatever it claims to be before the size
	// check ever ran.
	raw, err := io.ReadAll(io.LimitReader(
		base64.NewDecoder(base64.StdEncoding, strings.NewReader(payload)), int64(max)+1))
	if err != nil {
		return nil, fmt.Errorf("the attachment's base64 could not be decoded: %w", err)
	}
	if len(raw) > max {
		return nil, fmt.Errorf(
			"the attachment is larger than the %d byte limit this server is configured with, so it was not "+
				"written", max)
	}
	return raw, nil
}

// safeFilename turns an uploaded name into one that can be written inside dir.
//
// Three separate hazards, three separate defences:
//
//   - Path traversal: only the base name is kept, so "../../etc/passwd" becomes
//     "passwd".
//   - Collisions: two attachments called screenshot.png would overwrite each other,
//     so the id prefixes the name and a counter is appended if that is somehow
//     taken too.
//   - Hostile names: separators, control characters and leading dots are replaced,
//     because a name can end up in a shell command somewhere downstream.
func safeFilename(id int, name string, used map[string]bool) string {
	// Take the base name first, then clean: order matters, since Clean on a
	// traversal collapses it to ".." rather than removing it.
	base := filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	base = filepath.Clean(base)

	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_':
			b.WriteRune(r)
		default:
			// Spaces and everything else become a hyphen, collapsed below.
			b.WriteByte('-')
		}
	}
	cleaned := strings.Trim(b.String(), "-.")
	if cleaned == "" {
		cleaned = "attachment"
	}

	stored := strconv.Itoa(id) + "-" + cleaned
	if used[stored] {
		for n := 2; ; n++ {
			candidate := stored + "-" + strconv.Itoa(n)
			if !used[candidate] {
				stored = candidate
				break
			}
		}
	}
	used[stored] = true
	return stored
}

// sanitizeSegment makes one path element safe to join.
func sanitizeSegment(segment string) string {
	cleaned := safeFilename(0, segment, map[string]bool{})
	return strings.TrimPrefix(cleaned, "0-")
}

// workspaceHint explains the two failures an operator actually hits, both of which
// look like a bug in this server rather than a misconfigured destination.
func workspaceHint(base string, err error) string {
	if err == nil {
		return ""
	}
	switch {
	case !filepath.IsAbs(base):
		// A relative TASK_RABBIT_WORKSPACE is resolved against a working directory
		// that in a container is /.
		return " The work directory is relative, so it is resolved against the process's working " +
			"directory, which in a container is /. Set TASK_RABBIT_WORKSPACE to an absolute path, " +
			"ideally a mounted volume."
	case strings.Contains(err.Error(), "permission denied"):
		return " The process cannot write there. In the container image that means the work directory " +
			"was not created at build time or TASK_RABBIT_WORKSPACE points somewhere unwritable; " +
			"point it at a mounted volume you own, or run the binary directly."
	case os.IsNotExist(err):
		return " The parent directory does not exist and could not be created."
	default:
		return ""
	}
}

func workspaceDir() string {
	if dir := strings.TrimSpace(os.Getenv("TASK_RABBIT_WORKSPACE")); dir != "" {
		return filepath.Clean(dir)
	}
	return filepath.Join(os.TempDir(), "task-rabbit")
}

func maxAttachmentBytes() int {
	raw := strings.TrimSpace(os.Getenv("TASK_RABBIT_MAX_ATTACHMENT"))
	if raw == "" {
		return defaultMaxAttachmentBytes
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 {
		return defaultMaxAttachmentBytes
	}
	return limit
}
