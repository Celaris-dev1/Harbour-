package license

import (
	"strings"
	"testing"
	"time"
)

// Package tests sign with the dev key, so trust it here; the regression test
// below turns that off to model a release build.
func init() { trustDevKey = true }

func TestReleaseBuildRejectsDevSignedLicense(t *testing.T) {
	trustDevKey = false
	t.Cleanup(func() { trustDevKey = true })
	tok, err := Sign(DevSigner(), testLicense(EditionEnterprise, time.Now().Add(24*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(tok); err == nil {
		t.Fatal("a license signed with the public dev key verified in a release build")
	}
}

func TestNonCanonicalSignatureEncodingRejected(t *testing.T) {
	tok, err := Sign(DevSigner(), testLicense(EditionEnterprise, time.Now().Add(24*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	// 64-byte signature -> 86 chars; the last char carries 2 unused bits.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := tok[len(tok)-1]
	variant := tok[:len(tok)-1] + string(alphabet[strings.IndexByte(alphabet, last)^1])
	if _, err := Verify(variant); err == nil {
		t.Fatal("non-canonical encoding of a valid signature verified")
	}
}
