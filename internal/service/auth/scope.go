package auth

import "context"

// Services of a SigV4 credential scope ("<key>/<date>/<region>/<service>/
// aws4_request"). A signature is valid for the service it was computed for
// only: the verifier refuses a scope naming another service than the one
// the request is addressed to (WithExpectedService).
const (
	// ServiceS3 is the service of the S3 API (the default).
	ServiceS3 = "s3"
	// ServiceSTS is the service of the STS API (AssumeRole).
	ServiceSTS = "sts"
)

type expectedServiceKey struct{}

// WithExpectedService returns a context telling the verifier which service
// the request is addressed to (ServiceS3 or ServiceSTS); the transport,
// which routes the request, decides it. Without it, or with an empty
// service, the verifier expects ServiceS3.
func WithExpectedService(ctx context.Context, service string) context.Context {
	return context.WithValue(ctx, expectedServiceKey{}, service)
}

// ExpectedService returns the service set by WithExpectedService, or
// ServiceS3.
func ExpectedService(ctx context.Context) string {
	if s, ok := ctx.Value(expectedServiceKey{}).(string); ok && s != "" {
		return s
	}
	return ServiceS3
}
