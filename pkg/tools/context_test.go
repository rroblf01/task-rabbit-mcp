package tools

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestLoadTaskContextAssemblesTheBrief(t *testing.T) {
	fake := newFakeJira(t)
	d := fake.deps()

	var out taskContext
	call(t, "loadTaskContext", loadTaskContext(d), map[string]any{
		"issueKey":    "DEMO-42",
		"instanceUrl": addressable(t, fake.server.URL),
		"apiKey":      "k",
	}, &out)

	if out.Issue.Key != "DEMO-42" || out.Issue.Summary == "" {
		t.Fatalf("issue did not come through: %+v", out.Issue)
	}
	if len(out.Comments.Items) != 3 {
		t.Errorf("comments = %d, want all 3", len(out.Comments.Items))
	}
	if len(out.Links) != 1 || out.Links[0].Target != "DEMO-9" {
		t.Errorf("links = %+v; a brief that quietly dropped \"blocked by DEMO-9\" is worse than an error", out.Links)
	}
	if out.Counts.Comments != 3 || out.Counts.Links != 1 || out.Counts.Attachments != 2 {
		t.Errorf("counts = %+v, want the issue's own numbers", out.Counts)
	}
	if out.CustomFields["severity"] != "high" {
		t.Errorf("customFields = %+v", out.CustomFields)
	}
}

// The whole reason the counts sit beside the lists. A brief that returns ten
// worklogs and says nothing is indistinguishable from one that says there are
// ten.
func TestLoadTaskContextSaysWhenItTruncated(t *testing.T) {
	fake := newFakeJira(t)
	d := fake.deps()

	var out taskContext
	call(t, "loadTaskContext", loadTaskContext(d), map[string]any{
		"issueKey":    "DEMO-42",
		"instanceUrl": addressable(t, fake.server.URL),
		"apiKey":      "k",
	}, &out)

	if !out.Worklogs.Truncated {
		t.Error("10 of 12 worklogs came back and the brief does not say so")
	}
	if out.Worklogs.Total != 12 || out.Worklogs.Returned != 10 {
		t.Errorf("worklogs = %+v, want returned/total stated", out.Worklogs)
	}
	if out.NextPages["worklogs"] != 2 {
		t.Errorf("nextPages = %+v, want the page holding the rest", out.NextPages)
	}
}

func TestLoadTaskContextAttachmentListingSaysWhereTheBytesAre(t *testing.T) {
	fake := newFakeJira(t)
	d := fake.deps()

	var out taskContext
	call(t, "loadTaskContext", loadTaskContext(d), map[string]any{
		"issueKey":    "DEMO-42",
		"instanceUrl": addressable(t, fake.server.URL),
		"apiKey":      "k",
	}, &out)

	if len(out.Attachments.Items) != 2 {
		t.Fatalf("attachments = %+v", out.Attachments.Items)
	}
	if out.Attachments.Note == "" {
		t.Error("a listing with no way to read the files reads as an absence of files")
	}
}

func TestLoadTaskContextWikiIsOptIn(t *testing.T) {
	fake := newFakeJira(t)
	d := fake.deps()

	var without taskContext
	call(t, "loadTaskContext", loadTaskContext(d), map[string]any{
		"issueKey":    "DEMO-42",
		"instanceUrl": addressable(t, fake.server.URL),
		"apiKey":      "k",
	}, &without)
	if without.Wiki != nil {
		t.Error("the wiki came back without being asked for")
	}

	var with taskContext
	call(t, "loadTaskContext", loadTaskContext(d), map[string]any{
		"issueKey":    "DEMO-42",
		"includeWiki": true,
		"instanceUrl": addressable(t, fake.server.URL),
		"apiKey":      "k",
	}, &with)
	if with.Wiki == nil || with.Wiki.Body == "" {
		t.Fatalf("includeWiki returned %+v", with.Wiki)
	}
	if with.Wiki.Empty {
		t.Error("a written wiki page reported itself empty")
	}
}

func TestLoadTaskContextSuggestsBranchNames(t *testing.T) {
	fake := newFakeJira(t)
	d := fake.deps()

	var out taskContext
	call(t, "loadTaskContext", loadTaskContext(d), map[string]any{
		"issueKey":    "DEMO-42",
		"instanceUrl": addressable(t, fake.server.URL),
		"apiKey":      "k",
	}, &out)

	if len(out.SuggestedBranches) == 0 {
		t.Fatal("no branch suggestions; the agent would have to invent a name to put to the user")
	}
	if !strings.HasPrefix(out.SuggestedBranches[0], "demo-42-") {
		t.Errorf("first suggestion %q does not carry the issue key", out.SuggestedBranches[0])
	}
}

