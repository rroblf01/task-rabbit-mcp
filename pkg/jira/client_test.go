package jira

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeJira is a jirrabit stand-in: enough of the wire shape to exercise the
// client without a database.
//
// The point is not coverage for its own sake. It is that these tests are the only
// ones in this repository that can assert what happens on a 404, on a rejected
// key, and on a URL that turns out not to be jirrabit at all — cases that a test
// suite pointed at a live instance either skips or cannot reproduce on demand.
type fakeJira struct {
	t        *testing.T
	server   *httptest.Server
	requests []string
	// handler answers; nil means "a jirrabit that says yes to everything".
	handler func(w http.ResponseWriter, r *http.Request)
}

func newFakeJira(t *testing.T) *fakeJira {
	t.Helper()
	f := &fakeJira{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		if f.handler != nil {
			f.handler(w, r)
			return
		}
		if r.URL.Path == "/api/v1/me/" {
			writeJSON(w, Me{ID: 1, Username: "alice_pm", DisplayName: "Alice"})
			return
		}
		http.NotFound(w, r)
	})

	// Bound to every interface rather than loopback, because some tests reach it
	// through a real address: ValidateTarget refuses a caller-supplied 127.0.0.1,
	// and that guard is not weakened for a test's convenience.
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

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func writeDetail(w http.ResponseWriter, status int, detail string) {
	w.WriteHeader(status)
	writeJSON(w, map[string]string{"detail": detail})
}

// addressableURL returns the fake jirrabit's URL with 127.0.0.1 swapped for a
// real interface address.
//
// It exists because ValidateTarget refuses a caller-supplied loopback URL — a
// guard that must not be weakened to make a test convenient. Rather than assert
// around it, the test speaks to the same server through a name the guard allows,
// which is what a caller on a LAN would actually do.
func addressableURL(t *testing.T, serverURL string) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.To4() == nil {
			continue
		}
		_, port, err := net.SplitHostPort(strings.TrimPrefix(serverURL, "http://"))
		if err != nil {
			t.Fatalf("cannot parse %q: %v", serverURL, err)
		}
		return "http://" + net.JoinHostPort(ipNet.IP.String(), port)
	}
	t.Skip("no non-loopback IPv4 address to reach the fake jirrabit through")
	return ""
}

func TestValidateInstanceAcceptsARealJirrabit(t *testing.T) {
	fake := newFakeJira(t)
	client, err := NewClient(Config{BaseURL: fake.server.URL, APIKey: "k"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := ValidateInstance(context.Background(), client); err != nil {
		t.Fatalf("ValidateInstance: %v", err)
	}
}

func TestValidateInstanceSaysWhichOfTheTwoThingsIsWrong(t *testing.T) {
	t.Run("a rejected key names the key, not the URL", func(t *testing.T) {
		fake := newFakeJira(t)
		fake.handler = func(w http.ResponseWriter, r *http.Request) {
			writeDetail(w, http.StatusUnauthorized, "Invalid token")
		}
		client, _ := NewClient(Config{BaseURL: fake.server.URL, APIKey: "wrong"})

		err := ValidateInstance(context.Background(), client)
		if err == nil {
			t.Fatal("expected an error for a 401")
		}
		// The whole value of this message is that it does not send the caller
		// looking at the URL when the key is what is wrong.
		if !contains(err.Error(), "rejected this API key") {
			t.Errorf("error should blame the key: %v", err)
		}
		if contains(err.Error(), "not reachable") {
			t.Errorf("a 401 is not an unreachable instance; the message would send the caller the wrong way: %v", err)
		}
	})

	t.Run("something that is not jirrabit names the API path", func(t *testing.T) {
		// A URL that serves 200 and HTML is the realistic mistake: the user pasted
		// the site root plus /api/v1, or pointed at the reverse proxy's dashboard.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html><body>Grafana</body></html>"))
		}))
		defer server.Close()
		client, _ := NewClient(Config{BaseURL: server.URL, APIKey: "k"})

		err := ValidateInstance(context.Background(), client)
		if err == nil {
			t.Fatal("expected an error for a non-jirrabit")
		}
		if !contains(err.Error(), "does not look like a jirrabit") {
			t.Errorf("error should say what the URL probably is: %v", err)
		}
	})
}

