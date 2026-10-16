package dns01

// WithNameserverPort sets the port of the authoritative nameservers the
// checker finds (tests: a server on a loopback port, not 53).
func WithNameserverPort(port string) CheckerOption {
	return func(c *checker) { c.nsPort = port }
}

// ParentNames exposes parentNames.
var ParentNames = parentNames
