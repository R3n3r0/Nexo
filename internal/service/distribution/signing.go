package distribution

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
)

// Query parameters and cookies of the signed URLs and cookies.
const (
	ParamExpires   = "Expires"
	ParamSignature = "Signature"
	ParamKeyPairID = "Key-Pair-Id"
	ParamPolicy    = "Policy"

	CookiePolicy    = "CloudFront-Policy"
	CookieExpires   = "CloudFront-Expires"
	CookieSignature = "CloudFront-Signature"
	CookieKeyPairID = "CloudFront-Key-Pair-Id"

	// maxPolicySize bounds a custom policy.
	maxPolicySize = 4 << 10
	// maxSignatureSize bounds an encoded signature (RSA-4096: 684).
	maxSignatureSize = 1 << 10
)

// signingParams reports the query parameters of the signature, removed from
// the URL whose resource the policy names.
func signingParam(name string) bool {
	switch name {
	case ParamExpires, ParamSignature, ParamKeyPairID, ParamPolicy:
		return true
	}
	return false
}

// policyDoc is a CloudFront policy. Field order and omitempty make the
// canned policy encode exactly as CloudFront writes it.
type policyDoc struct {
	Statement []policyStatement `json:"Statement"`
}

type policyStatement struct {
	Resource  string          `json:"Resource,omitempty"`
	Condition policyCondition `json:"Condition"`
}

type policyCondition struct {
	DateLessThan    *epochCondition `json:"DateLessThan,omitempty"`
	DateGreaterThan *epochCondition `json:"DateGreaterThan,omitempty"`
	IPAddress       *ipCondition    `json:"IpAddress,omitempty"`
}

type epochCondition struct {
	EpochTime int64 `json:"AWS:EpochTime"`
}

type ipCondition struct {
	SourceIP string `json:"AWS:SourceIp"`
}

// encodePolicy encodes a policy without HTML escaping (a resource URL
// keeps its '&').
func encodePolicy(p policyDoc) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// cannedPolicy is the policy of a canned signed URL or cookie.
func cannedPolicy(resource string, expires int64) ([]byte, error) {
	return encodePolicy(policyDoc{Statement: []policyStatement{{
		Resource:  resource,
		Condition: policyCondition{DateLessThan: &epochCondition{EpochTime: expires}},
	}}})
}

// safeEncoding is the CloudFront URL-safe base64: '+' → '-', '=' → '_',
// '/' → '~'.
func safeEncode(b []byte) string {
	return strings.NewReplacer("+", "-", "=", "_", "/", "~").Replace(base64.StdEncoding.EncodeToString(b))
}

func safeDecode(s string, limit int) ([]byte, error) {
	if len(s) > limit*4/3+4 {
		return nil, fmt.Errorf("too long")
	}
	return base64.StdEncoding.DecodeString(strings.NewReplacer("-", "+", "_", "=", "~", "/").Replace(s))
}

// resourceURL returns the URL a policy names for a request: scheme, host
// (lowercase), escaped path and the query without the signature
// parameters, in their order.
func resourceURL(scheme, host, escapedPath, rawQuery string) string {
	var kept []string
	for _, part := range strings.Split(rawQuery, "&") {
		if part == "" {
			continue
		}
		name, _, _ := strings.Cut(part, "=")
		if n, err := url.QueryUnescape(name); err == nil && signingParam(n) {
			continue
		}
		kept = append(kept, part)
	}
	if escapedPath == "" {
		escapedPath = "/"
	}
	out := strings.ToLower(scheme) + "://" + strings.ToLower(host) + escapedPath
	if len(kept) > 0 {
		out += "?" + strings.Join(kept, "&")
	}
	return out
}

