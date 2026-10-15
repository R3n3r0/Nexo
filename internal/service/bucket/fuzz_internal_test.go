package bucket

import (
	"errors"
	"testing"

	"github.com/r3n3r0/nexo/internal/domain"
)

// FuzzParseCORS checks the parser of the CORSConfiguration documents (PUT
// ?cors, the console): no panic, failures are S3 errors, and a valid
// configuration answers any origin, method and request headers without
// panicking, with a rule that allows them.
func FuzzParseCORS(f *testing.F) {
	for _, seed := range []string{
		`<CORSConfiguration><CORSRule><ID>r</ID><AllowedOrigin>https://*.example.com</AllowedOrigin><AllowedMethod>GET</AllowedMethod><AllowedMethod>PUT</AllowedMethod><AllowedHeader>x-amz-*</AllowedHeader><ExposeHeader>ETag</ExposeHeader><MaxAgeSeconds>3000</MaxAgeSeconds></CORSRule></CORSConfiguration>`,
		`<CORSConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><CORSRule><AllowedOrigin>*</AllowedOrigin><AllowedMethod>HEAD</AllowedMethod></CORSRule></CORSConfiguration>`,
		`<CORSConfiguration><CORSRule><AllowedOrigin>a**b</AllowedOrigin><AllowedMethod>PATCH</AllowedMethod><MaxAgeSeconds>-1</MaxAgeSeconds></CORSRule></CORSConfiguration>`,
		`<CORSConfiguration/>`, ``, `<CORSConfiguration><CORSRule>`,
	} {
		f.Add([]byte(seed), "https://app.example.com", "GET", "x-amz-date")
	}
	f.Fuzz(func(t *testing.T, doc []byte, origin, method, header string) {
		cfg, err := parseCORS(doc)
		if err == nil {
			err = cfg.Validate()
		}
		if err != nil {
			var s3err *domain.S3Error
			if !errors.As(err, &s3err) {
				t.Fatalf("parseCORS: unexpected error %T %v", err, err)
			}
			return
		}
		for _, headers := range [][]string{nil, {header}, {header, "content-type"}} {
			r, ok := cfg.Match(origin, method, headers)
			if ok && (!r.MatchesOrigin(origin) || !r.MatchesMethod(method) || !r.MatchesHeaders(headers)) {
				t.Fatalf("Match(%q, %q, %q) returned the rule %+v that does not allow the request", origin, method, headers, r)
			}
		}
	})
}