func TestLoadTaskContextRefusesWithoutAnIssueKey(t *testing.T) {
	fake := newFakeJira(t)
	d := fake.deps()

	msg := callExpectingError(t, "loadTaskContext", loadTaskContext(d), map[string]any{
		"instanceUrl": addressable(t, fake.server.URL),
		"apiKey":      "k",
	})
	if !strings.Contains(msg, "issueKey") {
		t.Errorf("the refusal should name the argument: %s", msg)
	}
}

func TestLoadTaskContextReportsAMissingIssueAsPossiblyInvisible(t *testing.T) {
	fake := newFakeJira(t)
	d := fake.deps()

	msg := callExpectingError(t, "loadTaskContext", loadTaskContext(d), map[string]any{
		"issueKey":    "NOPE-1",
		"instanceUrl": addressable(t, fake.server.URL),
		"apiKey":      "k",
	})
	// jirrabit hides projects the caller cannot see behind a 404. The message has
	// to say that, or the agent tells the user the key does not exist.
	if !strings.Contains(msg, "not a member") {
		t.Errorf("a 404 must mention membership: %s", msg)
	}
}

func TestMaterializeAttachmentsWritesFilesOutsideAnyCheckout(t *testing.T) {
	fake := newFakeJira(t)
	d := fake.deps()
	workspace := tempWorkspace(t)

	var out materialisedReport
	call(t, "materializeTaskAttachments", materializeTaskAttachments(d), map[string]any{
		"issueKey":    "DEMO-42",
		"instanceUrl": addressable(t, fake.server.URL),
		"apiKey":      "k",
	}, &out)

	if out.Written != 2 {
		t.Fatalf("written = %d, want 2: %+v", out.Written, out.Items)
	}
	// The id prefix keeps two attachments called screenshot.png apart.
	if filepath.Base(out.Items[0].Path) != "5-shot.png" {
		t.Errorf("stored as %q", filepath.Base(out.Items[0].Path))
	}
	if got := osReadFile(t, out.Items[0].Path); got != "hello" {
		t.Errorf("decoded bytes = %q, want the base64 payload", got)
	}
	// Not one path may be inside a git checkout: an untracked screenshot in the
	// working tree is noise the agent has to reason about, or worse, commits.
	for _, item := range out.Items {
		if !strings.HasPrefix(item.Path, workspace) {
			t.Errorf("%s is outside the configured work directory %s", item.Path, workspace)
		}
	}
	if out.Note == "" {
		t.Error("the report should say where the files went and that it was not the repository")
	}
}

func TestMaterializeAttachmentsInlinesTextOnRequest(t *testing.T) {
	fake := newFakeJira(t)
	d := fake.deps()
	tempWorkspace(t)

	var out materialisedReport
	call(t, "materializeTaskAttachments", materializeTaskAttachments(d), map[string]any{
		"issueKey":    "DEMO-42",
		"inlineText":  true,
		"instanceUrl": addressable(t, fake.server.URL),
		"apiKey":      "k",
	}, &out)

	var log *materialised
	for i := range out.Items {
		if out.Items[i].ID == 6 {
			log = &out.Items[i]
		}
	}
	if log == nil {
		t.Fatalf("the log attachment is missing from %+v", out.Items)
	}
	if !strings.Contains(log.InlineText, "timeout after 30s") {
		t.Errorf("inline text = %q", log.InlineText)
	}
	// A PNG is not inlined even with inlineText on: a model cannot read it as
	// text and the base64 would be wasted context.
	for _, item := range out.Items {
		if item.ID == 5 && item.InlineText != "" {
			t.Error("a PNG was inlined as text")
		}
	}
}

func TestMaterializeAttachmentsSelectsById(t *testing.T) {
	fake := newFakeJira(t)
	d := fake.deps()
	tempWorkspace(t)

	var out materialisedReport
	call(t, "materializeTaskAttachments", materializeTaskAttachments(d), map[string]any{
		"issueKey":      "DEMO-42",
		"attachmentIds": []int{6},
		"instanceUrl":   addressable(t, fake.server.URL),
		"apiKey":        "k",
	}, &out)

	if out.Written != 1 || out.Items[0].ID != 6 {
		t.Fatalf("selected one attachment and got %+v", out.Items)
	}
}

func TestMaterializeAttachmentsRefusesWhenThereAreNone(t *testing.T) {
	fake := newFakeJira(t)
	d := fake.deps()
	tempWorkspace(t)

	msg := callExpectingError(t, "materializeTaskAttachments", materializeTaskAttachments(d), map[string]any{
		"issueKey":    "DEMO-999",
		"instanceUrl": addressable(t, fake.server.URL),
		"apiKey":      "k",
	})
	if !strings.Contains(msg, "404") && !strings.Contains(msg, "Not found") {
		t.Errorf("a missing issue should say so plainly: %s", msg)
	}
}

