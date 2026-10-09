package accessrule

// Option configures the Service and the Evaluator.
type Option func(*options)

type options struct {
	// maxRules bounds the rules of a bucket.
	maxRules int
	// blockPublic and blockMode are the cluster-level block public access
	// used while the dynamic configuration does not set it.
	blockPublic bool
	blockMode   BlockPublicMode
}

func defaultOptions() options {
	return options{maxRules: DefaultMaxRules, blockMode: BlockModeReject}
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

// WithMaxRules bounds the number of rules of a bucket (default
// DefaultMaxRules).
func WithMaxRules(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.maxRules = n
		}
	}
}

// WithBlockPublicDefault sets the cluster-level block public access used
// while the dynamic configuration does not set it (default off, reject
// mode): the static configuration of the deployment.
func WithBlockPublicDefault(enabled bool, mode BlockPublicMode) Option {
	return func(o *options) {
		o.blockPublic = enabled
		if mode == BlockModeReject || mode == BlockModeIgnore {
			o.blockMode = mode
		}
	}
}
