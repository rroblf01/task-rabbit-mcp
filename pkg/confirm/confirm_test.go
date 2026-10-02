package confirm

import (
	"strings"
	"testing"
	"time"
)

func testSigner() Signer {
	return NewSigner([]byte("a-secret-that-is-long-enough"), "principal-a", "https://jirrabit.example.com")
}

func testPreview(branch, base string) Preview {
	return Preview{
		Operation: "createBranch",
		Target:    "/home/you/code/web",
		Summary:   "create " + branch + " from " + base,
		Detail: map[string]any{
			"branch": branch,
			"base":   base,
			"clean":  true,
		},
		Reversible: true,
		Undo:       "delete the branch again",
	}
}

func TestAFreshTokenVerifies(t *testing.T) {
	signer := testSigner()
	preview := testPreview("demo-42-fix-login", "origin/main")
	token := signer.Issue("createBranch", "/home/you/code/web", preview)

	if err := signer.VerifyErr(token, "createBranch", "/home/you/code/web", preview); err != nil {
		t.Fatalf("a token this server just issued did not verify: %v", err)
	}
}

// The digest is what makes this a confirmation rather than a delay: the repository
// changed after the user said yes, so the answer is no.
func TestTheTokenIsSignedOverThePreview(t *testing.T) {
	signer := testSigner()
	preview := testPreview("demo-42-fix-login", "origin/main")
	token := signer.Issue("createBranch", "/home/you/code/web", preview)

	// The working tree got dirty in between.
	changed := preview
	changed.Detail["clean"] = false

	err := signer.VerifyErr(token, "createBranch", "/home/you/code/web", changed)
	if err == nil {
		t.Fatal("a changed preview was accepted")
	}
	if !strings.Contains(err.Error(), "changed between the preview") {
		t.Errorf("the refusal should say what happened: %v", err)
	}
}

func TestTheTokenIsPinnedToOneOperationAndTarget(t *testing.T) {
	signer := testSigner()
	preview := testPreview("demo-42", "origin/main")
	token := signer.Issue("createBranch", "/home/you/code/web", preview)

	// A token issued for a branch must not authorise a different operation.
	if err := signer.VerifyErr(token, "deleteBranch", "/home/you/code/web", preview); err == nil {
		t.Error("a branch token authorised a different operation")
	}
	// Nor a different repository.
	if err := signer.VerifyErr(token, "createBranch", "/home/you/code/ops", preview); err == nil {
		t.Error("a token authorised a different repository")
	}
}

// A shared server serves many jirrabits, so a token that travelled would be a
// confirmed branch against somebody else's checkout.
func TestATokenDoesNotTravel(t *testing.T) {
	issuer := testSigner()
	preview := testPreview("demo-42", "origin/main")
	token := issuer.Issue("createBranch", "/repo", preview)

	otherInstance := NewSigner([]byte("a-secret-that-is-long-enough"), "principal-a", "https://other.example")
	if err := otherInstance.VerifyErr(token, "createBranch", "/repo", preview); err == nil {
		t.Error("a token verified against a different instance")
	}

	otherKey := NewSigner([]byte("a-secret-that-is-long-enough"), "principal-b", "https://jirrabit.example.com")
	if err := otherKey.VerifyErr(token, "createBranch", "/repo", preview); err == nil {
		t.Error("a token verified for a different API key")
	}

	rotated := NewSigner([]byte("a-different-secret-entirely-here"), "principal-a", "https://jirrabit.example.com")
	if err := rotated.VerifyErr(token, "createBranch", "/repo", preview); err == nil {
		t.Error("a token verified after the key was rotated")
	}
}

func TestAnExpiredTokenIsRefused(t *testing.T) {
	signer := testSigner()
	preview := testPreview("demo-42", "origin/main")
	// Minted in the past, so it is already outside the window.
	stale := payload{
		Op: "createBranch", Target: "/repo", Instance: signer.instance,
		Principal: signer.principal, Digest: Digest(preview),
		Issued:  time.Now().Add(-10 * time.Minute).Unix(),
		Expires: time.Now().Add(-8 * time.Minute).Unix(),
	}
	err := signer.VerifyErr(signer.sign(stale), "createBranch", "/repo", preview)
	if err == nil {
		t.Fatal("an expired token was accepted")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("the refusal should say it expired: %v", err)
	}
}

func TestATamperedTokenIsRefused(t *testing.T) {
	signer := testSigner()
	preview := testPreview("demo-42", "origin/main")
	token := signer.Issue("createBranch", "/repo", preview)

	for _, tampered := range []string{
		"", "no-dot", "!!!.???", token + "x", "x." + token, strings.ToUpper(token),
	} {
		if err := signer.VerifyErr(tampered, "createBranch", "/repo", preview); err == nil {
			t.Errorf("a tampered token was accepted: %q", tampered)
		}
	}
}

// Every refusal has to name the way out, or the caller retries the same thing.
func TestEveryRefusalSaysWhatToDo(t *testing.T) {
	signer := testSigner()
	preview := testPreview("demo-42", "origin/main")

	for name, err := range map[string]error{
		"malformed": signer.VerifyErr("nonsense", "createBranch", "/repo", preview),
		"expired":   signer.VerifyErr(signer.sign(payload{Op: "createBranch", Target: "/repo", Instance: signer.instance, Principal: signer.principal, Digest: Digest(preview), Issued: 1, Expires: 2}), "createBranch", "/repo", preview),
		"wrong op":  signer.VerifyErr(signer.Issue("other", "/repo", preview), "createBranch", "/repo", preview),
		"changed": func() error {
			// Built from scratch, not by copying: Preview.Detail is a map, so a
			// shallow copy shares it and mutating one mutates the original. That is a
			// property of Go maps, not of this package, but it is worth a test that
			// does not rely on it.
			changed := testPreview("demo-42", "origin/main")
			changed.Detail["clean"] = false
			return signer.VerifyErr(signer.Issue("createBranch", "/repo", preview), "createBranch", "/repo", changed)
		}(),
	} {
		if err == nil {
			t.Errorf("%s: expected a refusal", name)
			continue
		}
		if !strings.Contains(err.Error(), "confirm") {
			t.Errorf("%s: the refusal should say how to get a fresh token: %v", name, err)
		}
	}
}

func TestDigestIgnoresTheTokenFields(t *testing.T) {
	preview := testPreview("demo-42", "origin/main")
	first := Digest(preview)

	// The token and its expiry are written into the same struct that is hashed, so
	// including them would make the digest depend on itself.
	preview.ConfirmationToken = "something"
	preview.ExpiresAt = 12345
	if got := Digest(preview); got != first {
		t.Errorf("the digest changed when the token fields were set: %s vs %s", got, first)
	}
}

func TestDigestChangesWithThePreview(t *testing.T) {
	base := Digest(testPreview("a", "origin/main"))

	if got := Digest(testPreview("b", "origin/main")); got == base {
		t.Error("a different branch produced the same digest")
	}
	if got := Digest(testPreview("a", "origin/develop")); got == base {
		t.Error("a different base produced the same digest")
	}
}