func TestSafeFilenameCannotEscapeTheDirectory(t *testing.T) {
	// The filename is whatever the uploader typed. Three separate escapes, and the
	// sanitised name has to defeat all of them.
	tests := []struct {
		in   string
		want string
	}{
		{"../../.ssh/authorized_keys", "authorized_keys"},
		// Backslashes are normalised to separators first, so a Windows-style
		// traversal keeps only its last element. "evil.dll" is the safer outcome,
		// not an accident: the point is that nothing before the final name
		// survives.
		{"..\\..\\windows\\system32\\evil.dll", "evil.dll"},
		{"....//....//etc/passwd", "passwd"},
		{"shot.png", "shot.png"},
		{"", "attachment"},
		// Nothing sluggable at all. The "-2" suffix is the collision counter doing
		// its job: the empty name above it already took "7-attachment".
		{"...", "attachment-2"},
		{"con:aux", "con-aux"},
		{"tab\there.png", "tab-here.png"},
	}

	used := map[string]bool{}
	for _, test := range tests {
		got := safeFilename(7, test.in, used)
		if got != "7-"+test.want {
			t.Errorf("safeFilename(%q) = %q, want %q", test.in, got, "7-"+test.want)
		}
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("safeFilename(%q) = %q, which still contains a separator", test.in, got)
		}
		if strings.Contains(got, "..") {
			t.Errorf("safeFilename(%q) = %q, which still contains a traversal", test.in, got)
		}
	}
}

func TestSafeFilenameDisambiguatesCollisions(t *testing.T) {
	used := map[string]bool{}
	first := safeFilename(1, "shot.png", used)
	second := safeFilename(1, "shot.png", used)
	if first == second {
		t.Fatalf("two attachments with the same id and name collapsed onto %q, so one overwrote the other",
			first)
	}
}

func TestDecodeDataURL(t *testing.T) {
	if _, err := decodeDataURL("data:image/png;base64,aGVsbG8=", 100); err != nil {
		t.Errorf("a well-formed data URL was refused: %v", err)
	}
	if _, err := decodeDataURL("aGVsbG8=", 100); err != nil {
		t.Errorf("a bare base64 payload was refused: %v", err)
	}

	if _, err := decodeDataURL("data:image/png,notbase64", 100); err == nil {
		t.Error("a non-base64 data URL was accepted; jirrabit always sends base64")
	}
	if _, err := decodeDataURL("data:image/png;base64,", 100); err == nil {
		t.Error("an empty payload was accepted")
	}
	// 16 base64 characters decode to 12 bytes, against a 3 byte limit.
	if _, err := decodeDataURL("data:image/png;base64,aGVsbG8gd29ybGRoIQ==", 3); err == nil {
		t.Error("a payload over the limit was accepted; the limit is checked while decoding")
	}
}

func TestCheckArgumentsRejectsWhatADoNotDeclare(t *testing.T) {
	known := map[string]bool{"issueKey": true, "instanceUrl": true, "apiKey": true}

	err := checkArguments("loadTaskContext", map[string]any{"issueId": "DEMO-1"}, known)
	if err == nil {
		t.Fatal("an undeclared argument was accepted")
	}
	// The suggestion is the point: without it a typo costs a round trip and a
	// debugging session.
	if !strings.Contains(err.Error(), `did you mean "issueKey"`) {
		t.Errorf("error should suggest the right name: %v", err)
	}
	if !strings.Contains(err.Error(), "Nothing was changed") {
		t.Errorf("error should say the call was harmless: %v", err)
	}

	if err := checkArguments("loadTaskContext", map[string]any{"issueKey": "DEMO-1"}, known); err != nil {
		t.Errorf("a declared argument was refused: %v", err)
	}
	if err := checkArguments("loadTaskContext", nil, known); err != nil {
		t.Errorf("a call with no arguments was refused: %v", err)
	}
}

func TestNearestDoesNotGuessSomethingWrong(t *testing.T) {
	candidates := []string{"issueKey", "includeWiki", "apiKey"}

	// Close enough to be a typo.
	if got := nearest("issueky", candidates); got != "issueKey" {
		t.Errorf("nearest(%q) = %q, want issueKey", "issueky", got)
	}
	// Nothing close: suggesting a wrong name sends the agent off to try an
	// argument that does not exist either.
	if got := nearest("bananas", candidates); got != "" {
		t.Errorf("nearest(%q) = %q, want no suggestion", "bananas", got)
	}
	// Not a typo at all.
	if got := nearest("issueKey", candidates); got != "" {
		t.Errorf("nearest of the correct name = %q, want nothing", got)
	}
	// The candidate list may legitimately be empty.
	if got := nearest("anything", nil); got != "" {
		t.Errorf("nearest with no candidates = %q", got)
	}
}

