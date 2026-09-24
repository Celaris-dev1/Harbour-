package receipt

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// DefaultKeyFile returns the path harbour persists its receipt signing key to
// when HARBOUR_RECEIPT_KEY (an inline seed) is not set: HARBOUR_RECEIPT_KEY_FILE if
// set, else <user config dir>/harbour/receipt.key. Returns "" if neither is
// available (e.g. no home dir), in which case the signer falls back to an
// ephemeral key.
func DefaultKeyFile() string {
	if v := os.Getenv("HARBOUR_RECEIPT_KEY_FILE"); v != "" {
		return v
	}
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "harbour", "receipt.key")
	}
	return ""
}

// LoadOrCreateSigner returns the Ed25519 signer harbour uses for stack-receipt/v1
// envelopes, in this order:
//
//  1. HARBOUR_RECEIPT_KEY, a base64-encoded 32-byte Ed25519 seed, if set and valid.
//  2. The key stored at keyFile (DefaultKeyFile() if keyFile is ""), if it exists.
//  3. A freshly generated key, persisted to keyFile with mode 0600 (and its
//     parent directory created 0700) so later runs reuse the same
//     signer_key_id. The directory is created if missing.
//  4. If keyFile is "" or cannot be written, an ephemeral key for this
//     process only; a warning is logged since signer_key_id will then change
//     on every restart and receipts cannot be re-verified against a
//     previously enrolled key.
func LoadOrCreateSigner(keyFile string) (Ed25519Signer, error) {
	if seed := os.Getenv("HARBOUR_RECEIPT_KEY"); seed != "" {
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(seed))
		if err == nil && len(b) == ed25519.SeedSize {
			return Ed25519Signer{Key: ed25519.NewKeyFromSeed(b)}, nil
		}
		log.Print("harbour: HARBOUR_RECEIPT_KEY is set but invalid (want base64 32-byte seed), ignoring")
	}
	if keyFile == "" {
		keyFile = DefaultKeyFile()
	}
	if keyFile != "" {
		seed, err := loadOrCreateSeed(keyFile)
		if err == nil {
			return Ed25519Signer{Key: ed25519.NewKeyFromSeed(seed)}, nil
		}
		log.Printf("harbour: could not load or persist receipt signing key at %s, using an ephemeral key for this process only (signer_key_id will change on restart): %v", keyFile, err)
	} else {
		log.Print("harbour: no writable location for a persistent receipt signing key, using an ephemeral key for this process only (signer_key_id will change on restart)")
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Ed25519Signer{}, err
	}
	return Ed25519Signer{Key: priv}, nil
}

func readSeedFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, err
	}
	if len(seed) != ed25519.SeedSize {
		return nil, errors.New("receipt: key file does not hold a 32-byte seed")
	}
	return seed, nil
}

// loadOrCreateSeed returns the seed stored at path, creating it if absent. An
// existing file that cannot be parsed is an error, never overwritten: it may be
// the only copy of a key whose receipts are already enrolled in Ledger.
func loadOrCreateSeed(path string) ([]byte, error) {
	seed, err := readSeedFile(path)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return seed, err
	}
	seed = make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	if err := writeSeedFile(path, seed); err != nil {
		if errors.Is(err, os.ErrExist) {
			// Another process created it first; use theirs so both sign with one key.
			return readSeedFile(path)
		}
		return nil, err
	}
	return seed, nil
}

// writeSeedFile publishes the seed atomically and only if path does not exist
// yet (hard link of a fully written temp file), so concurrent first runs agree.
func writeSeedFile(path string, seed []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".receipt-key-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.WriteString(base64.StdEncoding.EncodeToString(seed) + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Link(tmp, path)
}

// process-wide cached signer, loaded once per process: a stable signer_key_id
// for the life of the process, and only one key-file read/create attempt.
var (
	defaultSignerOnce sync.Once
	defaultSigner     Ed25519Signer
	defaultSignerErr  error
)

// DefaultSigner returns the process-wide receipt signer, loading or creating
// it on first use (see LoadOrCreateSigner).
func DefaultSigner() (Ed25519Signer, error) {
	defaultSignerOnce.Do(func() {
		defaultSigner, defaultSignerErr = LoadOrCreateSigner("")
	})
	return defaultSigner, defaultSignerErr
}
