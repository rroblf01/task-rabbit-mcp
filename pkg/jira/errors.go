package jira

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// APIError is a non-2xx answer from jirrabit.
//
// It carries the four things needed to say something useful about the failure —
// what was attempted, which path, what status, and jirrabit's own message — and
// nothing else. In particular it never carries the API key, because this value
// ends up in log lines and tool results.
type APIError struct {
	StatusCode int
	Detail     string
	Method     string
	Path       string
}

func (e *APIError) Error() string {
	detail := e.Detail
	if detail == "" {
		detail = httpText(e.StatusCode)
	}
	return fmt.Sprintf("%s %s: %d %s", e.Method, e.Path, e.StatusCode, detail)
}

func (e *APIError) NotFound() bool     { return e.StatusCode == 404 }
func (e *APIError) Forbidden() bool    { return e.StatusCode == 403 }
func (e *APIError) Unauthorized() bool { return e.StatusCode == 401 }
func (e *APIError) Unprocessable() bool {
	return e.StatusCode == 400 || e.StatusCode == 422
}

// Message renders the failure as a sentence an agent can act on.
//
// This is the part that matters. jirrabit answers 404 for a project or issue the
// caller may not see — that is how it avoids leaking which keys exist — so a bare
// "not found" teaches an agent that keys are often wrong, when the real cause is
// membership. Every message here names the next thing to do.
func Message(err error) string {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return fmt.Sprintf("the request to jirrabit failed: %v", err)
	}
	switch {
	case apiErr.NotFound():
		return fmt.Sprintf(
			"Not found: jirrabit returned 404 for %s %s. Either the key does not exist, or the API key's owner "+
				"is not a member of its project — jirrabit hides what you cannot see behind a 404 on purpose, so "+
				"do not treat this as proof the key is wrong. (%s)",
			apiErr.Method, apiErr.Path, apiErr.Detail)
	case apiErr.Forbidden():
		return fmt.Sprintf(
			"Permission denied: the API key's user lacks the required role for %s %s. (%s)",
			apiErr.Method, apiErr.Path, apiErr.Detail)
	case apiErr.Unprocessable():
		return fmt.Sprintf(
			"jirrabit rejected the request as invalid (%d) on %s %s: %s",
			apiErr.StatusCode, apiErr.Method, apiErr.Path, apiErr.Detail)
	case apiErr.Unauthorized():
		return "jirrabit rejected the API key (401). Check the apiKey argument: it may be revoked, or it may not " +
			"belong to this instance."
	}
	return fmt.Sprintf("jirrabit answered %d for %s %s: %s",
		apiErr.StatusCode, apiErr.Method, apiErr.Path, apiErr.Detail)
}

// parseErrorBody reads jirrabit's error envelope, and falls back to a trimmed
// plain body.
//
// The envelope is {"detail": "..."} everywhere, but a reverse proxy in front of
// it will happily answer HTML, and "unexpected token '<'" is not something an
// agent can act on. So an unparseable body is reported as text, trimmed.
func parseErrorBody(raw []byte) string {
	var envelope struct {
		Detail any `json:"detail"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Detail != nil {
		switch detail := envelope.Detail.(type) {
		case string:
			if detail != "" {
				return detail
			}
		default:
			// django-ninja sends a list of per-field errors for some 422s, which is
			// more useful than any single string.
			if encoded, err := json.Marshal(detail); err == nil {
				return string(encoded)
			}
		}
	}
	body := strings.TrimSpace(string(raw))
	if body == "" {
		return ""
	}
	// Collapse whitespace: an HTML error page is one line of noise per line of
	// markup, and the message is already long enough.
	if len(body) > 400 {
		body = body[:400] + "…"
	}
	return strings.Join(strings.Fields(body), " ")
}

func httpText(status int) string {
	switch status {
	case 400:
		return "bad request"
	case 401:
		return "unauthorized"
	case 403:
		return "forbidden"
	case 404:
		return "not found"
	case 405:
		return "method not allowed"
	case 409:
		return "conflict"
	case 429:
		return "too many requests"
	default:
		return "request failed"
	}
}
