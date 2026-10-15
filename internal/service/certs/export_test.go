package certs

// WithoutKeystoreWatcher disables the file notifications of the keystore:
// only the poller and Reload notice the changes (tests of the poller).
func WithoutKeystoreWatcher() KeystoreOption {
	return func(o *keystoreOptions) { o.noWatch = true }
}
