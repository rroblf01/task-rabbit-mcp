package jira

// The wire types this server reads. They mirror jirrabit's own DTOs, field for
// field, because jirrabit's DTOs are what is returned: there is no translation
// layer between them and the tool result.
//
// Two rules keep this file honest, both learned the hard way in the sibling
// project:
//
//   - A field is only declared if something reads it. A DTO here is a
//     description of what the server needs, not a copy of the API surface.
//   - Names come from the API's own JSON, not from what the field is called in
//     Python. LinkOut really does answer "source"/"target" and "sourceId", which
//     is not what the model on the other side is called.

// Me is GET /api/v1/me/.
type Me struct {
	ID          int    `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
}

// Issue is GET /api/v1/issues/{key}/.
//
// It carries comment_count and worklog_count, which is the reason one call is
// enough to know what else is worth fetching: a zero there means the list call
// would come back empty and can be skipped.
type Issue struct {
	ID          int    `json:"id"`
	Key         string `json:"key"`
	Summary     string `json:"summary"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Priority    string `json:"priority"`
	Type        string `json:"type"`
	Project     string `json:"project"`

	Assignee string `json:"assignee"`
	Reporter string `json:"reporter"`

	StoryPoints          int    `json:"story_points"`
	DueDate              string `json:"due_date"`
	EstimateMinutes      *int   `json:"estimate_minutes"`
	TimeSpentMinutes     int    `json:"time_spent_minutes"`
	TimeRemainingMinutes *int   `json:"time_remaining_minutes"`

	StatusID       int      `json:"status_id"`
	PriorityID     int      `json:"priority_id"`
	IssueTypeID    int      `json:"issue_type_id"`
	StatusCategory string   `json:"status_category"`
	Created        string   `json:"created"`
	Updated        string   `json:"updated"`
	Labels         []string `json:"labels"`
	Parent         string   `json:"parent"`
	Epic           string   `json:"epic"`
	EpicID         int      `json:"epic_id"`
	SprintID       *int     `json:"sprint_id"`
	Archived       bool     `json:"archived"`
	CommentCount   int      `json:"comment_count"`
	WorklogCount   int      `json:"worklog_count"`
}

// Comment is one row of GET /api/v1/issues/{key}/comments/.
//
// DeletedAt is never populated by default: the listing filters soft-deleted rows
// out, and ?include_deleted=1 is how a client checks that a deletion landed.
type Comment struct {
	ID        int    `json:"id"`
	Issue     string `json:"issue"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	Edited    bool   `json:"edited"`
	DeletedAt string `json:"deleted_at"`
}

// WorkLog is one row of GET /api/v1/issues/{key}/worklogs/. Time is in minutes,
// which is jirrabit's own unit and is left alone here.
type WorkLog struct {
	ID       int    `json:"id"`
	Issue    string `json:"issue"`
	Author   string `json:"author"`
	Minutes  int    `json:"minutes"`
	Comment  string `json:"comment"`
	LoggedAt string `json:"logged_at"`
}

// Link is one row of GET /api/v1/issues/{key}/links/, with both ends as issue
// keys. Type is the link type's name, so "blocks" or "is blocked by" reads
// straight off the response.
type Link struct {
	ID        int    `json:"id"`
	Type      string `json:"type"`
	Source    string `json:"source"`
	Target    string `json:"target"`
	SourceID  int    `json:"sourceId"`
	TargetID  int    `json:"targetId"`
	CreatedBy int    `json:"created_by"`
	CreatedAt string `json:"created_at"`
}

// Attachment is one row of GET /api/v1/issues/{key}/attachments/.
//
// There is no path to read: jirrabit stores files in the database, so the bytes
// come from GET /api/v1/attachments/{id}/ as a data: URL. A listing carries no
// dataUrl on purpose — a page of them would be megabytes of base64 to answer "is
// there a screenshot on this".
type Attachment struct {
	ID          int    `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	UploadedBy  string `json:"uploaded_by"`
	UploadedAt  string `json:"uploaded_at"`
}

// AttachmentData is GET /api/v1/attachments/{id}/: the metadata plus the bytes.
type AttachmentData struct {
	Attachment
	DataURL string `json:"dataUrl"`
}

// Wiki is GET /api/v1/projects/{key}/wiki/: one page per project, empty rather
// than 404 when it has never been written.
type Wiki struct {
	Project   string `json:"project"`
	Body      string `json:"body"`
	UpdatedAt string `json:"updated_at"`
	UpdatedBy string `json:"updated_by"`
}

// BranchLink is one row of GET /api/v1/issues/{key}/branches/.
//
// The model is already there: an issue can record the branch and commit that
// implement it, and the POST is idempotent on issue+branch+sha, so recording the
// same work twice is harmless.
type BranchLink struct {
	ID        int    `json:"id"`
	Issue     string `json:"issue"`
	Branch    string `json:"branch"`
	RepoURL   string `json:"repo_url"`
	CommitSHA string `json:"commit_sha"`
	Message   string `json:"message"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
}

// BranchLinkIn is the POST body for linking a branch to an issue.
type BranchLinkIn struct {
	Branch    string `json:"branch"`
	RepoURL   string `json:"repo_url,omitempty"`
	CommitSHA string `json:"commit_sha,omitempty"`
	Message   string `json:"message,omitempty"`
}

// IssueIn is the PATCH body for an issue.
//
// The field names are jirrabit's own, which is the point of not translating:
// whatever the API calls it is what the tool argument is called.
type IssueIn struct {
	Summary     *string `json:"summary,omitempty"`
	Description *string `json:"description,omitempty"`
	PriorityID  *int    `json:"priority_id,omitempty"`
	StatusID    *int    `json:"status_id,omitempty"`
	SprintID    *int    `json:"sprint_id,omitempty"`
	EpicID      *int    `json:"epic_id,omitempty"`
	Assignee    *string `json:"assignee,omitempty"`
	DueDate     *string `json:"due_date,omitempty"`
}

// CommentIn is the POST body for a comment.
type CommentIn struct {
	Body string `json:"body"`
}
