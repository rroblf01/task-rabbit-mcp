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
