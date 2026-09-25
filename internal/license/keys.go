package license

import (
	"crypto/ed25519"
	"encoding/base64"
)

func decodeStd(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }

// VendorPublicKeys are the Ed25519 public keys harbour trusts to sign license tokens. Verify
// accepts a signature from any key in this list, so the production key can be rotated by
// appending a new one here (old licenses signed by the old key keep verifying) and cutting
// a release.
//
// ---------------------------------------------------------------------------------------
// PRODUCTION KEY: replace devPublicKey below (or, better, add a second entry) with your
// real vendor public key before shipping a build that customers will use to verify paid
// licenses. To generate a real keypair and keep this repo's tests passing:
//
//	go run -tags licensegen ./tools/licensegen keygen --out priv.key
//	# priv.key now holds your private signing key -- keep it offline, NEVER commit it.
//	# it prints the matching public key; paste that into a const below.
//
// The private key never goes in this repo. Only the public key does.
// ---------------------------------------------------------------------------------------

// prodPublicKeyB64 is Prolu's production Ed25519 public key (std base64). The matching
// private key lives only in the owner's Google Apps Script properties.
const prodPublicKeyB64 = "UCXEclvh87t1apSNsbff7yYy8AyY8h++HNWSVfYseE4=" // Prolu production key (signed by the Grey autopay Apps Script)

// devPublicKeyB64 is a fixed development key pair used only by this repo's own tests, so
// tests don't need to mutate the trusted key list. Its private half is devPrivateKeyB64
// below, also test-only. Do not use it to sign real licenses: it is public, checked into
// git, and anyone can sign tokens with it.
const devPublicKeyB64 = "1LqUDvnUhMaTgEPSu1CJTCEobuLw6FurDSTYXw3c+fY="
const devPrivateKeyB64 = "sSHAG3gkQPnLGf5x9PxyG+9X7lcjK0UAVBZYs7KxHrjUupQO+dSExpOAQ9K7UIlMIShu4vDoW6sNJNhfDdz59g=="

// VendorPublicKeys returns the trusted verification keys: the production placeholder plus
// the fixed dev key used by this repo's tests. Both are compile-time constants; there is
// no runtime fetch of any kind (offline verification only).
func VendorPublicKeys() []ed25519.PublicKey {
	keys := []ed25519.PublicKey{mustDecodeKey(prodPublicKeyB64)}
	if trustDevKey {
		keys = append(keys, mustDecodeKey(devPublicKeyB64))
	}
	return keys
}

// trustDevKey makes the dev key a trusted vendor key. Its private half is in
// this file, so a release binary must never trust it: it is only set by builds
// tagged "licensedev" (e2e) and by tests via TrustDevKeyForTesting. NO environment
// variable may enable this -- see devtrust.go and devtrust_test.go.
var trustDevKey = false

// TrustDevKeyForTesting makes licenses signed by DevSigner verify. Tests only.
func TrustDevKeyForTesting() {
	trustDevKey = true
	Reload()
}

// DevSigner returns the fixed development Ed25519 keypair used by this repo's own tests
// and by `scripts/e2e.sh` (via `licensegen issue --key dev`, binaries built with
// -tags licensedev) to issue throwaway licenses without touching any real signing key. It
// must never be used for a real customer license: the private key is public (checked into
// this source file).
func DevSigner() ed25519.PrivateKey { return mustDecodeKeySeed(devPrivateKeyB64) }

func mustDecodeKey(b64s string) ed25519.PublicKey {
	b, err := decodeStd(b64s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		panic("license: malformed embedded public key")
	}
	return ed25519.PublicKey(b)
}

func mustDecodeKeySeed(b64s string) ed25519.PrivateKey {
	b, err := decodeStd(b64s)
	if err != nil || len(b) != ed25519.PrivateKeySize {
		panic("license: malformed embedded dev private key")
	}
	return ed25519.PrivateKey(b)
}
