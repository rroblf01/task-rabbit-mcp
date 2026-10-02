package jira

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Pool hands out clients for the instance a call asks for.
//
// One deployed task-rabbit-mcp can serve many jirrabits, so the instance URL and
// API key travel with each tool call rather than living in the server's
// environment. A client per call would be correct and wasteful, so clients are
// cached and shared: connections get reused, and a short TTL lets a rotated key
// take effect without a restart.
//
// The zero value is not usable; construct one with NewPool.
type Pool struct {
	defaultBaseURL string
	defaultAPIKey  string
	timeout        time.Duration
	maxRetries     int

	ttl        time.Duration
	maxEntries int
	validator  func(ctx context.Context, c *Client) error
	allowed    *HostPolicy

	mu      sync.Mutex
	clients map[string]*pooledClient
}

type pooledClient struct {
	client  *Client
	lastUse time.Time
}

// PoolOptions configures a Pool.
type PoolOptions struct {
	// DefaultBaseURL and DefaultAPIKey are the fallback for a call that names no
	// instance. Both may be empty, in which case every call must supply its own.
	DefaultBaseURL string
	DefaultAPIKey  string
	Timeout        time.Duration
	MaxRetries     int
	// CacheTTL is how long an unused client is kept. Zero means defaultCacheTTL.
	CacheTTL time.Duration
	// MaxEntries bounds the cache. Zero means defaultMaxEntries.
	MaxEntries int
	// Validator confirms a (url, key) pair really addresses a jirrabit before it
	// is cached. Optional; see ValidateInstance.
	Validator func(ctx context.Context, c *Client) error
	// AllowedHosts is the operator's allowlist. Nil allows every host that passes
	// ValidateTarget. See NewHostPolicy and TASK_RABBIT_ALLOWED_HOSTS.
	AllowedHosts *HostPolicy
}

const (
	defaultCacheTTL   = 10 * time.Minute
	defaultMaxEntries = 256
)

// NewPool returns a Pool. It does not require a default instance: a server whose
// every caller supplies its own target is a valid deployment.
func NewPool(opts PoolOptions) *Pool {
	ttl := opts.CacheTTL
	if ttl <= 0 {
		ttl = defaultCacheTTL
	}
	entries := opts.MaxEntries
	if entries <= 0 {
		entries = defaultMaxEntries
	}
	return &Pool{
		defaultBaseURL: strings.TrimRight(strings.TrimSpace(opts.DefaultBaseURL), "/"),
		defaultAPIKey:  strings.TrimSpace(opts.DefaultAPIKey),
		timeout:        opts.Timeout,
		maxRetries:     opts.MaxRetries,
		ttl:            ttl,
		maxEntries:     entries,
		validator:      opts.Validator,
		clients:        make(map[string]*pooledClient),
		// Not inside a conditional on the validator: the allowlist and the
		// validator are unrelated concerns, and an allowlist that only takes
		// effect when a validator happens to be configured is one that can be
		// switched off by accident.
		allowed: opts.AllowedHosts,
	}
}

