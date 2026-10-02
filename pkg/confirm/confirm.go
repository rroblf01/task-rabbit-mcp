// Package confirm is the two-step handshake for operations that change something
// the caller has to agree to.
//
// Branch creation is not destructive the way a delete is, and it is not treated as
// if it were. But it does change the working tree of a checkout the user has open,
// it is derived from a branch name only a person can pick, and a model asked to
// "start the task" will happily invent a name and proceed. So the first call does
// nothing, says exactly what would happen, and returns a token; the second call,
// with that token and the answers, does it.
//
// # What this does not do
//
// It does not obtain the human's consent. Nothing in an MCP server can: a tool
// returns a result and a model decides what to do with it, and a model that calls
// the tool twice in a row has created a branch nobody agreed to. What the two steps
// buy is that the *first* step is where the consequences are visible, and the server
// instructions require the second to be taken only after the user has answered. That
// is two layers and the second is a rule the model follows rather than a lock.
//
// Describing that as a guarantee would be the more dangerous claim, so the tool
// descriptions say what it is.
package confirm

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// TTL is how long a token stays usable.
//
// Long enough for a person to read the preview and answer, short enough that a
// token pasted into a transcript next week is not still good. There is no
// single-use enforcement: this server is stateless by design, and enforcing it
// would mean a store whose only job is to remember which tokens were spent. The
// bound that matters is that a token is signed over the preview digest, so it
// cannot be reused for a different branch or for a repository that has since
// changed.
const TTL = 2 * time.Minute

// payload is what gets signed. Every field exists to stop a token being useful for
// something other than what it was issued for.
type payload struct {
	// Op and Target pin the token to one operation on one object.
	Op     string `json:"op"`
	Target string `json:"target"`
	// Instance is the base URL. It is already implied by the signing key, since each
	// instance's secret is derived from its own API key, but carrying it means a
	// token pasted from one deployment into another is rejected with a readable
	// reason rather than a bare signature mismatch.
	Instance string `json:"inst"`
	// Principal is a hash of the API key, so two keys for the same person do not
	// share authority and revoking a key kills its outstanding tokens.
	Principal string `json:"who"`
	// Digest is the hash of the preview the caller was shown. This is what makes the
	// token a confirmation rather than a delay.
	Digest  string `json:"digest"`
	Issued  int64  `json:"iat"`
	Expires int64  `json:"exp"`
}

// Preview is what a first call returns instead of acting.
//
// The shape is meant to be read by a model and relayed to a person, so every count
// is real and the consequences are stated rather than implied.
type Preview struct {
	// Confirmed is true on the second call, when the operation actually ran.
	Confirmed bool   `json:"confirmed"`
	Operation string `json:"operation"`
	Target    string `json:"target"`
	// Summary is one sentence saying what will happen.
	Summary string `json:"summary"`
	// Detail carries whatever the operation found: the branch, the base it comes
	// from, the files in the way. It is whatever the caller put in it, so a tool can
	// report its own facts without this package knowing what they are.
	Detail map[string]any `json:"detail,omitempty"`
	// Blockers are the reasons the operation cannot run. Present alongside — not
	// instead of — the confirmation token: a preview that says "here is your token"
	// for a repository that is not ready is asking the user to agree to something
	// that will then fail.
	Blockers []string `json:"blockers,omitempty"`
	// Questions are the decisions only a person can make. This server cannot ask
	// anybody anything; it reports them and the agent puts them to the user.
	Questions []string `json:"questions,omitempty"`
	// Reversible and Undo say whether this can be taken back, and how.
	Reversible bool   `json:"reversible"`
	Undo       string `json:"undo,omitempty"`
	// ConfirmationToken goes back on the second call. ExpiresAt is a Unix time so an
	// agent can reason about it without parsing prose.
	ConfirmationToken string `json:"confirmationToken,omitempty"`
	ExpiresAt         int64  `json:"expiresAt,omitempty"`
	// Result is the operation's own outcome, once it has run.
	Result any `json:"result,omitempty"`
}

// Signer mints and verifies tokens for one instance.
type Signer struct {
	secret    []byte
	principal string
	instance  string
}

