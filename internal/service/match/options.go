package match

// Option configures the matcher service.
type Option func(*options)

type options struct {
	// nfc normalizes keys and patterns to the Unicode NFC form.
	nfc bool
	// maxLength bounds the length of a pattern, in characters.
	maxLength int
}

func defaultOptions() options {
	return options{nfc: true, maxLength: MaxPatternLength}
}

// newOptions applies opts (nil entries are ignored) over the defaults.
func newOptions(opts []Option) options {
	o := defaultOptions()
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// WithNFC enables (the default) or disables the Unicode NFC normalization
// of keys and patterns.
func WithNFC(enabled bool) Option {
	return func(o *options) { o.nfc = enabled }
}

// WithMaxPatternLength bounds the length of a pattern in characters
// (default MaxPatternLength).
func WithMaxPatternLength(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.maxLength = n
		}
	}
}