// Resolve returns the client for the requested instance, falling back to the
// pool's default when baseURL is empty.
//
// A caller-supplied key with no base URL is a configuration mistake rather than a
// request for the default, and is rejected: silently pairing it with whatever
// instance the operator configured would send one person's credentials to
// another person's data.
func (p *Pool) Resolve(ctx context.Context, baseURL, apiKey string) (*Client, error) {
	trimmedURL := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	trimmedKey := strings.TrimSpace(apiKey)

	// Whether the instance was named by the caller rather than defaulted, which
	// decides whether the guards apply.
	callerSupplied := trimmedURL != ""

	if trimmedURL == "" {
		if trimmedKey != "" {
			return nil, fmt.Errorf(
				"an apiKey was supplied without an instanceUrl; name the instance the key belongs to")
		}
		if p.defaultBaseURL == "" {
			return nil, fmt.Errorf(
				"no instance was given and this server has no default; pass instanceUrl and apiKey")
		}
		trimmedURL, trimmedKey = p.defaultBaseURL, p.defaultAPIKey
	}
	if trimmedKey == "" {
		return nil, fmt.Errorf("an apiKey is required for %s", trimmedURL)
	}

	// Only a caller-supplied URL is checked. The operator's own default is a
	// deliberate choice and may legitimately be localhost or a private address,
	// which ValidateTarget refuses because a remote caller must not be able to
	// name those.
	if callerSupplied {
		if err := ValidateTarget(trimmedURL); err != nil {
			return nil, err
		}
		// Checked per call rather than once at start-up, because a hosted server is
		// configured once and then fed URLs by strangers.
		if err := p.allowed.CheckTarget(trimmedURL); err != nil {
			return nil, err
		}
	}

	key := cacheKey(trimmedURL, trimmedKey)
	p.mu.Lock()
	if existing, ok := p.clients[key]; ok {
		existing.lastUse = time.Now()
		p.mu.Unlock()
		return existing.client, nil
	}
	p.mu.Unlock()

	client, err := NewClient(Config{
		BaseURL:    trimmedURL,
		APIKey:     trimmedKey,
		Timeout:    p.timeout,
		MaxRetries: p.maxRetries,
	})
	if err != nil {
		return nil, err
	}

	// Prove the pair addresses a real jirrabit before caching it. A URL that is
	// reachable but is not jirrabit must fail here rather than on every subsequent
	// call with a JSON parse error.
	if p.validator != nil {
		if err := p.validator(ctx, client); err != nil {
			return nil, err
		}
	}

	p.store(key, client)
	return client, nil
}

func (p *Pool) store(key string, client *Client) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clients[key] = &pooledClient{client: client, lastUse: time.Now()}
	if len(p.clients) <= p.maxEntries {
		return
	}
	// Evict the least recently used entry.
	var oldestKey string
	var oldest time.Time
	for k, entry := range p.clients {
		if oldestKey == "" || entry.lastUse.Before(oldest) {
			oldestKey, oldest = k, entry.lastUse
		}
	}
	delete(p.clients, oldestKey)
}

// Sweep drops clients unused within the TTL, closing their idle connections.
// Call it periodically from a long-running server: an entry swept on a TTL is what
// makes a rotated or revoked key stop being used.
func (p *Pool) Sweep() {
	p.mu.Lock()
	defer p.mu.Unlock()
	cutoff := time.Now().Add(-p.ttl)
	for key, entry := range p.clients {
		if entry.lastUse.Before(cutoff) {
			entry.client.CloseIdleConnections()
			delete(p.clients, key)
		}
	}
}

// cacheKey identifies a (url, key) pair without holding the secret in a map key
// that might be logged. The URL is readable on purpose: it is not a secret, and a
// cache key that shows which instances are in use is useful when diagnosing.
func cacheKey(baseURL, apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return baseURL + "|" + hex.EncodeToString(sum[:8])
}

// ValidateInstance confirms a client really addresses a jirrabit, by asking who
// the caller is.
//
// It runs once per (url, key) pair, on first sight, and its whole job is to turn
// a mistyped URL into one clear message instead of a JSON parse error on every
// later call. GET /me/ is the cheapest endpoint that proves three things at once:
// the URL is a jirrabit, the key is accepted, and it names a user.
func ValidateInstance(ctx context.Context, c *Client) error {
	var me Me
	if err := c.Get(ctx, "me/", &me); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Unauthorized() {
			return fmt.Errorf(
				"jirrabit at %s rejected this API key (401). Check apiKey: it may be revoked, or it may "+
					"belong to a different instance", c.BaseURL())
		}
		return fmt.Errorf(
			"this does not look like a jirrabit at %s, or it is not reachable: %s. A jirrabit's REST API "+
				"lives under /api/v1/, so the instanceUrl is the site root, without that suffix", c.BaseURL(), err)
	}
	if me.Username == "" {
		return fmt.Errorf(
			"jirrabit at %s answered /api/v1/me/ without a username, so this is probably not a jirrabit. "+
				"The instanceUrl should be the site root, not the API root", c.BaseURL())
	}
	return nil
}
