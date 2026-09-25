package license

import (
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