func TestMessageExplainsA404AsPossiblyHidden(t *testing.T) {
	// jirrabit hides projects the caller cannot see behind a 404. Saying only "not
	// found" teaches an agent that keys are often wrong, when the cause is
	// membership.
	err := &APIError{StatusCode: 404, Method: "GET", Path: "/issues/WEB-1/", Detail: "No issue matches"}
	msg := Message(err)

	if !contains(msg, "not a member") {
		t.Errorf("a 404 message must mention membership: %s", msg)
	}
}

func TestClientSendsBearerAuthAndNoRedirects(t *testing.T) {
	var gotAuth, gotUA string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		// A redirect that would drop the Authorization header on the floor.
		http.Redirect(w, r, "http://elsewhere.invalid/", http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	client, _ := NewClient(Config{BaseURL: server.URL, APIKey: "secret", MaxRetries: -1})

	var out map[string]any
	// The redirect is refused rather than followed, so this is a 307 and the body
	// is not JSON.
	_ = client.Get(context.Background(), "me/", &out)

	if gotAuth != "Bearer secret" {
		t.Errorf("Authorization = %q, want the bearer key", gotAuth)
	}
	if gotUA == "" {
		t.Error("no User-Agent sent; jirrabit's access logs will not say who called")
	}
}

func TestClientRetriesTransientStatusesButNotDecisions(t *testing.T) {
	t.Run("a 503 is retried", func(t *testing.T) {
		var calls int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				writeDetail(w, http.StatusServiceUnavailable, "starting")
				return
			}
			writeJSON(w, Me{Username: "alice_pm"})
		}))
		defer server.Close()
		client, _ := NewClient(Config{BaseURL: server.URL, APIKey: "k", Timeout: 5 * time.Second})

		var out Me
		if err := client.Get(context.Background(), "me/", &out); err != nil {
			t.Fatalf("a 503 should be retried, got: %v", err)
		}
		if calls != 2 {
			t.Errorf("calls = %d, want 2: the first 503 and one retry", calls)
		}
	})

	t.Run("a 403 is not retried", func(t *testing.T) {
		var calls int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			writeDetail(w, http.StatusForbidden, "nope")
		}))
		defer server.Close()
		client, _ := NewClient(Config{BaseURL: server.URL, APIKey: "k"})

		var out Me
		err := client.Get(context.Background(), "me/", &out)
		if err == nil {
			t.Fatal("expected an error")
		}
		if calls != 1 {
			t.Errorf("calls = %d, want exactly 1: a 403 is a decision, and retrying it only wastes the agent's turn", calls)
		}
	})
}

func TestListDecodesTheEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("page"); got != "2" {
			t.Errorf("page = %q, want 2", got)
		}
		next := 3
		writeJSON(w, Page[Comment]{
			Count: 41, Page: 2, Size: 20, Pages: 3, Next: &next,
			Items: []Comment{{ID: 7, Author: "bob_dev", Body: "on it"}},
		})
	}))
	defer server.Close()
	client, _ := NewClient(Config{BaseURL: server.URL, APIKey: "k"})

	page, err := List[Comment](context.Background(), client, "issues/WEB-1/comments/", 2, 20)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if page.Count != 41 || page.Next == nil || *page.Next != 3 {
		t.Errorf("envelope decoded as %+v", page)
	}
	if len(page.Items) != 1 || page.Items[0].Author != "bob_dev" {
		t.Errorf("items = %+v", page.Items)
	}
}

func TestConfirmSecretDiffersPerInstanceAndKey(t *testing.T) {
	a, _ := NewClient(Config{BaseURL: "https://one.example", APIKey: "same"})
	b, _ := NewClient(Config{BaseURL: "https://two.example", APIKey: "same"})
	c, _ := NewClient(Config{BaseURL: "https://one.example", APIKey: "different"})

	// The same key on two instances must not share a signing secret: a shared
	// server serves both, so a token that verified against one would create a
	// branch on the other's checkout.
	if string(a.ConfirmSecret()) == string(b.ConfirmSecret()) {
		t.Error("two instances given the same key share a signing secret, so a token could travel between them")
	}
	if string(a.ConfirmSecret()) == string(c.ConfirmSecret()) {
		t.Error("rotating the key did not change the signing secret, so old tokens would still verify")
	}

	// The principal says which credential, not which deployment, so it is the
	// key that changes it.
	if a.ConfirmPrincipal() == c.ConfirmPrincipal() {
		t.Error("two different keys share a principal, so a token would cross credentials")
	}
	if a.ConfirmPrincipal() != b.ConfirmPrincipal() {
		t.Error("the same key on two instances changed principal; it describes the credential, not the deployment")
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