func TestTruncateMarksThatItDid(t *testing.T) {
	text, truncated := truncate("hello", 100)
	if truncated || text != "hello" {
		t.Errorf("short text was altered: %q %v", text, truncated)
	}

	long := strings.Repeat("x", 200)
	got, truncated := truncate(long, 50)
	if !truncated {
		t.Error("a 200 byte string truncated to 50 did not say so")
	}
	if !strings.Contains(got, "150 more bytes") {
		t.Errorf("the truncation notice should say what was lost: %q", got)
	}
}

func TestWorkspaceDirHonoursTheEnvironment(t *testing.T) {
	t.Setenv("TASK_RABBIT_WORKSPACE", "/var/tmp/task-rabbit")
	if got := workspaceDir(); got != "/var/tmp/task-rabbit" {
		t.Errorf("workspaceDir() = %q", got)
	}

	t.Setenv("TASK_RABBIT_WORKSPACE", "")
	if got := workspaceDir(); !strings.HasSuffix(got, "task-rabbit") {
		t.Errorf("the default workspace %q should end in task-rabbit", got)
	}
	// The default must never be the working directory: that is the invariant the
	// whole tool exists to protect.
	if got := workspaceDir(); got == "." || got == "" {
		t.Errorf("the default workspace is %q", got)
	}
}

func TestMaxAttachmentBytesIgnoresGarbage(t *testing.T) {
	t.Setenv("TASK_RABBIT_MAX_ATTACHMENT", "not a number")
	if got := maxAttachmentBytes(); got != defaultMaxAttachmentBytes {
		t.Errorf("a malformed limit gave %d, want the default", got)
	}
	t.Setenv("TASK_RABBIT_MAX_ATTACHMENT", "-1")
	if got := maxAttachmentBytes(); got != defaultMaxAttachmentBytes {
		t.Errorf("a negative limit gave %d, want the default", got)
	}
	t.Setenv("TASK_RABBIT_MAX_ATTACHMENT", "1024")
	if got := maxAttachmentBytes(); got != 1024 {
		t.Errorf("a valid limit gave %d", got)
	}
}

func TestMaterializeWritesNothingInsideARepository(t *testing.T) {
	// Belt and braces for the invariant above: even with the workspace pointing
	// somewhere odd, nothing here may create a .git.
	fake := newFakeJira(t)
	d := fake.deps()
	dir := t.TempDir()
	t.Setenv("TASK_RABBIT_WORKSPACE", dir)

	var out materialisedReport
	call(t, "materializeTaskAttachments", materializeTaskAttachments(d), map[string]any{
		"issueKey":    "DEMO-42",
		"instanceUrl": addressable(t, fake.server.URL),
		"apiKey":      "k",
	}, &out)

	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		t.Error("materialising created a .git")
	}
	if out.Dir == "" || out.Written == 0 {
		t.Errorf("nothing was written: %+v", out)
	}
}

// The work directory is operator configuration, and both ways of getting it wrong
// look like a bug in this server unless the error says what to change.
func TestWorkspaceHintNamesTheFix(t *testing.T) {
	// A relative path is a mistake whatever the underlying error was, so the hint
	// still fires with a generic failure.
	relative := workspaceHint("task-rabbit", errors.New("something else"))
	if !strings.Contains(relative, "absolute") {
		t.Errorf("a relative work directory should say so: %q", relative)
	}

	denied := workspaceHint("/tmp/task-rabbit", &os.PathError{Op: "mkdir", Path: "/tmp", Err: syscall.EACCES})
	if !strings.Contains(denied, "permission denied") && !strings.Contains(denied, "cannot write") {
		t.Errorf("a permission failure should be named: %q", denied)
	}
	if !strings.Contains(denied, "TASK_RABBIT_WORKSPACE") || !strings.Contains(denied, "volume") {
		t.Errorf("the operator needs to be told what to set: %q", denied)
	}

	missing := workspaceHint("/nowhere/task-rabbit", &os.PathError{Op: "mkdir", Path: "/nowhere", Err: syscall.ENOENT})
	if !strings.Contains(missing, "does not exist") {
		t.Errorf("a missing parent should be named: %q", missing)
	}

	if got := workspaceHint("/absolute/path", errors.New("disk full")); got != "" {
		t.Errorf("an ordinary failure should add nothing, got %q", got)
	}
	// And a nil error must not panic: a helper that formats a hint should not be
	// the thing that crashes while reporting a different failure.
	if got := workspaceHint("/absolute/path", nil); got != "" {
		t.Errorf("a nil error should add nothing, got %q", got)
	}
}
