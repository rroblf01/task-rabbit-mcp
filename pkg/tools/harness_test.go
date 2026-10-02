package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/rroblf01/task-rabbit-mcp/pkg/jira"
	"github.com/rroblf01/task-rabbit-mcp/pkg/repos"
)

// fakeJira serves the endpoints the context tools read, so their output can be
// asserted without a jirrabit and without inventing a network.
type fakeJira struct {
	t        *testing.T
	server   *httptest.Server
	requests []string
}

func newFakeJira(t *testing.T) *fakeJira {
	t.Helper()
	f := &fakeJira{t: t}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v1/me/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, jira.Me{ID: 1, Username: "alice_pm"})
	})
	mux.HandleFunc("/api/v1/issues/DEMO-42/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, jira.Issue{
			ID: 42, Key: "DEMO-42",
			Summary:     "Login times out on slow networks",
			Description: "Reproduced with a 3G profile.\n\nAcceptance:\n- no timeout under 30s",
			Status:      "In Progress", StatusCategory: "indeterminate", StatusID: 2,
			Priority: "High", Type: "Bug", Project: "DEMO",
			Assignee: "bob_dev", Reporter: "alice_pm",
			Labels: []string{"auth"}, Epic: "Checkout", SprintID: intPtr(7),
			CommentCount: 3, WorklogCount: 12, Updated: "2026-10-01T09:00:00Z",
		})
	})
	mux.HandleFunc("/api/v1/issues/DEMO-42/comments/", func(w http.ResponseWriter, r *http.Request) {
		items := make([]jira.Comment, 0, 3)
		for i := 1; i <= 3; i++ {
			items = append(items, jira.Comment{
				ID: i, Author: "carol_dev", CreatedAt: "2026-10-01T10:00:00Z",
				Body: "comment " + string(rune('0'+i)),
			})
		}
		writeJSON(w, jira.Page[jira.Comment]{Count: 3, Page: 1, Size: 20, Pages: 1, Items: items})
	})
	mux.HandleFunc("/api/v1/issues/DEMO-42/worklogs/", func(w http.ResponseWriter, r *http.Request) {
		// Twelve entries against a limit of ten: this is the case that makes the
		// truncation report the only thing standing between a model and a wrong
		// conclusion about the work logged.
		items := make([]jira.WorkLog, 0, 12)
		for i := 1; i <= 12; i++ {
			items = append(items, jira.WorkLog{ID: i, Author: "bob_dev", Minutes: 60})
		}
		next := 2
		writeJSON(w, jira.Page[jira.WorkLog]{Count: 12, Page: 1, Size: 10, Pages: 2, Next: &next, Items: items[:10]})
	})
	mux.HandleFunc("/api/v1/issues/DEMO-42/links/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []jira.Link{{ID: 3, Type: "blocks", Source: "DEMO-42", Target: "DEMO-9"}})
	})
	mux.HandleFunc("/api/v1/issues/DEMO-42/attachments/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, jira.Page[jira.Attachment]{
			Count: 2, Page: 1, Size: 100, Pages: 1,
			Items: []jira.Attachment{
				{ID: 5, Filename: "shot.png", ContentType: "image/png", Size: 1234},
				{ID: 6, Filename: "repro.log", ContentType: "text/plain", Size: 20},
			},
		})
	})
	mux.HandleFunc("/api/v1/issues/DEMO-42/branches/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, jira.Page[jira.BranchLink]{Count: 0, Page: 1, Pages: 1, Items: []jira.BranchLink{}})
	})
	mux.HandleFunc("/api/v1/issues/DEMO-42/custom-fields/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"customFields": map[string]any{"severity": "high"}})
	})
	mux.HandleFunc("/api/v1/projects/DEMO/wiki/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, jira.Wiki{Project: "DEMO", Body: "# Conventions\n\nRun make check.", UpdatedBy: "alice_pm"})
	})
	mux.HandleFunc("/api/v1/attachments/", func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/attachments/"), "/") {
		case "5":
			writeJSON(w, map[string]any{
				"id": 5, "filename": "shot.png", "content_type": "image/png", "size": 4,
				"dataUrl": "data:image/png;base64,aGVsbG8=",
			})
		case "6":
			writeJSON(w, map[string]any{
				"id": 6, "filename": "repro.log", "content_type": "text/plain", "size": 20,
				"dataUrl": "data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte("timeout after 30s\n")),
			})
		default:
			http.NotFound(w, r)
		}
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.URL.Path)
		http.NotFound(w, r)
	})

	// Bound to every interface rather than loopback: ValidateTarget refuses a
	// caller-supplied 127.0.0.1, and that guard is not weakened to make a test
	// convenient, so the tests reach the same server through a real address.
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f.server = httptest.NewUnstartedServer(mux)
	f.server.Listener.Close()
	f.server.Listener = listener
	f.server.Start()
	t.Cleanup(f.server.Close)
	return f
}

// addressable returns the fake's URL on a non-loopback address, because
// ValidateTarget refuses a caller-supplied 127.0.0.1 and that guard is not
// weakened for a test's convenience.
func addressable(t *testing.T, serverURL string) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	_, port, err := net.SplitHostPort(strings.TrimPrefix(serverURL, "http://"))
	if err != nil {
		t.Fatalf("cannot parse %q: %v", serverURL, err)
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.To4() == nil {
			continue
		}
		return "http://" + net.JoinHostPort(ipNet.IP.String(), port)
	}
	t.Skip("no non-loopback IPv4 address to reach the fake jirrabit through")
	return ""
}

// deps builds the handler dependencies. The pool has no default instance on
// purpose: the tests pass instanceUrl and apiKey on every call, so the
// per-call resolution path is what runs, and a default would quietly bypass it.
func (f *fakeJira) deps() Deps {
	pool := jira.NewPool(jira.PoolOptions{Validator: jira.ValidateInstance})
	index, _ := repos.Parse("")
	return Deps{Pool: pool, Repos: index}
}

// call runs a handler the way the server does, and decodes the result back into a
// Go value.
//
// The handler constructor is called directly rather than going through a
// registered server: what is being tested is what the tool does with jirrabit's
// answer, and mcp-go's own plumbing is not the thing under test.
func call(t *testing.T, name string, handler server.ToolHandlerFunc,
	args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()

	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("%s returned a Go error, which is reserved for bugs: %v", name, err)
	}
	if out != nil {
		text := resultText(t, result)
		if err := json.Unmarshal([]byte(text), out); err != nil {
			t.Fatalf("%s result is not JSON: %v\n%s", name, err, text)
		}
	}
	return result
}

// callExpectingError runs a handler that is supposed to refuse, and returns the
// message it refused with.
func callExpectingError(t *testing.T, name string, handler server.ToolHandlerFunc,
	args map[string]any) string {
	t.Helper()

	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("%s returned a Go error, which is reserved for bugs: %v", name, err)
	}
	if !result.IsError {
		t.Fatalf("%s was expected to refuse, but returned: %s", name, resultTextOf(result))
	}
	return resultTextOf(result)
}

func resultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if result.IsError {
		t.Fatalf("tool reported an error: %s", resultTextOf(result))
	}
	return resultTextOf(result)
}

func resultTextOf(result *mcp.CallToolResult) string {
	var b strings.Builder
	for _, content := range result.Content {
		if text, ok := content.(mcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func intPtr(v int) *int { return &v }

// osReadFile is a seam so the attachment test can assert on real bytes without
// caring where the workspace was.
func osReadFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(raw)
}

func tempWorkspace(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "work")
	t.Setenv("TASK_RABBIT_WORKSPACE", dir)
	return dir
}