// wildcardMatch matches s against a pattern where '*' is any run of
// characters ('/' included) and '?' any character: the Resource of a
// custom policy. Linear backtracking on the last star.
func wildcardMatch(pattern, s string) bool {
	p, i := 0, 0
	star, mark := -1, 0
	for i < len(s) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == s[i]):
			p++
			i++
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, i
			p++
		case star >= 0:
			p = star + 1
			mark++
			i = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// --- Signer ---------------------------------------------------------------------

type signer struct {
	clock clock.Clock
}

var _ Signer = signer{}

// SignerDeps are the dependencies of the signer.
type SignerDeps struct {
	// Clock checks that the expiration is in the future (default wall
	// clock).
	Clock clock.Clock
}

// NewSigner returns the Signer of the URLs and cookies.
func NewSigner(deps SignerDeps) Signer {
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	return signer{clock: deps.Clock}
}

// signedPolicy is a policy ready to be sent: canned (expires) or custom.
type signedPolicy struct {
	canned    bool
	expires   int64
	policy    []byte
	signature []byte
}

// sign builds and signs the policy of in.
func (s signer) sign(in SignInput) (*url.URL, signedPolicy, error) {
	u, err := url.Parse(in.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, signedPolicy{}, fmt.Errorf("%w: %q is not an http(s) URL", ErrInvalid, in.URL)
	}
	if in.KeyPairID == "" {
		return nil, signedPolicy{}, fmt.Errorf("%w: the key pair id is required", ErrInvalid)
	}
	if in.Expires.IsZero() || !in.Expires.After(s.clock.Now()) {
		return nil, signedPolicy{}, fmt.Errorf("%w: the expiration must be in the future", ErrInvalid)
	}
	key, err := parsePrivateKeyPEM(in.PrivateKeyPEM)
	if err != nil {
		return nil, signedPolicy{}, err
	}
	for name := range u.Query() {
		if signingParam(name) {
			return nil, signedPolicy{}, fmt.Errorf("%w: the URL already carries %s", ErrInvalid, name)
		}
	}
	sp := signedPolicy{expires: in.Expires.Unix()}
	if in.NotBefore.IsZero() && in.IPRange == "" && in.Resource == "" {
		sp.canned = true
		sp.policy, err = cannedPolicy(resourceURL(u.Scheme, u.Host, u.EscapedPath(), u.RawQuery), sp.expires)
	} else {
		st := policyStatement{
			Resource:  in.Resource,
			Condition: policyCondition{DateLessThan: &epochCondition{EpochTime: sp.expires}},
		}
		if st.Resource == "" {
			st.Resource = resourceURL(u.Scheme, u.Host, u.EscapedPath(), u.RawQuery)
		}
		if !in.NotBefore.IsZero() {
			st.Condition.DateGreaterThan = &epochCondition{EpochTime: in.NotBefore.Unix()}
		}
		if in.IPRange != "" {
			prefixes, perr := parseIPEntries([]string{in.IPRange})
			if perr != nil {
				return nil, signedPolicy{}, perr
			}
			st.Condition.IPAddress = &ipCondition{SourceIP: prefixes[0].String()}
		}
		sp.policy, err = encodePolicy(policyDoc{Statement: []policyStatement{st}})
	}
	if err != nil {
		return nil, signedPolicy{}, fmt.Errorf("distribution: encode policy: %w", err)
	}
	if sp.signature, err = signPolicy(key, sp.policy); err != nil {
		return nil, signedPolicy{}, err
	}
	return u, sp, nil
}

// SignURL implements Signer.
func (s signer) SignURL(_ context.Context, in SignInput) (string, error) {
	u, sp, err := s.sign(in)
	if err != nil {
		return "", err
	}
	params := []string{}
	if sp.canned {
		params = append(params, ParamExpires+"="+strconv.FormatInt(sp.expires, 10))
	} else {
		params = append(params, ParamPolicy+"="+url.QueryEscape(safeEncode(sp.policy)))
	}
	params = append(params,
		ParamSignature+"="+url.QueryEscape(safeEncode(sp.signature)),
		ParamKeyPairID+"="+url.QueryEscape(in.KeyPairID))
	if u.RawQuery != "" {
		u.RawQuery += "&"
	}
	u.RawQuery += strings.Join(params, "&")
	return u.String(), nil
}

