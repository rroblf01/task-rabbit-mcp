package creds

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPContextFuncStashesBothHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set(HeaderInstanceURL, "https://jirrabit.example.com")
	req.Header.Set(HeaderAPIKey, "secret-key")

	ctx := HTTPContextFunc(context.Background(), req)
	url, key := FromContext(ctx)

	if url != "https://jirrabit.example.com" {
		t.Errorf("instance URL = %q, want the header value", url)
	}
	if key != "secret-key" {
		t.Errorf("api key = %q, want the header value", key)
	}
}

func TestHTTPContextFuncDropsAHalfPair(t *testing.T) {
	tests := map[string]map[string]string{
		"url without key": {HeaderInstanceURL: "https://jirrabit.example.com"},
		"key without url": {HeaderAPIKey: "secret-key"},
		"neither":         {},
		"empty strings":   {HeaderInstanceURL: "", HeaderAPIKey: ""},
	}

	for name, headers := range tests {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			for k, v := range headers {
				req.Header.Set(k, v)
			}

			url, key := FromContext(HTTPContextFunc(context.Background(), req))

			if url != "" || key != "" {
				t.Fatalf("got (%q, %q), want both empty: half a pair identifies no instance, "+
					"so it must not reach the resolver where the default would fill in the other half", url, key)
			}
		})
	}
}

func TestFromContextWithNoHeaders(t *testing.T) {
	url, key := FromContext(context.Background())
	if url != "" || key != "" {
		t.Fatalf("got (%q, %q) from a bare context, want both empty", url, key)
	}
}

// A stdio deployment never goes through HTTPContextFunc, so a caller has no other
// way to supply credentials. This is the path a test or another transport uses.
func TestWithCredentials(t *testing.T) {
	ctx := WithCredentials(context.Background(), "https://jirrabit.example.com", "k")
	url, key := FromContext(ctx)
	if url != "https://jirrabit.example.com" || key != "k" {
		t.Fatalf("got (%q, %q), want the pair that was put in", url, key)
	}
}
