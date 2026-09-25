package license

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"strings"
	"testing"
	"time"
)

func testLicense(edition string, expires time.Time) *License {
	return &License{
		LicenseID: "lic_test", Customer: "Acme Inc", Product: "harbour",
		Edition: edition, Seats: 5, IssuedAt: time.Now().Add(-time.Hour), ExpiresAt: expires,
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	lic := testLicense(EditionEnterprise, time.Now().Add(30*24*time.Hour))
	tok, err := Sign(DevSigner(), lic)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.Customer != "Acme Inc" || got.Edition != EditionEnterprise || got.Seats != 5 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestVerifyTamperedPayload(t *testing.T) {
	lic := testLicense(EditionEnterprise, time.Now().Add(30*24*time.Hour))
	tok, err := Sign(DevSigner(), lic)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(tok, ".", 2)
	tampered := strings.Replace(parts[0], "e", "E", 1) + "." + parts[1]
	if tampered == tok {
		t.Skip("payload had no 'e' to flip")
	}
	if _, err := Verify(tampered); err == nil {
		t.Fatal("tampered payload verified")
	}
}

func TestVerifyTamperedSignature(t *testing.T) {
	lic := testLicense(EditionEnterprise, time.Now().Add(30*24*time.Hour))
	tok, err := Sign(DevSigner(), lic)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(tok, ".", 2)
	tampered := parts[0] + "." + strings.Replace(parts[1], "A", "B", 1)
	if tampered == tok {
		t.Skip("sig had no 'A' to flip")
	}
	if _, err := Verify(tampered); err == nil {
		t.Fatal("tampered signature verified")
	}
}

func TestVerifyWrongKey(t *testing.T) {
	_, wrongPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	lic := testLicense(EditionEnterprise, time.Now().Add(30*24*time.Hour))
	tok, err := Sign(wrongPriv, lic)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(tok); err == nil {
		t.Fatal("signature from an untrusted key verified")
	}
}

func TestVerifyMalformedToken(t *testing.T) {
	for _, bad := range []string{"", "not-a-token", "abc.", ".abc", "abc.def.ghi"} {
		if _, err := Verify(bad); err == nil {
			t.Fatalf("malformed token %q verified", bad)
		}
	}
}

func TestVerifyWrongProduct(t *testing.T) {
	lic := testLicense(EditionEnterprise, time.Now().Add(time.Hour))
	lic.Product = "not-harbour"
	tok, err := Sign(DevSigner(), lic)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(tok); err == nil {
		t.Fatal("license for another product verified")
	}
}

func TestResolveStates(t *testing.T) {
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	valid := testLicense(EditionEnterprise, now.Add(24*time.Hour))
	if st := resolve(valid, now); st.State != StateValid {
		t.Fatalf("want valid, got %s", st.State)
	}

	justExpired := testLicense(EditionEnterprise, now.Add(-time.Minute))
	if st := resolve(justExpired, now); st.State != StateGrace {
		t.Fatalf("want grace just after expiry, got %s", st.State)
	}

	edgeOfGrace := testLicense(EditionEnterprise, now.Add(-Grace).Add(time.Minute))
	if st := resolve(edgeOfGrace, now); st.State != StateGrace {
		t.Fatalf("want grace at edge, got %s", st.State)
	}

	pastGrace := testLicense(EditionEnterprise, now.Add(-Grace).Add(-time.Minute))
	if st := resolve(pastGrace, now); st.State != StateExpired {
		t.Fatalf("want expired past grace, got %s", st.State)
	}

	graceSt := resolve(justExpired, now)
	if !graceSt.Active() {
		t.Fatal("grace state should still be Active")
	}
	if !graceSt.HasFeature(FeatureMultiWorker) {
		t.Fatal("grace state should still unlock enterprise features")
	}

	expSt := resolve(pastGrace, now)
	if expSt.Active() {
		t.Fatal("expired state should not be Active")
	}
	if expSt.HasFeature(FeatureMultiWorker) {
		t.Fatal("expired state should not unlock features")
	}
}

func TestHasFeatureByEdition(t *testing.T) {
	ent := &License{Edition: EditionEnterprise}
	if !ent.HasFeature(FeatureMultiWorker) {
		t.Error("enterprise should have multi-worker")
	}

	none := &License{Edition: ""}
	if none.HasFeature(FeatureMultiWorker) {
		t.Error("no edition should not unlock enterprise features")
	}

	custom := &License{Edition: "", Features: []string{FeatureMultiWorker}}
	if !custom.HasFeature(FeatureMultiWorker) {
		t.Error("explicit feature grant should override edition default")
	}
}

func TestLoadFromEnvToken(t *testing.T) {
	lic := testLicense(EditionEnterprise, time.Now().Add(time.Hour))
	tok, err := Sign(DevSigner(), lic)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARBOUR_LICENSE", tok)
	t.Setenv("HARBOUR_LICENSE_FILE", "")
	st := Reload()
	if st.State != StateValid {
		t.Fatalf("want valid, got %s (%s)", st.State, st.Warning)
	}
	if st.License.Customer != "Acme Inc" {
		t.Fatalf("unexpected license: %+v", st.License)
	}
}

func TestLoadFromEnvFile(t *testing.T) {
	lic := testLicense(EditionEnterprise, time.Now().Add(time.Hour))
	tok, err := Sign(DevSigner(), lic)
	if err != nil {
		t.Fatal(err)
	}
	f := t.TempDir() + "/harbour.lic"
	if err := writeFile(f, tok); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARBOUR_LICENSE", "")
	t.Setenv("HARBOUR_LICENSE_FILE", f)
	st := Reload()
	if st.State != StateValid {
		t.Fatalf("want valid, got %s (%s)", st.State, st.Warning)
	}
}

func TestLoadNoLicenseIsCommunity(t *testing.T) {
	t.Setenv("HARBOUR_LICENSE", "")
	t.Setenv("HARBOUR_LICENSE_FILE", "")
	st := Reload()
	if st.State != StateNone {
		t.Fatalf("want none, got %s", st.State)
	}
	if st.Warning != "" {
		t.Fatalf("no license configured should not warn, got %q", st.Warning)
	}
	if st.Active() {
		t.Fatal("community should not be Active")
	}
}

func TestLoadTamperedTokenFallsBackToCommunityWithWarning(t *testing.T) {
	lic := testLicense(EditionEnterprise, time.Now().Add(time.Hour))
	tok, err := Sign(DevSigner(), lic)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(tok, ".", 2)
	tampered := parts[0] + "x." + parts[1]
	t.Setenv("HARBOUR_LICENSE", tampered)
	t.Setenv("HARBOUR_LICENSE_FILE", "")
	st := Reload()
	if st.State != StateNone {
		t.Fatalf("tampered license should fall back to none, got %s", st.State)
	}
	if st.Warning == "" {
		t.Fatal("tampered license should warn")
	}
}

func TestRequireGating(t *testing.T) {
	t.Setenv("HARBOUR_LICENSE", "")
	t.Setenv("HARBOUR_LICENSE_FILE", "")
	Reload()
	if err := Require(FeatureMultiWorker); err == nil {
		t.Fatal("feature should be gated with no license")
	}

	entLic := testLicense(EditionEnterprise, time.Now().Add(time.Hour))
	tok, _ := Sign(DevSigner(), entLic)
	t.Setenv("HARBOUR_LICENSE", tok)
	Reload()
	if err := Require(FeatureMultiWorker); err != nil {
		t.Fatalf("enterprise license should unlock feature: %v", err)
	}

	t.Setenv("HARBOUR_LICENSE", "")
	Reload()
}

func TestSeatWarning(t *testing.T) {
	st := &Status{State: StateValid, License: testLicense(EditionEnterprise, time.Now().Add(time.Hour))}
	st.License.Seats = 5
	if w := SeatWarning(st, 5); w != "" {
		t.Fatalf("at seat limit should not warn, got %q", w)
	}
	if w := SeatWarning(st, 6); w == "" {
		t.Fatal("over seat limit should warn")
	}
	if w := SeatWarning(nil, 100); w != "" {
		t.Fatal("nil status should not warn")
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
