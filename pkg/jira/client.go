// Package jira is a small HTTP client for jirrabit's REST API.
//
// It is deliberately its own thing rather than a wrapper around another project's
// client: task-rabbit-mcp reads a handful of endpoints and needs three things a
// generic proxy would not give it — per-call instance resolution so one server
// can serve many jirrabits, typed errors a model can reason about, and a signing
// key per instance for the branch-creation handshake.
//
// jirrabit's own DTOs are returned as-is. There is no re-shaping layer here: an
// agent working on a task wants the issue as the tracker stores it.
package jira

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Defaults for a client that is not configured.
const (
	DefaultTimeout    = 30 * time.Second
	DefaultMaxRetries = 2
)

// apiPrefix is jirrabit's versioned REST root.
const apiPrefix = "/api/v1/"

// Client talks to one jirrabit instance with one API key.
//
// Safe for concurrent use: the *http.Client is shared, and http.Client is itself
// safe for concurrent requests.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	maxRetries int
	userAgent  string
}

// Config configures a Client.
type Config struct {
	// BaseURL is the instance root, e.g. "https://jirrabit.example.com". A
	// trailing slash is tolerated.
	BaseURL string
	// APIKey is the plaintext key from jirrabit's API keys page.
	APIKey string
	// Timeout bounds one request. Zero means DefaultTimeout.
	Timeout time.Duration
	// MaxRetries bounds retries of transient failures. Zero means
	// DefaultMaxRetries; negative disables retrying.
	MaxRetries int
	// UserAgent overrides the default User-Agent header.
	UserAgent string
}

// NewClient validates its configuration and returns a ready client.
//
// BaseURL and APIKey are both mandatory, and both failures are errors rather
// than defaults: a wrong base URL would otherwise point the agent at the wrong
// instance, and an empty key would quietly send nothing.
func NewClient(cfg Config) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, errors.New("an instance URL is required (e.g. https://jirrabit.example.com)")
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("the instance URL is not a valid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("the instance URL must use http or https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("the instance URL is missing a host: %q", base)
	}

	key := strings.TrimSpace(cfg.APIKey)
	if key == "" {
		return nil, errors.New("an API key is required (create one in jirrabit under your profile)")
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	retries := cfg.MaxRetries
	if retries == 0 {
		retries = DefaultMaxRetries
	} else if retries < 0 {
		retries = 0
	}
	agent := cfg.UserAgent
	if agent == "" {
		agent = "task-rabbit-mcp/" + Version
	}

	return &Client{
		baseURL: base,
		apiKey:  key,
		httpClient: &http.Client{
			Timeout: timeout,
			// A redirect would drop the Authorization header, or hand it to a
			// host nobody validated. Neither is worth following on an API call.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		maxRetries: retries,
		userAgent:  agent,
	}, nil
}

// Version is the UA fragment and the string the User-Agent advertises. It is set
// from main so the build can stamp it; a library that reported a wrong version to
// jirrabit's access logs would be a small, permanent lie.
var Version = "dev"

// BaseURL returns the instance root, without a trailing slash.
func (c *Client) BaseURL() string { return c.baseURL }

// CloseIdleConnections releases the pooled TCP connections this client holds.
func (c *Client) CloseIdleConnections() { c.httpClient.CloseIdleConnections() }

// retryable reports whether a status is worth another attempt.
//
// Client errors are decisions, not hiccups: a 400, 401, 403, 404 or 429 must
// surface at once, or an agent waits out a delay for an answer that will never
// change.
func retryable(status int) bool {
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	fullURL := c.baseURL + apiPrefix + strings.TrimLeft(path, "/")

	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request body: %w", err)
		}
		payload = encoded
	}

	// Short bounded backoff: 150ms, 300ms, 600ms. A jirrabit restart should not
	// cost the agent its turn, and a genuinely down instance should not be
	// hammered either.
	backoff := 150 * time.Millisecond

	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		resp, err := c.attempt(ctx, method, fullURL, payload)
		if err != nil {
			if attempt >= c.maxRetries {
				return fmt.Errorf("%s %s: %w", method, path, err)
			}
			if !sleepCtx(ctx, backoff) {
				return ctx.Err()
			}
			backoff *= 2
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			// Drain a bounded amount so the connection can be reused, then decide
			// whether the status is worth another attempt.
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()

			if retryable(resp.StatusCode) && attempt < c.maxRetries {
				if !sleepCtx(ctx, backoff) {
					return ctx.Err()
				}
				backoff *= 2
				continue
			}
			return &APIError{
				StatusCode: resp.StatusCode,
				Detail:     parseErrorBody(raw),
				Method:     method,
				Path:       "/" + strings.TrimLeft(path, "/"),
			}
		}

		defer resp.Body.Close()
		if out == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			return nil
		}
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("reading %s %s response: %w", method, path, err)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decoding %s %s response: %w (body: %s)",
				method, path, err, truncate(raw, 300))
		}
		return nil
	}
}

