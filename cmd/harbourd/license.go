package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/Celaris-dev1/Harbour-/internal/license"
)

const licenseUsage = `harbourd license show            — current license state, customer, edition, features, seats, expiry
harbourd license verify <token|file>
                              — verify a license token (or a file holding one) offline and print it

env: HARBOUR_LICENSE (token) or HARBOUR_LICENSE_FILE (path), read by every harbourd command that
     gates an Enterprise feature. See PRICING.md and the README's LICENSING section.
`

func cmdLicense(args []string) error {
	if len(args) == 0 {
		fmt.Print(licenseUsage)
		return nil
	}
	switch args[0] {
	case "show":
		asJSON := false
		for _, a := range args[1:] {
			if a == "--json" {
				asJSON = true
			}
		}
		return cmdLicenseShow(asJSON)
	case "verify":
		if len(args) < 2 {
			return fmt.Errorf("usage: harbourd license verify <token|file>")
		}
		return cmdLicenseVerify(args[1])
	case "help", "-h", "--help":
		fmt.Print(licenseUsage)
		return nil
	default:
		fmt.Print(licenseUsage)
		return fmt.Errorf("harbourd license: unknown subcommand %q", args[0])
	}
}

func cmdLicenseShow(asJSON bool) error {
	st := license.Reload()
	if asJSON {
		out := map[string]any{"state": st.State}
		if st.Warning != "" {
			out["warning"] = st.Warning
		}
		if st.License != nil {
			out["license"] = st.License
		}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	fmt.Printf("state:    %s\n", st.State)
	if st.License == nil {
		fmt.Println("edition:  community (free) — core Harbour works fully; see PRICING.md for Enterprise")
		if st.Warning != "" {
			fmt.Fprintln(os.Stderr, st.Warning)
		}
		return nil
	}
	l := st.License
	fmt.Printf("customer: %s\n", l.Customer)
	fmt.Printf("edition:  %s\n", l.Edition)
	feats := l.Features
	if len(feats) == 0 {
		feats = []string{"(edition defaults)"}
	}
	fmt.Printf("features: %s\n", strings.Join(feats, ", "))
	fmt.Printf("seats:    %d licensed\n", l.Seats)
	fmt.Printf("issued:   %s\n", l.IssuedAt.Format("2006-01-02"))
	fmt.Printf("expires:  %s\n", l.ExpiresAt.Format("2006-01-02"))
	if st.Warning != "" {
		fmt.Fprintln(os.Stderr, st.Warning)
	}
	return nil
}

func cmdLicenseVerify(arg string) error {
	tok := arg
	if b, err := os.ReadFile(arg); err == nil {
		tok = strings.TrimSpace(string(b))
	}
	lic, err := license.Verify(tok)
	if err != nil {
		return fmt.Errorf("invalid license: %w", err)
	}
	b, err := json.MarshalIndent(lic, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}
