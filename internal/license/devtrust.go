//go:build licensedev

package license

// Builds tagged licensedev (the e2e harness) trust the public dev key so
// throwaway licenses from `licensegen issue --key dev` verify. Never ship one.
func init() { trustDevKey = true }