func (c *Client) attempt(ctx context.Context, method, fullURL string, payload []byte) (*http.Response, error) {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.httpClient.Do(req)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// --- verbs -----------------------------------------------------------------

// Get performs a GET and decodes the JSON body into out.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// Post performs a POST with a JSON body and decodes the reply into out.
func (c *Client) Post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}

// Patch performs a PATCH with a JSON body and decodes the reply into out.
func (c *Client) Patch(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPatch, path, body, out)
}

// --- handshake key ---------------------------------------------------------

// ConfirmSecret returns the HMAC key that signs branch-creation tokens for this
// instance.
//
// Derived from the API key with HKDF, salted with the instance URL, so:
//
//   - The plaintext key never leaves this package. The pool keys its cache on
//     sha256(key) and logs nothing; a method returning the raw key would undo
//     that discipline for the sake of a signature.
//   - Every instance has a different secret, even when the two were given the
//     same API key. That is what stops a token issued against one jirrabit from
//     being replayed against another: a shared server serves both, so a token
//     that travelled would be a confirmed branch creation against somebody else's
//     repository. The salt is public — it is the URL, and it is carried in the
//     token anyway — and that costs nothing, because HKDF's job is to produce
//     independent keys from one input under different salt, not to keep the salt
//     secret.
//   - Rotating or revoking the key invalidates every outstanding token, which is
//     the right behaviour and falls out of the derivation for free.
//
// HKDF rather than a bare hash because the key is already a high-entropy token:
// this separates it from any other use of the same bytes.
func (c *Client) ConfirmSecret() []byte {
	secret, err := hkdf.Key(sha256.New, []byte(c.apiKey), []byte(c.baseURL),
		"task-rabbit-mcp/confirm/v1", sha256.Size)
	if err != nil {
		// Only fails on a length the hash cannot produce, and sha256.Size is its
		// own, so this cannot happen. A zero key would be worse than a panic: it
		// would accept unsigned tokens.
		panic("task-rabbit-mcp: hkdf failed: " + err.Error())
	}
	return secret
}

// ConfirmPrincipal returns a stable identifier for whoever holds this key, used
// to bind a token to the credential that asked for it.
//
// The hash of the key, and nothing about the instance: this says which
// credential, not which deployment. Two keys belonging to the same person are two
// credentials and must not share authority, so a token issued to one does not
// authorise a branch under the other. Two instances handed the same key do share
// a principal, and that is harmless — their secrets already differ, so a token
// cannot cross between them at all.
func (c *Client) ConfirmPrincipal() string {
	sum := sha256.Sum256([]byte(c.apiKey))
	return hex.EncodeToString(sum[:16])
}

// --- page envelope ---------------------------------------------------------

// Page is jirrabit's list envelope: offset paging, not cursors.
//
// This server returns counts and a next page rather than the opaque
// nextPageToken a Jira-trained agent expects. Translating would be a lie about
// a tracker that does not do cursors, and an agent that follows an explicit
// count and page number has nothing to be confused about.
type Page[T any] struct {
	Count    int  `json:"count"`
	Page     int  `json:"page"`
	Size     int  `json:"size"`
	Pages    int  `json:"pages"`
	Next     *int `json:"next"`
	Previous *int `json:"previous"`
	Items    []T  `json:"items"`
}

// List fetches one page of a jirrabit list endpoint.
func List[T any](ctx context.Context, c *Client, path string, page, size int) (Page[T], error) {
	var out Page[T]
	full := fmt.Sprintf("%s?page=%d&size=%d", path, page, size)
	if err := c.Get(ctx, full, &out); err != nil {
		return Page[T]{}, err
	}
	return out, nil
}
