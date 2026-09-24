package receipt

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testEnv(t *testing.T) Envelope {
	t.Helper()
	ph, err := PayloadHash(map[string]any{"decision": "allow", "effect_id": "eff-1"})
	if err != nil {
		t.Fatal(err)
	}
	return Envelope{
		Product: "harbour", Kind: "harbour.effect", Subject: "eff-1",
		Actors:      []Actor{{Kind: "human", ID: "alice"}},
		PayloadHash: ph,
		IssuedAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	env, err := Sign(context.Background(), Ed25519Signer{priv}, testEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(env, pub); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	otherPub, _, _ := ed25519.GenerateKey(nil)
	env, _ := Sign(context.Background(), Ed25519Signer{priv}, testEnv(t))
	if err := Verify(env, otherPub); err == nil {
		t.Fatal("expected failure with wrong key")
	}
}

// TestConformanceVectors validates this package's copy of the codec against Ledger's
// checked-in fixtures (testdata/receipts/, copied verbatim from Ledger's docs/receipt-spec.md
// vectors) so the two implementations stay byte-compatible without a build dependency.
func TestConformanceVectors(t *testing.T) {
	keyRaw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "receipts", "valid", "key.json"))
	if err != nil {
		t.Fatal(err)
	}
	var keyDoc struct {
		PublicKey string `json:"public_key"`
	}
	if err := json.Unmarshal(keyRaw, &keyDoc); err != nil {
		t.Fatal(err)
	}
	pub, err := base64.StdEncoding.DecodeString(keyDoc.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	checkFile := func(path string, wantOK bool) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var env Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			if wantOK {
				t.Fatalf("%s: unmarshal: %v", path, err)
			}
			return
		}
		err = Verify(env, ed25519.PublicKey(pub))
		if wantOK && err != nil {
			t.Errorf("%s: expected valid, got %v", path, err)
		}
		if !wantOK && err == nil {
			t.Errorf("%s: expected invalid, but verified OK", path)
		}
	}
	validDir := filepath.Join("..", "..", "testdata", "receipts", "valid")
	invalidDir := filepath.Join("..", "..", "testdata", "receipts", "invalid")
	checkFile(filepath.Join(validDir, "gate-verdict.json"), true)
	checkFile(filepath.Join(validDir, "warrant-decision.json"), true)
	checkFile(filepath.Join(invalidDir, "bad-signature.json"), false)
	checkFile(filepath.Join(invalidDir, "bad-version.json"), false)
	checkFile(filepath.Join(invalidDir, "missing-human-actor.json"), false)
	checkFile(filepath.Join(invalidDir, "malformed-payload-hash.json"), false)
}
