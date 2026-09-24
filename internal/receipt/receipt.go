// Package receipt implements the "stack-receipt/v1" envelope defined in Ledger's
// docs/receipt-spec.md: a signed, product-agnostic record this product (or any other in the
// stack) can emit to attest to a decision, effect or verification, that Ledger stores, verifies
// and links across products for the incident report.
//
// This is a self-contained copy of Ledger's internal/receipt (canonical JSON + signing +
// verification), so Warrant has no build dependency on the Ledger module. Any change here
// must stay byte-for-byte compatible with Ledger's copy: canonical JSON is sorted-key,
// no-HTML-escape JSON with no insignificant whitespace, and payload_hash/signing bytes are
// computed the same way. See testdata/receipts/ (copied from Ledger) for conformance vectors.
package receipt

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Version is the only envelope version this package emits and accepts.
const Version = "stack-receipt/v1"

// Actor mirrors the actor_chain element used across the stack.
type Actor struct {
	Kind         string `json:"kind"`
	ID           string `json:"id"`
	Model        string `json:"model,omitempty"`
	ModelVersion string `json:"model_version,omitempty"`
}

// Link points at another product's receipt or record.
type Link struct {
	Product string `json:"product"`
	ID      string `json:"id"`
	Hash    string `json:"hash"`
}

// Envelope is the stack-receipt/v1 wire format.
type Envelope struct {
	Version     string    `json:"version"`
	Product     string    `json:"product"`
	Kind        string    `json:"kind"`
	GoalID      string    `json:"goal_id,omitempty"`
	Actors      []Actor   `json:"actor_chain"`
	Subject     string    `json:"subject,omitempty"`
	PayloadHash string    `json:"payload_hash"`
	Links       []Link    `json:"links,omitempty"`
	IssuedAt    time.Time `json:"issued_at"`
	SignerKeyID string    `json:"signer_key_id"`
	Signature   string    `json:"signature"`
}

var validProduct = map[string]bool{
	"gate": true, "proof": true, "ledger": true, "warrant": true, "harbour": true, "bench": true,
}

// --- canonical JSON (RFC 8785-style: sorted keys, no HTML escaping, no insignificant ws) ---

func normalize(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("receipt: trailing data after JSON value")
	}
	return v, nil
}

func canonicalMarshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func canonicalBytes(raw []byte) ([]byte, error) {
	v, err := normalize(raw)
	if err != nil {
		return nil, err
	}
	return canonicalMarshal(v)
}

// canonHash mirrors Ledger's internal/canon.Hash("", body): sha256hex("\n" + canonical(body)).
func canonHash(body any) (string, error) {
	b, err := canonicalMarshal(body)
	if err != nil {
		return "", err
	}
	c, err := canonicalBytes(b)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte("\n"))
	h.Write(c)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// PayloadHash returns hex sha256 of the canonical JSON of payload, for use as PayloadHash.
func PayloadHash(payload any) (string, error) { return canonHash(payload) }

func signingBytes(env Envelope) ([]byte, error) {
	env.Signature = ""
	b, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	return canonicalBytes(b)
}

// --- signing / verification (Ed25519; matches Ledger's keys.AlgEd25519 + KeyIDFor) ---

// Signer is the minimal interface receipt signing needs (Ledger's keys.Signer implements it).
type Signer interface {
	KeyID() string
	Sign(ctx context.Context, msg []byte) ([]byte, error)
}

// Ed25519Signer is a local Ed25519 signer.
type Ed25519Signer struct{ Key ed25519.PrivateKey }

// KeyIDFor derives "ed25519:"+hex(sha256(pub)[:8]), matching Ledger's keys.KeyIDFor.
func KeyIDFor(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return "ed25519:" + hex.EncodeToString(h[:8])
}

func (s Ed25519Signer) KeyID() string { return KeyIDFor(s.Key.Public().(ed25519.PublicKey)) }
func (s Ed25519Signer) Sign(_ context.Context, msg []byte) ([]byte, error) {
	return ed25519.Sign(s.Key, msg), nil
}

// Sign fills SignerKeyID and Signature on env using s. IssuedAt is set to now if zero.
func Sign(ctx context.Context, s Signer, env Envelope) (Envelope, error) {
	if env.Version == "" {
		env.Version = Version
	}
	if env.IssuedAt.IsZero() {
		env.IssuedAt = time.Now().UTC()
	}
	if err := validateContent(env); err != nil {
		return Envelope{}, err
	}
	env.SignerKeyID = s.KeyID()
	msg, err := signingBytes(env)
	if err != nil {
		return Envelope{}, err
	}
	sig, err := s.Sign(ctx, msg)
	if err != nil {
		return Envelope{}, err
	}
	env.Signature = base64.StdEncoding.EncodeToString(sig)
	return env, nil
}

// Verify checks structure and the Ed25519 signature against pub.
func Verify(env Envelope, pub ed25519.PublicKey) error {
	if err := validateStructure(env); err != nil {
		return err
	}
	if got := KeyIDFor(pub); got != env.SignerKeyID {
		return fmt.Errorf("receipt: signer_key_id %q does not match supplied key (%q)", env.SignerKeyID, got)
	}
	sig, err := base64.StdEncoding.DecodeString(env.Signature)
	if err != nil {
		return fmt.Errorf("receipt: malformed signature: %w", err)
	}
	msg, err := signingBytes(env)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, msg, sig) {
		return errors.New("receipt: ed25519 signature invalid")
	}
	return nil
}

func validateContent(env Envelope) error {
	if env.Version != Version {
		return fmt.Errorf("receipt: unsupported version %q, want %q", env.Version, Version)
	}
	if !validProduct[env.Product] {
		return fmt.Errorf("receipt: unknown product %q", env.Product)
	}
	if env.Kind == "" {
		return errors.New("receipt: kind is required")
	}
	if len(env.Actors) == 0 || env.Actors[0].Kind != "human" {
		return errors.New("receipt: actor_chain must be non-empty and start with a human")
	}
	if len(env.PayloadHash) != 64 || !isHex(env.PayloadHash) {
		return errors.New("receipt: payload_hash must be 64 hex chars (sha256)")
	}
	for i, l := range env.Links {
		if !validProduct[l.Product] {
			return fmt.Errorf("receipt: links[%d].product invalid: %q", i, l.Product)
		}
		if l.ID == "" {
			return fmt.Errorf("receipt: links[%d].id is required", i)
		}
	}
	if env.IssuedAt.IsZero() {
		return errors.New("receipt: issued_at is required")
	}
	return nil
}

func validateStructure(env Envelope) error {
	if err := validateContent(env); err != nil {
		return err
	}
	if env.SignerKeyID == "" {
		return errors.New("receipt: signer_key_id is required")
	}
	if env.Signature == "" {
		return errors.New("receipt: signature is required")
	}
	return nil
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
