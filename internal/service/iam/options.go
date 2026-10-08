package iam

// Option configures the IAM service.
type Option func(*options)

type options struct {
	// exportKDF derives the key of Export documents.
	exportKDF KDFParams
}

func defaultOptions() options {
	return options{exportKDF: defaultKDFParams()}
}

// WithExportKDFParams sets the Argon2id parameters of the documents
// written by Export (default: 1 pass, 64 MiB, 4 threads). Import reads the
// parameters recorded in the document.
func WithExportKDFParams(p KDFParams) Option {
	return func(o *options) { o.exportKDF = p.withDefaults() }
}
