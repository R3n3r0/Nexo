package s3

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/settings"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// CORS request and response headers.
const (
	headerOrigin                 = "Origin"
	headerVary                   = "Vary"
	headerACRequestMethod        = "Access-Control-Request-Method"
	headerACRequestHeaders       = "Access-Control-Request-Headers"
	headerACAllowOrigin          = "Access-Control-Allow-Origin"
	headerACAllowMethods         = "Access-Control-Allow-Methods"
	headerACAllowHeaders         = "Access-Control-Allow-Headers"
	headerACExposeHeaders        = "Access-Control-Expose-Headers"
	headerACMaxAge               = "Access-Control-Max-Age"
	headerACAllowCredentials     = "Access-Control-Allow-Credentials"
	globalCORSMaxAge             = 300
	corsPreflightNotAllowedError = "CORSResponse: This CORS request is not allowed. This is usually because the evalution of Origin, request method / Access-Control-Request-Method or Access-Control-Request-Headers are not whitelisted by the resource's CORS spec."
)

// globalCORSExposeHeaders are the response headers the deployment-wide
// CORS policy exposes: those the S3 browser clients need.
func globalCORSExposeHeaders() []string {
	return []string{"ETag", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified",
		"x-amz-request-id", "x-amz-id-2", "x-amz-version-id", "x-amz-delete-marker",
		"x-amz-checksum-crc32", "x-amz-checksum-crc32c", "x-amz-checksum-crc64nvme", "x-amz-checksum-sha1", "x-amz-checksum-sha256"}
}

// globalCORS is the deployment-wide CORS policy (api.cors_allow_origin):
// one rule allowing the S3 methods with any request header from origins
// (every origin when empty).
func globalCORS(origins []string) domain.CORSConfiguration {
	if len(origins) == 0 {
		origins = []string{"*"}
	}
	maxAge := globalCORSMaxAge
	return domain.CORSConfiguration{Rules: []domain.CORSRule{{
		AllowedOrigins: origins, AllowedMethods: domain.CORSMethods(), AllowedHeaders: []string{"*"},
		ExposeHeaders: globalCORSExposeHeaders(), MaxAgeSeconds: &maxAge,
	}}}
}

// corsHandler applies the CORS rules to the cross-origin requests of the
// S3 API (docs/02 S-BKT-06): it answers the preflight requests (OPTIONS
// with Access-Control-Request-Method) and adds the Access-Control-*
// headers of the matching rule to the other responses, errors included.
// The rules are those of the bucket the request addresses when it has a
// CORS configuration (they alone decide, as in S3), the deployment-wide
// policy (api.cors_allow_origin) otherwise: for the buckets without one,
// the service-level requests and the system routes.
type corsHandler struct {
	buckets  bucket.Service
	settings settings.Service
	enc      httpx.Encoder
	logger   *slog.Logger
	// resolve returns the bucket a request addresses ("" for none).
	resolve func(r *http.Request) string
}

// rules returns the CORS rules applying to a request of bucketName.
func (c *corsHandler) rules(ctx context.Context, bucketName string) domain.CORSConfiguration {
	if bucketName != "" && c.buckets != nil {
		cfg, err := c.buckets.GetCORSConfig(ctx, bucketName)
		switch {
		case err == nil:
			return cfg
		case !errors.Is(err, domain.ErrPolicyNotFound) && !errors.Is(err, domain.ErrBucketNotFound) && !errors.Is(err, domain.ErrInvalidBucketName):
			// Unreadable rules allow nothing: a failure must not widen
			// the access to a bucket.
			c.logger.WarnContext(ctx, "s3: CORS configuration unavailable", "bucket", bucketName, "err", err)
			return domain.CORSConfiguration{}
		}
	}
	var origins []string
	if c.settings != nil {
		api, err := c.settings.API(ctx)
		if err != nil {
			c.logger.WarnContext(ctx, "s3: CORS settings unavailable", "err", err)
			return domain.CORSConfiguration{}
		}
		origins = api.CORSAllowOrigin
	}
	return globalCORS(origins)
}

// wrap returns next with the CORS handling.
func (c *corsHandler) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get(headerOrigin)
		preflight := r.Method == http.MethodOptions
		if origin == "" {
			if preflight {
				c.enc.WriteS3Error(w, r, domain.NewS3Error(domain.ErrInvalidArgument, "BadRequest", "Insufficient information. Origin request header needed."))
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Add(headerVary, headerOrigin)
		rules := c.rules(r.Context(), c.resolve(r))
		if preflight {
			h.Add(headerVary, headerACRequestHeaders)
			h.Add(headerVary, headerACRequestMethod)
			method := r.Header.Get(headerACRequestMethod)
			if method == "" {
				c.enc.WriteS3Error(w, r, domain.NewS3Error(domain.ErrInvalidArgument, "BadRequest", "Insufficient information. Access-Control-Request-Method request header needed."))
				return
			}
			requested := splitHeaderList(r.Header.Values(headerACRequestHeaders))
			rule, ok := rules.Match(origin, method, requested)
			if !ok {
				c.enc.WriteS3Error(w, r, domain.NewS3Error(domain.ErrAccessDenied, "AccessForbidden", corsPreflightNotAllowedError))
				return
			}
			setCORSHeaders(h, rule, origin)
			if len(requested) > 0 {
				// Lowercase, as S3 answers (header names are
				// case-insensitive).
				h.Set(headerACAllowHeaders, strings.ToLower(strings.Join(requested, ", ")))
			}
			if rule.MaxAgeSeconds != nil {
				h.Set(headerACMaxAge, strconv.Itoa(*rule.MaxAgeSeconds))
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		// As S3, an actual request that declares the method (and headers)
		// of a preflight is matched with them.
		method, requested := r.Method, []string(nil)
		if m := r.Header.Get(headerACRequestMethod); m != "" {
			method, requested = m, splitHeaderList(r.Header.Values(headerACRequestHeaders))
		}
		if rule, ok := rules.Match(origin, method, requested); ok {
			setCORSHeaders(h, rule, origin)
		}
		next.ServeHTTP(w, r)
	})
}

// setCORSHeaders sets the Access-Control-* headers of a matching rule: the
// origin ("*" for a rule allowing every origin, otherwise the request's,
// with credentials), the allowed methods and the exposed headers.
func setCORSHeaders(h http.Header, rule domain.CORSRule, origin string) {
	if rule.AllowsAnyOrigin() {
		h.Set(headerACAllowOrigin, "*")
	} else {
		h.Set(headerACAllowOrigin, origin)
		h.Set(headerACAllowCredentials, "true")
	}
	h.Set(headerACAllowMethods, strings.Join(rule.AllowedMethods, ", "))
	if len(rule.ExposeHeaders) > 0 {
		h.Set(headerACExposeHeaders, strings.Join(rule.ExposeHeaders, ", "))
	}
}

// splitHeaderList splits comma-separated header values.
func splitHeaderList(values []string) []string {
	var out []string
	for _, v := range values {
		for _, item := range strings.Split(v, ",") {
			if item = strings.TrimSpace(item); item != "" {
				out = append(out, item)
			}
		}
	}
	return out
}

// putCORS stores the CORS configuration of a bucket.
func (h *bucketRoutes) putCORS(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	raw, err := readBody(r, maxXMLBody)
	switch {
	case errors.Is(err, errBodyTooLarge):
		h.fail(w, r, domain.NewS3Error(domain.ErrMalformedXML, "MaxMessageLengthExceeded", "Your request was too big."))
		return
	case err != nil:
		h.fail(w, r, err)
		return
	}
	if err := h.deps.Buckets.SetCORS(r.Context(), name, raw); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}
