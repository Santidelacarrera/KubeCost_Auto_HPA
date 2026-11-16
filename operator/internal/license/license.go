// Package license verifies a signed licence file offline and turns it into the operating mode the operator may run in.
//
// Design rules (docs/COMMERCIAL.md):
//   - a licence is a file, verified with a public key the operator is given; nothing is ever sent anywhere;
//   - the tier gates the MODE and nothing else: observe-only (starter), assisted (professional), automated (enterprise);
//   - it fails safe: a missing, invalid or expired licence means the starter tier, which is observe-only. A licence can only
//     ever remove a restriction that enforcement added; it can never weaken a safety control;
//   - enforcement is opt-in (--enforce-license). Without it the operator behaves exactly as before and the licence is reported only.
package license

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/approval"
)

// Tier is a commercial edition.
type Tier string

const (
	Starter      Tier = "starter"
	Professional Tier = "professional"
	Enterprise   Tier = "enterprise"
)

func (t Tier) rank() int {
	switch t {
	case Professional:
		return 1
	case Enterprise:
		return 2
	}
	return 0
}

// License is what the vendor signs.
type License struct {
	ID       string `json:"id"`
	Customer string `json:"customer"`
	Tier     Tier   `json:"tier"`
	// MaxNodes is the licensed node count; 0 means not limited.
	MaxNodes  int       `json:"maxNodes,omitempty"`
	NotBefore time.Time `json:"notBefore"`
	Expires   time.Time `json:"expires"`
}

// File is the on-disk form: the exact signed bytes (base64 of the licence JSON), so verification never depends on how JSON is
// re-serialised, plus the signature and the ID of the key that made it.
type File struct {
	Payload   string `json:"payload"`
	KeyID     string `json:"keyID"`
	Signature string `json:"signature"`
}

const maxFile = 16 << 10

// Issue signs a licence. It is the vendor-side half and needs the private key.
func Issue(key ed25519.PrivateKey, l License) ([]byte, error) {
	switch {
	case l.ID == "" || l.Customer == "":
		return nil, errors.New("a licence needs an id and a customer")
	case l.Tier != Starter && l.Tier != Professional && l.Tier != Enterprise:
		return nil, fmt.Errorf("unknown tier %q", l.Tier)
	case l.MaxNodes < 0:
		return nil, errors.New("maxNodes must not be negative")
	case !l.Expires.After(l.NotBefore):
		return nil, errors.New("the licence must expire after it starts")
	}
	raw, err := json.Marshal(l)
	if err != nil {
		return nil, err
	}
	pub, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("not an Ed25519 key")
	}
	return json.MarshalIndent(File{Payload: base64.StdEncoding.EncodeToString(raw), KeyID: approval.KeyID(pub),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, raw))}, "", "  ")
}

// Verify checks the signature against the trusted public keys (PEM or base64, as approval.ParsePublic reads them) and the
// validity period. It returns the licence only when everything holds.
func Verify(data []byte, trusted []string, now time.Time) (License, error) {
	var l License
	if len(data) > maxFile {
		return l, errors.New("licence file is too large")
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return l, fmt.Errorf("licence file is not valid JSON: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(f.Payload)
	if err != nil {
		return l, errors.New("licence payload is not base64")
	}
	sig, err := base64.StdEncoding.DecodeString(f.Signature)
	if err != nil {
		return l, errors.New("licence signature is not base64")
	}
	if len(trusted) == 0 {
		return l, errors.New("no trusted licence key is configured")
	}
	ok := false
	for _, t := range trusted {
		pub, err := approval.ParsePublic(t)
		if err != nil {
			return l, fmt.Errorf("trusted licence key: %v", err)
		}
		if ed25519.Verify(pub, raw, sig) {
			ok = true
			break
		}
	}
	if !ok {
		return l, errors.New("licence signature does not verify with any trusted key")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return License{}, fmt.Errorf("licence payload: %v", err)
	}
	switch {
	case l.Tier != Starter && l.Tier != Professional && l.Tier != Enterprise:
		return License{}, fmt.Errorf("licence has an unknown tier %q", l.Tier)
	case now.Before(l.NotBefore):
		return License{}, fmt.Errorf("licence is not valid before %s", l.NotBefore.Format(time.RFC3339))
	case !now.Before(l.Expires):
		return License{}, fmt.Errorf("licence expired on %s", l.Expires.Format(time.RFC3339))
	}
	return l, nil
}

// Requested is what the operator was configured to do.
type Requested struct {
	ObserveOnly bool
	Applier     bool
}

// Effective is what it may do.
type Effective struct {
	Tier        Tier
	ObserveOnly bool
	Applier     bool
	// ForceApproval: every change needs a human approval whatever the policy says (the automated mode is not licensed).
	ForceApproval bool
	// Reasons says, for the log and the status, what enforcement changed and why. Empty when nothing changed.
	Reasons []string
}

// Enforce maps a licence (or the lack of one: pass verifyErr) onto the requested configuration. It only ever makes the
// configuration MORE conservative: it can turn observe-only on, the applier off and approvals on, never the reverse.
// nodes is the current node count (0 = unknown, not checked).
func Enforce(req Requested, l License, verifyErr error, nodes int) Effective {
	e := Effective{Tier: Starter, ObserveOnly: req.ObserveOnly, Applier: req.Applier}
	switch {
	case verifyErr != nil:
		e.Reasons = append(e.Reasons, "no valid licence ("+verifyErr.Error()+"): running as the starter tier")
	case l.MaxNodes > 0 && nodes > l.MaxNodes:
		e.Reasons = append(e.Reasons, fmt.Sprintf("the cluster has %d nodes and the licence covers %d: running as the starter tier", nodes, l.MaxNodes))
	default:
		e.Tier = l.Tier
	}
	if e.Tier.rank() < Professional.rank() {
		if !req.ObserveOnly {
			e.ObserveOnly = true
			e.Reasons = append(e.Reasons, "the starter tier is observe-only: observe-only was turned on")
		}
		if req.Applier {
			e.Applier = false
			e.Reasons = append(e.Reasons, "the starter tier does not include applying changes: the recommendation applier was not started")
		}
	}
	if e.Tier.rank() < Enterprise.rank() {
		e.ForceApproval = true // harmless below professional (nothing is applied); the gate for professional
	}
	return e
}