// SignCookies implements Signer.
func (s signer) SignCookies(_ context.Context, in SignInput) ([]SignedCookie, error) {
	_, sp, err := s.sign(in)
	if err != nil {
		return nil, err
	}
	exp := time.Unix(sp.expires, 0).UTC()
	var out []SignedCookie
	if sp.canned {
		out = append(out, SignedCookie{Name: CookieExpires, Value: strconv.FormatInt(sp.expires, 10), Expires: exp})
	} else {
		out = append(out, SignedCookie{Name: CookiePolicy, Value: safeEncode(sp.policy), Expires: exp})
	}
	return append(out,
		SignedCookie{Name: CookieSignature, Value: safeEncode(sp.signature), Expires: exp},
		SignedCookie{Name: CookieKeyPairID, Value: in.KeyPairID, Expires: exp},
	), nil
}

// parsePrivateKeyPEM parses an Ed25519 or RSA private key (PKCS #8, or
// PKCS #1 for RSA).
func parsePrivateKeyPEM(data string) (crypto.Signer, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(data)))
	if block == nil {
		return nil, fmt.Errorf("%w: the private key is not PEM", ErrInvalid)
	}
	var (
		key any
		err error
	)
	switch block.Type {
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("%w: PEM block %q is not a private key", ErrInvalid, block.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: private key: %v", ErrInvalid, err)
	}
	switch k := key.(type) {
	case ed25519.PrivateKey:
		return k, nil
	case *rsa.PrivateKey:
		if bits := k.N.BitLen(); bits < minRSABits || bits > maxRSABits {
			return nil, fmt.Errorf("%w: RSA keys of %d to %d bits only", ErrInvalid, minRSABits, maxRSABits)
		}
		return k, nil
	default:
		return nil, fmt.Errorf("%w: only Ed25519 and RSA private keys are supported", ErrInvalid)
	}
}

// signPolicy signs a policy: Ed25519 over the bytes, RSA PKCS #1 v1.5
// over their SHA-256.
func signPolicy(key crypto.Signer, policy []byte) ([]byte, error) {
	switch k := key.(type) {
	case ed25519.PrivateKey:
		return ed25519.Sign(k, policy), nil
	case *rsa.PrivateKey:
		sum := sha256.Sum256(policy)
		sig, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, sum[:])
		if err != nil {
			return nil, fmt.Errorf("distribution: sign: %w", err)
		}
		return sig, nil
	default:
		return nil, fmt.Errorf("%w: unsupported key", ErrInvalid)
	}
}

// verifyPolicy verifies the signature of a policy.
func verifyPolicy(k parsedKey, policy, sig []byte) bool {
	switch pub := k.pub.(type) {
	case ed25519.PublicKey:
		return len(sig) == ed25519.SignatureSize && ed25519.Verify(pub, policy, sig)
	case *rsa.PublicKey:
		sum := sha256.Sum256(policy)
		return rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig) == nil
	}
	return false
}

// --- verification --------------------------------------------------------------

// signatureParts are the signature parameters of a request, from its
// query or its cookies.
type signatureParts struct {
	source    string
	expires   string
	policy    string
	signature string
	keyPairID string
}

// signatureOf extracts the signature of the query, or else of the cookies.
func signatureOf(rawQuery string, cookies map[string]string) (signatureParts, bool) {
	q, err := url.ParseQuery(rawQuery)
	if err == nil && q.Get(ParamSignature) != "" && q.Get(ParamKeyPairID) != "" {
		return signatureParts{source: "url", expires: q.Get(ParamExpires), policy: q.Get(ParamPolicy),
			signature: q.Get(ParamSignature), keyPairID: q.Get(ParamKeyPairID)}, true
	}
	if cookies[CookieSignature] != "" && cookies[CookieKeyPairID] != "" {
		return signatureParts{source: "cookie", expires: cookies[CookieExpires], policy: cookies[CookiePolicy],
			signature: cookies[CookieSignature], keyPairID: cookies[CookieKeyPairID]}, true
	}
	return signatureParts{}, false
}

