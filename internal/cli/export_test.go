package cli

// LocalNameOn is LocalName for an explicit target system, so that the
// Windows rules are tested on every platform.
func LocalNameOn(rel string, windows bool) (string, error) { return localName(rel, windows) }
