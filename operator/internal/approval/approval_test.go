package approval

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
)

func rec() recommend.Recommendation {
	return recommend.Recommendation{ID: "r1", Namespace: "shop", Kind: "Deployment", Workload: "api", Container: "app",
		CPU: recommend.ResourceResult{Action: recommend.Decrease, Current: 1, Recommended: 0.5}}
}

func keypair(t *testing.T) (priv []byte, trusted map[string]string, id string) {
	t.Helper()
	priv, pub, id, err := GenerateKey(bytes.NewReader(bytes.Repeat([]byte{7}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	return priv, map[string]string{id: string(pub)}, id
}

// A signature verifies for exactly the record, target, approver and time it was made for, and for nothing else.
func TestSignedApprovalBindsRecordTargetApproverAndTime(t *testing.T) {
	privPEM, trusted, id := keypair(t)
	priv, err := ParsePrivate(privPEM)
	if err != nil {
		t.Fatal(err)
	}
	const at = "2026-11-12T00:00:00Z"
	sig, err := Sign(priv, rec(), "alice", at)
	if err != nil || !strings.HasPrefix(sig, id+":") {
		t.Fatalf("%v %s", err, sig)
	}
	if got, err := Verify(trusted, rec(), "alice", at, sig); err != nil || got != id {
		t.Fatalf("a valid signature must verify: %v", err)
	}
	other := rec()
	other.CPU.Recommended = 0.1 // same ID, different record
	moved := rec()
	moved.Namespace = "prod"
	for name, c := range map[string]struct {
		r       recommend.Recommendation
		who, at string
	}{
		"other record, same ID": {other, "alice", at}, "other target": {moved, "alice", at},
		"forged approver": {rec(), "mallory", at}, "other time": {rec(), "alice", "2026-11-13T00:00:00Z"},
	} {
		if _, err := Verify(trusted, c.r, c.who, c.at, sig); err == nil {
			t.Errorf("%s verified", name)
		}
	}
	if _, err := Verify(map[string]string{}, rec(), "alice", at, sig); err == nil || !strings.Contains(err.Error(), "not in approverKeys") {
		t.Fatalf("an unknown key must be refused: %v", err)
	}
	if _, err := Verify(trusted, rec(), "alice", at, ""); err == nil {
		t.Fatal("an unsigned approval must be refused")
	}
	// a trusted entry whose key does not match its ID is refused (no key confusion)
	_, pub2, _, _ := GenerateKey(bytes.NewReader(bytes.Repeat([]byte{9}, 64)))
	if _, err := Verify(map[string]string{id: string(pub2)}, rec(), "alice", at, sig); err == nil {
		t.Fatal("a mismatched key ID must be refused")
	}
}

func TestPublicKeysParseFromPEMOrRawBase64(t *testing.T) {
	_, pubPEM, id, _ := GenerateKey(bytes.NewReader(bytes.Repeat([]byte{3}, 64)))
	pub, err := ParsePublic(string(pubPEM))
	if err != nil || KeyID(pub) != id {
		t.Fatal(err)
	}
	raw, err := ParsePublic(base64.StdEncoding.EncodeToString(pub))
	if err != nil || KeyID(raw) != id {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "not a key", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := ParsePublic(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := ParsePrivate(pubPEM); err == nil {
		t.Fatal("a public key is not a private key")
	}
}

// The signed fields are newline-joined: a field holding a line break could make one signature verify for another split of the
// approver and the time, so signing and verifying refuse it.
func TestFieldsWithLineBreaksAreRefused(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	if _, err := Sign(priv, rec(), "alice\nadmin", "2026-11-12T10:00:00Z"); err == nil {
		t.Fatal("an approver with a newline must be refused")
	}
	if _, err := Sign(priv, rec(), "alice", "2026-11-12\r"); err == nil {
		t.Fatal("a time with a carriage return must be refused")
	}
}
