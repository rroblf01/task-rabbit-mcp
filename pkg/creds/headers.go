// Package creds carries per-call jirrabit credentials from the transport into the
// tool layer.
//
// Credentials arrive three ways, in this order: tool arguments, HTTP headers,
// and the operator's configured default. This package owns only the middle one —
// getting a value out of an HTTP header and into the context that the tool
// handlers run under. It deliberately does not resolve anything: resolution has
// to see the arguments as well, so it lives with the tool layer where both are
// visible at once.
package creds

import (
	"context"
	"net/http"
)

// Header names. Both are X-prefixed to keep them out of the way of anything
// standard, and both are checked against the same rules as an argument: a
// header URL is caller-supplied exactly like an argument URL, so it gets the
// allowlist and SSRF checks like any other.
const (
	HeaderInstanceURL = "X-Task-Rabbit-Instance-Url"
	HeaderAPIKey      = "X-Task-Rabbit-Api-Key"
)

type credentials struct {
	instanceURL string
	apiKey      string
}

type contextKey struct{}

// WithCredentials returns a context carrying credentials. Exported for tests and
// for transports other than net/http that want to supply them directly.
func WithCredentials(ctx context.Context, instanceURL, apiKey string) context.Context {
	return context.WithValue(ctx, contextKey{}, credentials{instanceURL: instanceURL, apiKey: apiKey})
}

// FromContext returns whatever credentials the transport supplied, and two empty
// strings when it supplied none. It never returns a partial pair: a header URL
// with no key is dropped whole, because the pair is what identifies an instance
// and half of one identifies nothing.
func FromContext(ctx context.Context) (instanceURL, apiKey string) {
	c, ok := ctx.Value(contextKey{}).(credentials)
	if !ok {
		return "", ""
	}
	if c.instanceURL == "" || c.apiKey == "" {
		return "", ""
	}
	return c.instanceURL, c.apiKey
}

// HTTPContextFunc stashes the header credentials on the request context. It is
// the hook mcp-go's streamable HTTP server takes.
//
// The values are never logged, never put in a cache key in the clear, and never
// returned in an error message: a key that reaches a log line is a key in a file
// that outlives the deployment.
func HTTPContextFunc(ctx context.Context, req *http.Request) context.Context {
	url := req.Header.Get(HeaderInstanceURL)
	key := req.Header.Get(HeaderAPIKey)
	if url == "" || key == "" {
		return ctx
	}
	return WithCredentials(ctx, url, key)
}