// VerifySignedRequest implements SignedRequestVerifier.
func (s *service) VerifySignedRequest(_ context.Context, in SignedRequest) error {
	return verifySigned(s.keys.Load(), in, s.deps.Clock.Now())
}

// verifySigned verifies a signed request against the keys of ks.
func verifySigned(ks *keySet, in SignedRequest, now time.Time) error {
	if !in.Now.IsZero() {
		now = in.Now
	}
	parts, ok := signatureOf(in.RawQuery, in.Cookies)
	if !ok {
		return ErrMissingSignature
	}
	key, ok := ks.keys[parts.keyPairID]
	if !ok || key.group != in.KeyGroup {
		return fmt.Errorf("%w: unknown key pair %q", ErrBadSignature, truncate(parts.keyPairID, 64))
	}
	sig, err := safeDecode(parts.signature, maxSignatureSize)
	if err != nil {
		return fmt.Errorf("%w: malformed signature", ErrBadSignature)
	}
	// The URL the policy names: the cookies sign a resource pattern too,
	// matched against the URL of the request with its query.
	resource := resourceURL(in.Scheme, in.Host, in.EscapedPath, in.RawQuery)
	var policy []byte
	switch {
	case parts.policy != "":
		if policy, err = safeDecode(parts.policy, maxPolicySize); err != nil || len(policy) > maxPolicySize {
			return fmt.Errorf("%w: malformed policy", ErrBadSignature)
		}
	case parts.expires != "":
		exp, perr := strconv.ParseInt(parts.expires, 10, 64)
		if perr != nil || exp <= 0 {
			return fmt.Errorf("%w: malformed expiration", ErrBadSignature)
		}
		if policy, err = cannedPolicy(resource, exp); err != nil {
			return fmt.Errorf("%w: policy", ErrBadSignature)
		}
	default:
		return fmt.Errorf("%w: neither Expires nor Policy", ErrBadSignature)
	}
	if !verifyPolicy(key, policy, sig) {
		return ErrBadSignature
	}
	return checkPolicy(policy, resource, in.ClientIP, now)
}

// checkPolicy applies the conditions of a verified policy.
func checkPolicy(raw []byte, resource, clientIP string, now time.Time) error {
	var p policyDoc
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil || len(p.Statement) != 1 {
		return fmt.Errorf("%w: the policy must hold one statement", ErrBadSignature)
	}
	st := p.Statement[0]
	if st.Resource != "" && !wildcardMatch(st.Resource, resource) {
		return fmt.Errorf("%w: the policy does not cover %s", domain.ErrAccessDenied, truncate(resource, 256))
	}
	c := st.Condition
	if c.DateLessThan == nil {
		return fmt.Errorf("%w: the policy has no DateLessThan", ErrBadSignature)
	}
	if now.Unix() >= c.DateLessThan.EpochTime {
		return ErrSignatureExpired
	}
	if c.DateGreaterThan != nil && now.Unix() <= c.DateGreaterThan.EpochTime {
		return ErrSignatureExpired
	}
	if c.IPAddress != nil {
		prefixes, err := parseIPEntries([]string{c.IPAddress.SourceIP})
		if err != nil {
			return fmt.Errorf("%w: malformed IpAddress", ErrBadSignature)
		}
		addr, err := netip.ParseAddr(clientIP)
		if err != nil || !prefixes[0].Contains(addr.Unmap()) {
			return fmt.Errorf("%w: client address outside the policy", domain.ErrAccessDenied)
		}
	}
	return nil
}

// truncate bounds a string echoed in an error.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
