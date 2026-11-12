// Package approval signs and verifies human approvals of a recommendation with Ed25519.
//
// Without it, an approval is an annotation (or status field) holding the recommendation ID and the approver's username, stamped
// by `rightsize approve` from a SelfSubjectReview. Anyone allowed to update the object can forge that stamp; only the Kubernetes
// audit log shows who really wrote it. A signed approval binds, with a private key only the approver holds, exactly:
//
//	the recommendation ID, the target (namespace/kind/name), the SHA-256 of the full recommendation record, the approver's
//	username and the approval time.
//
// The policy lists the public keys it trusts (approverKeys). A signature that does not verify, a key that is not listed, or an
// approval of a different record (even with the same ID) is refused. Private keys never enter the cluster.
package approval

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
)

// Version prefixes every signed payload so a signature can never be replayed as another kind of message.
const Version = "kubecost-rightsize-approval/v1"

// Digest is the SHA-256 (hex) of the recommendation record as encoding/json writes it. Go's encoder is deterministic for a struct,
// so the CLI that signs and the operator that verifies compute the same digest from the same record.
func Digest(rec recommend.Recommendation) (string, error) {
	b, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// Payload is the exact byte string that is signed.
func Payload(rec recommend.Recommendation, approver, at string) ([]byte, error) {
	d, err := Digest(rec)
	if err != nil {
		return nil, err
	}
	return []byte(strings.Join([]string{Version, rec.ID, rec.Namespace + "/" + rec.Kind + "/" + rec.Workload, d, approver, at}, "\n")), nil
}

// KeyID names a public key: the first 8 bytes of its SHA-256, hex.
func KeyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:8])
}

// Sign returns "<keyID>:<base64 signature>".
func Sign(key ed25519.PrivateKey, rec recommend.Recommendation, approver, at string) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", errors.New("not an Ed25519 private key")
	}
	p, err := Payload(rec, approver, at)
	if err != nil {
		return "", err
	}
	pub := key.Public().(ed25519.PublicKey)
	return KeyID(pub) + ":" + base64.StdEncoding.EncodeToString(ed25519.Sign(key, p)), nil
}

// Verify checks sig against the trusted keys (key ID -> public key, PEM or base64 of the 32 raw bytes) and returns the key ID used.
func Verify(trusted map[string]string, rec recommend.Recommendation, approver, at, sig string) (string, error) {
	id, b64, ok := strings.Cut(strings.TrimSpace(sig), ":")
	if !ok || id == "" || b64 == "" {
		return "", errors.New("the approval carries no signature (or not in the keyID:signature form)")
	}
	pubText, ok := trusted[id]
	if !ok {
		return id, fmt.Errorf("the approval is signed with key %s, which is not in approverKeys", id)
	}
	pub, err := ParsePublic(pubText)
	if err != nil {
		return id, fmt.Errorf("approverKeys[%s]: %w", id, err)
	}
	if KeyID(pub) != id {
		return id, fmt.Errorf("approverKeys[%s] holds a key whose ID is %s", id, KeyID(pub))
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return id, errors.New("the signature is not base64")
	}
	p, err := Payload(rec, approver, at)
	if err != nil {
		return id, err
	}
	if !ed25519.Verify(pub, p, raw) {
		return id, errors.New("the signature does not match this recommendation, target, approver and time")
	}
	return id, nil
}

// GenerateKey makes a key pair and returns the private key (PKCS#8 PEM), the public key (PKIX PEM) and its ID.
func GenerateKey(r io.Reader) (privPEM, pubPEM []byte, id string, err error) {
	if r == nil {
		r = rand.Reader
	}
	pub, priv, err := ed25519.GenerateKey(r)
	if err != nil {
		return nil, nil, "", err
	}
	pk, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, "", err
	}
	pb, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, nil, "", err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk}), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pb}), KeyID(pub), nil
}

// ParsePrivate reads a PKCS#8 PEM Ed25519 private key.
func ParsePrivate(b []byte) (ed25519.PrivateKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("not a PEM file")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("not an Ed25519 private key")
	}
	return priv, nil
}

// ParsePublic reads a PKIX PEM public key, or base64 of the 32 raw bytes (convenient inside a ConfigMap).
func ParsePublic(s string) (ed25519.PublicKey, error) {
	s = strings.TrimSpace(s)
	if blk, _ := pem.Decode([]byte(s)); blk != nil {
		k, err := x509.ParsePKIXPublicKey(blk.Bytes)
		if err != nil {
			return nil, err
		}
		pub, ok := k.(ed25519.PublicKey)
		if !ok {
			return nil, errors.New("not an Ed25519 public key")
		}
		return pub, nil
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("not a PEM public key nor base64 of 32 bytes")
	}
	return ed25519.PublicKey(raw), nil
}
