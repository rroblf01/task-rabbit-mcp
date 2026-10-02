package jira

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResolveUsesTheDefaultWhenNoInstanceIsNamed(t *testing.T) {
	fake := newFakeJira(t)
	pool := NewPool(PoolOptions{
		DefaultBaseURL: fake.server.URL,
		DefaultAPIKey:  "k",
		Validator:      ValidateInstance,
	})

	client, err := pool.Resolve(context.Background(), "", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if client.BaseURL() != strings.TrimRight(fake.server.URL, "/") {
		t.Errorf("BaseURL = %q, want the configured default", client.BaseURL())
	}
}

// The one that matters for a shared server: a key with no instance must never be
// paired with the operator's default, or one tenant's credential is sent to
// another tenant's data.
func TestResolveRefusesAKeyWithNoInstance(t *testing.T) {
	fake := newFakeJira(t)
	pool := NewPool(PoolOptions{
		DefaultBaseURL: fake.server.URL,
		DefaultAPIKey:  "the-operators-key",
	})

	_, err := pool.Resolve(context.Background(), "", "a-strangers-key")
	if err == nil {
		t.Fatal("a key with no instance was accepted and paired with the default")
	}
	if !strings.Contains(err.Error(), "without an instanceUrl") {
		t.Errorf("error should say what is wrong: %v", err)
	}
}

func TestResolveRefusesALoopbackURLFromACaller(t *testing.T) {
	pool := NewPool(PoolOptions{DefaultBaseURL: "https://jirrabit.example", DefaultAPIKey: "k"})

	_, err := pool.Resolve(context.Background(), "http://localhost:8000", "k")
	if err == nil {
		t.Fatal("a caller-supplied loopback URL was accepted")
	}
	// The message has to name the way out, or the caller retries the same URL.
	if !strings.Contains(err.Error(), "TASK_RABBIT_URL") {
		t.Errorf("error should point at the operator default: %v", err)
	}
}

func TestResolveRefusesAHostOutsideTheAllowlist(t *testing.T) {
	pool := NewPool(PoolOptions{AllowedHosts: NewHostPolicy("jirrabit.example.com,.trusted.example")})

	for _, allowed := range []string{
		"https://jirrabit.example.com",
		"https://a.b.trusted.example",
	} {
		if _, err := pool.Resolve(context.Background(), allowed, "k"); err != nil {
			t.Errorf("%s should be allowed by .trusted.example: %v", allowed, err)
		}
	}

	for _, refused := range []string{
		"https://jirrabit.evil.example",
		"https://trusted.example.evil.com", // suffix match must be on a label boundary
	} {
		if _, err := pool.Resolve(context.Background(), refused, "k"); err == nil {
			t.Errorf("%s should not be allowed", refused)
		}
	}
}

// The operator's own default is not caller-supplied and must not be checked: an
// operator whose jirrabit is on localhost meant it, and the allowlist exists to
// constrain strangers, not the person who set the environment.
func TestResolveDoesNotApplyTheAllowlistToTheDefault(t *testing.T) {
	fake := newFakeJira(t)
	pool := NewPool(PoolOptions{
		DefaultBaseURL: fake.server.URL, // 127.0.0.1, which ValidateTarget would refuse
		DefaultAPIKey:  "k",
		AllowedHosts:   NewHostPolicy("jirrabit.example.com"),
		Validator:      ValidateInstance,
	})

	if _, err := pool.Resolve(context.Background(), "", ""); err != nil {
		t.Fatalf("the operator's own default was checked against the caller rules: %v", err)
	}
}

func TestResolveCachesPerURLAndKey(t *testing.T) {
	fake := newFakeJira(t)
	url := addressableURL(t, fake.server.URL)
	pool := NewPool(PoolOptions{Validator: ValidateInstance})

	first, err := pool.Resolve(context.Background(), url, "k")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	second, err := pool.Resolve(context.Background(), url, "k")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if first != second {
		t.Error("the same (url, key) built a second client, so connections are not reused")
	}

	third, err := pool.Resolve(context.Background(), url, "other-key")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if third == first {
		t.Error("a different key got the cached client, so it would be sent with the wrong credentials")
	}

	// The validator ran once per distinct pair, not once per call.
	meCalls := 0
	for _, req := range fake.requests {
		if strings.HasSuffix(req, "/me/") {
			meCalls++
		}
	}
	if meCalls != 2 {
		t.Errorf("/me/ called %d times, want 2 — one per distinct (url, key) pair", meCalls)
	}
}

func TestSweepDropsStaleClients(t *testing.T) {
	fake := newFakeJira(t)
	pool := NewPool(PoolOptions{
		Validator: ValidateInstance,
		CacheTTL:  time.Millisecond,
	})

	if _, err := pool.Resolve(context.Background(), addressableURL(t, fake.server.URL), "k"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	pool.Sweep()

	pool.mu.Lock()
	entries := len(pool.clients)
	pool.mu.Unlock()
	if entries != 0 {
		t.Errorf("%d entries survived the sweep; a rotated key would keep being used", entries)
	}
}

func TestMaxEntriesEvictsRatherThanGrowing(t *testing.T) {
	fake := newFakeJira(t)
	pool := NewPool(PoolOptions{MaxEntries: 4, Validator: ValidateInstance})

	url := addressableURL(t, fake.server.URL)
	for i := 0; i < 12; i++ {
		if _, err := pool.Resolve(context.Background(), url, string(rune('a'+i))); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}

	pool.mu.Lock()
	entries := len(pool.clients)
	pool.mu.Unlock()
	if entries > 4 {
		t.Errorf("cache holds %d entries, want at most 4", entries)
	}
}

// A URL that answers but is not jirrabit must fail once, clearly, rather than
// being cached and turning every later call into a parse error.
func TestResolveValidatesBeforeCaching(t *testing.T) {
	notJira := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<html>not here</html>"))
	}))
	defer notJira.Close()

	pool := NewPool(PoolOptions{Validator: ValidateInstance})
	if _, err := pool.Resolve(context.Background(), addressableURL(t, notJira.URL), "k"); err == nil {
		t.Fatal("expected an error")
	}

	pool.mu.Lock()
	entries := len(pool.clients)
	pool.mu.Unlock()
	if entries != 0 {
		t.Errorf("a pair that failed validation was cached (%d entries)", entries)
	}
}

func TestValidateTarget(t *testing.T) {
	refused := []string{
		"http://localhost:8000",
		"http://127.0.0.1:8000",
		"http://app.localhost",
		"http://[::1]:8000",
		"http://169.254.169.254", // cloud metadata
		"file:///etc/passwd",
		"gopher://127.0.0.1",
		"http://100.64.0.1", // carrier NAT
		"https://",
		"not a url at all",
	}
	for _, target := range refused {
		if err := ValidateTarget(target); err == nil {
			t.Errorf("%s should be refused", target)
		}
	}

	// A hostname is allowed even though it may resolve to a private address: a
	// shared server cannot know which, and refusing all names would break every
	// jirrabit on a LAN. metadata.google is here to document that.
	allowed := []string{
		"https://jirrabit.example.com",
		"http://jirrabit.internal:8000",
		"http://192.168.1.50:8000",
		"http://metadata.google",
	}
	for _, target := range allowed {
		if err := ValidateTarget(target); err != nil {
			t.Errorf("%s should be allowed: %v", target, err)
		}
	}
}
