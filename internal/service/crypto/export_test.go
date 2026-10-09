package crypto

// KeyBytes exposes the unsealed key of k to the redaction tests.
func KeyBytes(k ObjectKey) []byte { return k.key[:] }