// NewSigner builds a Signer from the per-instance values the jirrabit client
// exposes. The secret and the principal never come from a configuration file: a
// signature whose key is in a file on disk is not a signature.
func NewSigner(secret []byte, principal, instance string) Signer {
	return Signer{secret: secret, principal: principal, instance: instance}
}

// digest hashes a preview.
//
// The caller must include everything the user was told, because the whole point is
// that the token is a signature over what was said.
func Digest(preview Preview) string {
	// The token fields are excluded: including them would make the digest depend on
	// itself.
	preview.ConfirmationToken = ""
	preview.ExpiresAt = 0
	preview.Result = nil
	raw, err := json.Marshal(preview)
	if err != nil {
		// Preview is a plain struct of strings, ints and slices; marshalling it
		// cannot fail. A digest of the empty string would still be stable, so there
		// is nothing to do but say so loudly.
		panic("task-rabbit-mcp: preview digest failed: " + err.Error())
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Issue mints a token for a preview.
func (s Signer) Issue(op, target string, preview Preview) string {
	now := time.Now()
	return s.sign(payload{
		Op:        op,
		Target:    target,
		Instance:  s.instance,
		Principal: s.principal,
		Digest:    Digest(preview),
		Issued:    now.Unix(),
		Expires:   now.Add(TTL).Unix(),
	})
}

func (s Signer) sign(p payload) string {
	body, err := json.Marshal(p)
	if err != nil {
		// All fields are scalars, so this cannot fail either.
		panic("task-rabbit-mcp: confirm payload marshal failed: " + err.Error())
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// VerifyErr explains why a token was refused, or returns nil if it is good.
//
// Every failure names the fix, because a confirmation that says "invalid token"
// and stops leaves the caller with nowhere to go — and the natural wrong guess is
// to ask for a new token, which re-previews and loses the answer they were about
// to confirm.
func (s Signer) VerifyErr(token, op, target string, preview Preview) error {
	body, mac, ok := splitToken(token)
	if !ok {
		return fmt.Errorf(
			"the confirmation token is malformed. Call this tool again without `confirm` to get a fresh one")
	}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return fmt.Errorf(
			"the confirmation token is not readable. Call this tool again without `confirm` to get a fresh one")
	}
	// Constant-time: a byte-by-byte compare would leak the signature to an attacker
	// willing to time the call, and there is no reason to offer it.
	if !hmac.Equal(mac, s.signature(body)) {
		return fmt.Errorf(
			"the confirmation token is not valid for this instance and key. It was not issued here, or " +
				"the API key has changed since. Call the tool again without `confirm` to preview afresh")
	}
	if time.Now().Unix() > p.Expires {
		return fmt.Errorf(
			"the confirmation token expired (%d seconds is the limit). Nothing was changed. Call the tool "+
				"again without `confirm` to preview afresh", int(TTL.Seconds()))
	}
	if p.Instance != s.instance {
		return fmt.Errorf("the confirmation token was issued for %s, not %s. Nothing was changed",
			p.Instance, s.instance)
	}
	if p.Principal != s.principal {
		return fmt.Errorf("the confirmation token was issued to a different API key. Nothing was changed")
	}
	if p.Op != op || p.Target != target {
		return fmt.Errorf(
			"the confirmation token was issued for %s on %s, not %s on %s. Nothing was changed. Call this "+
				"tool again without `confirm` to preview the operation you actually want",
			p.Op, p.Target, op, target)
	}
	if got := Digest(preview); got != p.Digest {
		return fmt.Errorf(
			"the repository changed between the preview and this call, so the token no longer matches what " +
				"was shown and nothing was created. Read it again and confirm the new preview")
	}
	return nil
}

func (s Signer) signature(body []byte) []byte {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(body)
	return mac.Sum(nil)
}

func splitToken(token string) (body []byte, mac []byte, ok bool) {
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			decodedBody, err := base64.RawURLEncoding.DecodeString(token[:i])
			if err != nil {
				return nil, nil, false
			}
			decodedMac, err := base64.RawURLEncoding.DecodeString(token[i+1:])
			if err != nil {
				return nil, nil, false
			}
			return decodedBody, decodedMac, true
		}
	}
	return nil, nil, false
}
