package admin

// CopySourceOf exposes copySourceOf to the external tests.
func CopySourceOf(bucket, key, versionID string) string { return copySourceOf(bucket, key, versionID) }
