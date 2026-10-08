package lock

// Resource builds the canonical resource name for a volume and path: the
// volume alone for bucket-level locks, "volume/path" for objects.
func Resource(volume, path string) string {
	if path == "" {
		return volume
	}
	return volume + "/" + path
}
