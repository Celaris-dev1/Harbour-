//go:build licensegen

// Command licensegen issues Harbour Enterprise license keys. It is a separate main
// package guarded by the "licensegen" build tag, so it never ships in the default
// harbourd release binary (`go build ./...` and `go build ./cmd/harbourd` both skip it).
// Build or run it explicitly:
//
//	go run -tags licensegen ./tools/licensegen keygen --out priv.key
//	go run -tags licensegen ./tools/licensegen issue --key priv.key --customer "Acme Inc" \
//	    --edition enterprise --seats 10 --days 365
//
// See the README's "Issuing licenses" section for the full workflow. The private key this
// tool creates is the vendor's real signing key: keep it offline, never commit it, and
// never put it in a repo, CI secret store shared with customers, or a build artifact.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Celaris-dev1/Harbour-/internal/license"
)

const usage = `licensegen — issue Harbour Enterprise license keys (offline; never ships in the harbourd binary)

usage:
  licensegen keygen --out <path>                     generate an Ed25519 signing keypair
                                                       (writes <path> 0600; prints the public key)
  licensegen issue --key <path> --customer <name> --edition enterprise
                    [--seats N] [--days N] [--features f1,f2] [--license-id id]
                                                       print a signed license token on stdout
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = cmdKeygen(os.Args[2:])
	case "issue":
		err = cmdIssue(os.Args[2:])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "licensegen:", err)
		os.Exit(1)
	}
}

func cmdKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	out := fs.String("out", "priv.key", "path to write the private signing key (0600)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w (refusing to overwrite an existing key)", *out, err)
	}
	defer f.Close()
	if _, err := f.WriteString(base64.StdEncoding.EncodeToString(priv) + "\n"); err != nil {
		return err
	}
	fmt.Printf("wrote private key to %s (mode 0600) -- keep it offline, never commit it\n", *out)
	fmt.Printf("public key: %s\n", base64.StdEncoding.EncodeToString(pub))
	fmt.Println("paste that public key into internal/license/keys.go's prodPublicKeyB64 (or add it as a new entry) and cut a release.")
	return nil
}

func cmdIssue(args []string) error {
	fs := flag.NewFlagSet("issue", flag.ContinueOnError)
	keyFile := fs.String("key", "", "path to the private signing key (from `licensegen keygen`), or the literal \"dev\" to sign with the repo's test-only dev key (verifies in tests/e2e only, never in a build using a real prodPublicKeyB64)")
	customer := fs.String("customer", "", "customer name")
	edition := fs.String("edition", "", "enterprise")
	seats := fs.Int("seats", 1, "seat count")
	days := fs.Int("days", 365, "days until expiry")
	features := fs.String("features", "", "comma-separated extra features (usually leave unset; edition already implies its default features)")
	licenseID := fs.String("license-id", "", "license id (default: generated)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *keyFile == "" || *customer == "" || *edition == "" {
		return fmt.Errorf("--key, --customer and --edition are required")
	}
	if *edition != license.EditionEnterprise {
		return fmt.Errorf("--edition must be %q", license.EditionEnterprise)
	}
	var priv ed25519.PrivateKey
	if *keyFile == "dev" {
		priv = license.DevSigner()
	} else {
		keyB64, err := os.ReadFile(*keyFile)
		if err != nil {
			return fmt.Errorf("reading key file: %w", err)
		}
		seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(keyB64)))
		if err != nil || len(seed) != ed25519.PrivateKeySize {
			return fmt.Errorf("%s does not hold a valid Ed25519 private key", *keyFile)
		}
		priv = ed25519.PrivateKey(seed)
	}

	id := *licenseID
	if id == "" {
		var b [9]byte
		_, _ = rand.Read(b[:])
		id = "lic_" + base64.RawURLEncoding.EncodeToString(b[:])
	}
	var feats []string
	for _, f := range strings.Split(*features, ",") {
		if f = strings.TrimSpace(f); f != "" {
			feats = append(feats, f)
		}
	}
	now := time.Now().UTC()
	lic := &license.License{
		LicenseID: id,
		Customer:  *customer,
		Product:   "harbour",
		Edition:   *edition,
		Features:  feats,
		Seats:     *seats,
		IssuedAt:  now,
		ExpiresAt: now.Add(time.Duration(*days) * 24 * time.Hour),
	}
	tok, err := license.Sign(priv, lic)
	if err != nil {
		return err
	}
	fmt.Println(tok)
	fmt.Fprintf(os.Stderr, "issued %s edition license %s for %s, %d seats, expires %s\n",
		lic.Edition, lic.LicenseID, lic.Customer, lic.Seats, lic.ExpiresAt.Format("2006-01-02"))
	return nil
}
