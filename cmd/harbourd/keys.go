package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"flag"
	"fmt"

	"github.com/Celaris-dev1/Harbour-/internal/receipt"
)

const keysUsage = `harbourd keys show   — print the receipt signing key harbourd uses for stack-receipt/v1

usage:
  harbourd keys show

env: HARBOUR_RECEIPT_KEY (base64 Ed25519 seed), HARBOUR_RECEIPT_KEY_FILE (default: under the
     user config dir; created on first use)
`

func cmdKeys(args []string) error {
	if len(args) == 0 {
		fmt.Print(keysUsage)
		return nil
	}
	switch args[0] {
	case "show":
		fs := flag.NewFlagSet("keys show", flag.ContinueOnError)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return cmdKeysShow()
	case "help", "-h", "--help":
		fmt.Print(keysUsage)
		return nil
	default:
		fmt.Print(keysUsage)
		return fmt.Errorf("harbourd keys: unknown subcommand %q", args[0])
	}
}

func cmdKeysShow() error {
	s, err := receipt.DefaultSigner()
	if err != nil {
		return fmt.Errorf("receipt signing key: %w", err)
	}
	pub := s.Key.Public().(ed25519.PublicKey)
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	keyID := s.KeyID() // exactly what signed receipts carry as signer_key_id

	fmt.Printf("product:    harbour\n")
	fmt.Printf("key_id:     %s\n", keyID)
	fmt.Printf("alg:        ed25519\n")
	fmt.Printf("public_key: %s\n", pubB64)
	fmt.Println()
	fmt.Printf("ledger keys enroll --product harbour --key-id %s --public-key %s\n", keyID, pubB64)
	return nil
}
