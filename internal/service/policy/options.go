package policy

// Option configures the policy engine.
type Option func(*options)

type options struct {
	// maxPolicySize bounds JSON documents (default 20 KiB, as AWS).
	maxPolicySize int
}

func defaultOptions() options {
	return options{maxPolicySize: defaultMaxPolicySize}
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

// WithMaxPolicySize bounds the size of policy documents (default 20 KiB).
func WithMaxPolicySize(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.maxPolicySize = n
		}
	}
}
