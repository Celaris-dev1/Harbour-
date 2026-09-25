// Package license implements Harbour's offline license keys for the paid Enterprise
// add-ons. Everything here works without any network access: a license is a small signed
// token the customer sets via HARBOUR_LICENSE or HARBOUR_LICENSE_FILE, verified against
// vendor public keys embedded in the binary. There is no phone-home, no activation server
// and no telemetry. Harbour's core (the durable step engine, its HTTP API, and single-node worker execution) never requires a license at all
// -- see PRICING.md.
package license

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Edition names. Harbour has a single paid edition today: Enterprise.
const (
	EditionEnterprise = "enterprise"
)

// Grace is how long a license keeps working, with a loud warning, after it expires.
const Grace = 14 * 24 * time.Hour

// Known gated features. Keep this list small and deliberate: everything not listed here
// stays free forever. See README's LICENSING section.
const (
	// FeatureMultiWorker gates running harbourd with more worker goroutines than the
	// free single-node cap (freeWorkerCap in cmd/harbourd), i.e. scale-out/HA capacity
	// beyond the default. Enterprise.
	FeatureMultiWorker = "multi-worker"
)

// License is the signed payload. Field order is fixed and part of the signed encoding
// (see canonicalJSON), so do not reorder fields without a format-version bump.
type License struct {
	LicenseID string    `json:"license_id"`
	Customer  string    `json:"customer"`
	Product   string    `json:"product"` // always "harbour"
	Edition   string    `json:"edition"` // "enterprise"
	Features  []string  `json:"features,omitempty"`
	Seats     int       `json:"seats"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// HasFeature reports whether the license covers feature, either because its edition
// includes it by default or because it is explicitly listed (for custom/granular grants).
func (l *License) HasFeature(feature string) bool {
	if l == nil {
		return false
	}
	for _, f := range l.Features {
		if f == feature {
			return true
		}
	}
	switch l.Edition {
	case EditionEnterprise:
		return feature == FeatureMultiWorker
	}
	return false
}

// State is the coarse license status.
type State string

const (
	// StateNone: no license configured, or it was invalid/tampered/unparseable. Core
	// features work fully; gated features are off. Never a hard failure.
	StateNone State = "none"
	// StateValid: a signed, unexpired license for product "harbour". Gated features work.
	StateValid State = "valid"
	// StateGrace: past expiry but within Grace. Gated features keep working with a loud
	// warning so a lapsed renewal doesn't suddenly break a deployment.
	StateGrace State = "grace"
	// StateExpired: past the grace period. Gated features are off. Nothing is ever deleted
	// or hidden: the license simply stops unlocking Enterprise features.
	StateExpired State = "expired"
)

// Status is the resolved outcome of loading a license: state, the parsed license (nil for
// StateNone), and a human-readable warning to surface (empty when there is nothing to say).
type Status struct {
	State   State
	License *License // nil unless State is Valid, Grace or Expired
	Warning string
}

// Active reports whether gated features should currently be unlocked (Valid or Grace).
func (s *Status) Active() bool {
	return s != nil && (s.State == StateValid || s.State == StateGrace)
}

// HasFeature reports whether the current status unlocks feature.
func (s *Status) HasFeature(feature string) bool {
	if s == nil || !s.Active() {
		return false
	}
	return s.License.HasFeature(feature)
}

var (
	mu      sync.RWMutex
	current *Status
	loaded  bool
)

// Current returns the process-wide license status, loading it from HARBOUR_LICENSE /
// HARBOUR_LICENSE_FILE on first use and caching it thereafter. Call Reload to re-read it
// (tests do this after changing the environment).
func Current() *Status {
	mu.Lock()
	defer mu.Unlock()
	if !loaded {
		current = load()
		loaded = true
	}
	return current
}

// Reload re-reads HARBOUR_LICENSE / HARBOUR_LICENSE_FILE and replaces the cached status. It
// returns the new status.
func Reload() *Status {
	mu.Lock()
	defer mu.Unlock()
	current = load()
	loaded = true
	return current
}

func load() *Status {
	tok := strings.TrimSpace(os.Getenv("HARBOUR_LICENSE"))
	if tok == "" {
		if fp := strings.TrimSpace(os.Getenv("HARBOUR_LICENSE_FILE")); fp != "" {
			b, err := os.ReadFile(fp)
			if err != nil {
				return &Status{State: StateNone, Warning: fmt.Sprintf("license: could not read HARBOUR_LICENSE_FILE (%v); running as community", err)}
			}
			tok = strings.TrimSpace(string(b))
		}
	}
	if tok == "" {
		return &Status{State: StateNone}
	}
	lic, err := Verify(tok)
	if err != nil {
		return &Status{State: StateNone, Warning: fmt.Sprintf("license: %v; running as community", err)}
	}
	return resolve(lic, time.Now().UTC())
}

func resolve(lic *License, now time.Time) *Status {
	switch {
	case !lic.ExpiresAt.IsZero() && now.After(lic.ExpiresAt.Add(Grace)):
		return &Status{State: StateExpired, License: lic, Warning: fmt.Sprintf(
			"license: %s edition license for %s expired on %s (past the %d-day grace period); Enterprise features are off",
			lic.Edition, lic.Customer, lic.ExpiresAt.Format("2006-01-02"), int(Grace.Hours()/24))}
	case !lic.ExpiresAt.IsZero() && now.After(lic.ExpiresAt):
		return &Status{State: StateGrace, License: lic, Warning: fmt.Sprintf(
			"license: %s edition license for %s expired on %s; features stay on for the %d-day grace period (renew soon)",
			lic.Edition, lic.Customer, lic.ExpiresAt.Format("2006-01-02"), int(Grace.Hours()/24))}
	default:
		return &Status{State: StateValid, License: lic}
	}
}

// Error is returned by Require when a feature is gated and not unlocked.
type Error struct {
	Feature string
	State   State
	Edition string // minimum edition that would unlock Feature, e.g. "enterprise"
	Message string
}

func (e *Error) Error() string { return e.Message }

// requiredEdition names the cheapest edition that unlocks feature, for error messages.
func requiredEdition(feature string) string {
	return EditionEnterprise
}

// Require returns nil if feature is unlocked by the current license, or a *Error
// describing what's needed otherwise. Callers gate a feature with one call:
//
//	if err := license.Require(license.Feature...); err != nil { ... }
//
// This never fails closed on core use of ungated features: only call Require for the
// small set of Enterprise features.
func Require(feature string) error {
	st := Current()
	if st.HasFeature(feature) {
		return nil
	}
	need := requiredEdition(feature)
	var msg string
	switch st.State {
	case StateExpired:
		msg = fmt.Sprintf("%q requires a Harbour %s license (yours expired on %s, past the %d-day grace period). Contact sales (see PRICING.md), then set HARBOUR_LICENSE / HARBOUR_LICENSE_FILE.",
			feature, strings.Title(need), st.License.ExpiresAt.Format("2006-01-02"), int(Grace.Hours()/24))
	case StateValid, StateGrace:
		msg = fmt.Sprintf("%q requires a Harbour %s license; your current license is %s edition. See PRICING.md.", feature, strings.Title(need), st.License.Edition)
	default: // StateNone
		msg = fmt.Sprintf("%q is a Harbour %s feature. Harbour's core stays free; contact sales for Enterprise, then set HARBOUR_LICENSE or HARBOUR_LICENSE_FILE. See PRICING.md.", feature, strings.Title(need))
	}
	return &Error{Feature: feature, State: st.State, Edition: need, Message: msg}
}

// SeatWarning checks active against the license's seat count and returns a non-empty
// warning if active exceeds seats. It is advisory only: Harbour never locks anyone out
// for being over seats, it just says so.
func SeatWarning(st *Status, active int) string {
	if st == nil || st.License == nil || st.License.Seats <= 0 {
		return ""
	}
	if active > st.License.Seats {
		return fmt.Sprintf("license: %d active users exceeds the %d seats licensed to %s; please true up (this is a reminder, not a lockout)",
			active, st.License.Seats, st.License.Customer)
	}
	return ""
}

// Encode/Decode helpers shared with tools/licensegen and tests.

func b64(b []byte) string            { return base64.RawURLEncoding.EncodeToString(b) }
func unb64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

// canonicalJSON is the exact bytes that get signed: json.Marshal of the fixed-order
// License struct (no maps involved, so Go's struct field order is deterministic).
func canonicalJSON(l *License) ([]byte, error) { return json.Marshal(l) }

// Sign builds a license token: base64url(payload) + "." + base64url(signature). priv must
// be an Ed25519 private key (see tools/licensegen).
func Sign(priv ed25519.PrivateKey, l *License) (string, error) {
	if l.Product == "" {
		l.Product = "harbour"
	}
	payload, err := canonicalJSON(l)
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(priv, payload)
	return b64(payload) + "." + b64(sig), nil
}

// Verify parses and checks a token's signature against the embedded vendor public keys
// (VendorPublicKeys), returning the parsed License on success. It does not check
// expiry -- callers that want state (valid/grace/expired) use Current()/Reload(), which
// call resolve() on top of this.
func Verify(token string) (*License, error) {
	parts := strings.SplitN(strings.TrimSpace(token), ".", 2)
	if len(parts) != 2 {
		return nil, errors.New("malformed license token")
	}
	payload, err := unb64(parts[0])
	if err != nil {
		return nil, fmt.Errorf("malformed license token: %w", err)
	}
	sig, err := unb64(parts[1])
	if err != nil {
		return nil, fmt.Errorf("malformed license token: %w", err)
	}
	ok := false
	for _, pub := range VendorPublicKeys() {
		if len(pub) == ed25519.PublicKeySize && ed25519.Verify(pub, payload, sig) {
			ok = true
			break
		}
	}
	if !ok {
		return nil, errors.New("signature verification failed (tampered, or signed by an unknown key)")
	}
	var lic License
	if err := json.Unmarshal(payload, &lic); err != nil {
		return nil, fmt.Errorf("malformed license payload: %w", err)
	}
	if lic.Product != "harbour" {
		return nil, fmt.Errorf("license is for product %q, not harbour", lic.Product)
	}
	if lic.Edition != EditionEnterprise {
		return nil, fmt.Errorf("unknown edition %q", lic.Edition)
	}
	return &lic, nil
}
