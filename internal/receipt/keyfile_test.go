package receipt

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestLoadOrCreateSignerPersists(t *testing.T) {
	dir := t.TempDir()
	kf := filepath.Join(dir, "sub", "receipt.key")

	s1, err := LoadOrCreateSigner(kf)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(kf)
	if err != nil {
		t.Fatalf("key file not created: %v", err)
	}
	if runtime.GOOS != "windows" {
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Fatalf("key file perms = %o, want 0600", got)
		}
	}

	s2, err := LoadOrCreateSigner(kf)
	if err != nil {
		t.Fatal(err)
	}
	if s1.KeyID() != s2.KeyID() {
		t.Fatalf("key did not persist: %s != %s", s1.KeyID(), s2.KeyID())
	}
	if string(s1.Key) != string(s2.Key) {
		t.Fatal("reloaded key material differs")
	}
}

func TestKeyIDMatchesSignedReceipt(t *testing.T) {
	dir := t.TempDir()
	kf := filepath.Join(dir, "receipt.key")

	s, err := LoadOrCreateSigner(kf)
	if err != nil {
		t.Fatal(err)
	}
	env, err := Sign(context.Background(), s, testEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	if env.SignerKeyID != s.KeyID() {
		t.Fatalf("signer_key_id %q != KeyID() %q", env.SignerKeyID, s.KeyID())
	}
}

func TestHarbourReceiptKeyEnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	kf := filepath.Join(dir, "receipt.key")

	// Seed a key file with a different key than the env var.
	if _, err := LoadOrCreateSigner(kf); err != nil {
		t.Fatal(err)
	}

	envSeed := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" // 32 zero bytes, base64
	t.Setenv("HARBOUR_RECEIPT_KEY", envSeed)

	s, err := LoadOrCreateSigner(kf)
	if err != nil {
		t.Fatal(err)
	}
	want, err := LoadOrCreateSigner("") // no file, only env
	if err != nil {
		t.Fatal(err)
	}
	if s.KeyID() != want.KeyID() {
		t.Fatalf("HARBOUR_RECEIPT_KEY was not honored over the key file")
	}
}

func TestDefaultKeyFileHonorsOverrides(t *testing.T) {
	t.Setenv("HARBOUR_RECEIPT_KEY_FILE", "/tmp/explicit-receipt.key")
	if got := DefaultKeyFile(); got != "/tmp/explicit-receipt.key" {
		t.Fatalf("DefaultKeyFile() = %q, want explicit override", got)
	}
}

func TestLoadOrCreateSignerConcurrentFirstRunAgrees(t *testing.T) {
	t.Setenv("HARBOUR_RECEIPT_KEY", "")
	path := filepath.Join(t.TempDir(), "k", "receipt.key")
	const n = 16
	ids := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := LoadOrCreateSigner(path)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- s.KeyID()
		}()
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		} else if id != first {
			t.Fatalf("concurrent first runs produced different keys: %s vs %s", first, id)
		}
	}
}

func TestLoadOrCreateSignerNeverOverwritesCorruptKey(t *testing.T) {
	t.Setenv("HARBOUR_RECEIPT_KEY", "")
	path := filepath.Join(t.TempDir(), "receipt.key")
	if err := os.WriteFile(path, []byte("not-a-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateSigner(path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "not-a-key\n" {
		t.Fatalf("corrupt key file was overwritten: %q", b)
	}
}
