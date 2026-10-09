package cli

import (
	"encoding/base64"
	"encoding/hex"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
)

// EncryptionRule applies a server-side encryption to the objects of a
// remote location (docs/02 S-OBJ-13): the --enc-s3, --enc-kms and --enc-c
// flags of the transfer commands.
type EncryptionRule struct {
	// Alias, Bucket and Prefix select the objects: the keys of the bucket
	// that start with Prefix.
	Alias  string
	Bucket string
	Prefix string
	// SSE is the encryption: SSE-S3, SSE-KMS (with an optional key) or
	// SSE-C (with the key of the client).
	SSE domain.SSERequest
}

// matches reports whether the rule applies to a remote target.
func (r EncryptionRule) matches(t Target) bool {
	return t.IsRemote() && t.Alias == r.Alias && t.Bucket == r.Bucket && strings.HasPrefix(t.Key, r.Prefix)
}

// EncryptionRules select the encryption of each object: the matching rule
// with the longest prefix wins.
type EncryptionRules []EncryptionRule

// For returns the encryption of a remote target (zero when no rule
// matches, or for a local path).
func (rs EncryptionRules) For(t Target) domain.SSERequest {
	best := -1
	for i, r := range rs {
		if r.matches(t) && (best < 0 || len(r.Prefix) > len(rs[best].Prefix)) {
			best = i
		}
	}
	if best < 0 {
		return domain.SSERequest{}
	}
	return rs[best].SSE
}

// CustomerKeyFor returns the SSE-C key of a remote target (zero when the
// matching rule is not SSE-C): the key needed to read the object.
func (rs EncryptionRules) CustomerKeyFor(t Target) domain.SSERequest {
	sse := rs.For(t)
	if sse.Type != domain.SSEC {
		return domain.SSERequest{}
	}
	return sse
}

// EncryptionFlags are the values of the --enc-s3, --enc-kms and --enc-c
// flags: each value is a comma-separated list.
type EncryptionFlags struct {
	// S3 holds ALIAS/BUCKET[/PREFIX] locations encrypted with SSE-S3.
	S3 []string
	// KMS holds ALIAS/BUCKET[/PREFIX][=KEY] locations encrypted with
	// SSE-KMS (the default key of the KMS without KEY).
	KMS []string
	// C holds ALIAS/BUCKET[/PREFIX]=KEY locations encrypted with SSE-C,
	// KEY being 32 bytes in base64 (or 64 hexadecimal digits).
	C []string
}

// IsZero reports that no encryption flag is set.
func (f EncryptionFlags) IsZero() bool { return len(f.S3) == 0 && len(f.KMS) == 0 && len(f.C) == 0 }

// Rules parses the flags; parse resolves a location (an alias must name a
// remote location with a bucket). A location given twice is a usage
// error.
func (f EncryptionFlags) Rules(parse func(string) (Target, error)) (EncryptionRules, error) {
	var out EncryptionRules
	seen := map[string]string{}
	add := func(flag, location string, sse domain.SSERequest) error {
		t, err := parse(location)
		if err != nil {
			return err
		}
		if !t.IsRemote() || t.Bucket == "" {
			return UsageError("%s %q: expected ALIAS/BUCKET[/PREFIX] of a configured alias", flag, location)
		}
		key := t.Alias + "/" + t.Bucket + "/" + t.Key
		if prev, dup := seen[key]; dup {
			return UsageError("%s %q: the location already has an encryption (%s)", flag, location, prev)
		}
		seen[key] = flag
		out = append(out, EncryptionRule{Alias: t.Alias, Bucket: t.Bucket, Prefix: t.Key, SSE: sse})
		return nil
	}
	for _, v := range splitFlagValues(f.S3) {
		if strings.Contains(v, "=") {
			return nil, UsageError("--enc-s3 %q takes no key: ALIAS/BUCKET[/PREFIX]", v)
		}
		if err := add("--enc-s3", v, domain.SSERequest{Type: domain.SSES3}); err != nil {
			return nil, err
		}
	}
	for _, v := range splitFlagValues(f.KMS) {
		location, keyID, _ := strings.Cut(v, "=")
		if err := add("--enc-kms", location, domain.SSERequest{Type: domain.SSEKMS, KMSKeyID: strings.TrimSpace(keyID)}); err != nil {
			return nil, err
		}
	}
	for _, v := range splitFlagValues(f.C) {
		location, raw, ok := strings.Cut(v, "=")
		if !ok || raw == "" {
			return nil, UsageError("--enc-c %q needs a key: ALIAS/BUCKET[/PREFIX]=BASE64KEY", v)
		}
		key, err := ParseSSECustomerKey(raw)
		if err != nil {
			return nil, err
		}
		if err := add("--enc-c", location, domain.SSERequest{Type: domain.SSEC, CustomerKey: key}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// splitFlagValues splits the comma-separated values of repeated flags.
func splitFlagValues(values []string) []string {
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

// ParseSSECustomerKey parses an SSE-C key: 32 bytes encoded in base64
// (standard or URL alphabet, padded or not) or as 64 hexadecimal digits.
func ParseSSECustomerKey(s string) (domain.SSECustomerKey, error) {
	s = strings.TrimSpace(s)
	var raw []byte
	if len(s) == 2*domain.SSECustomerKeySize {
		if b, err := hex.DecodeString(s); err == nil {
			raw = b
		}
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if raw != nil {
			break
		}
		if b, err := enc.DecodeString(s); err == nil {
			raw = b
		}
	}
	if len(raw) != domain.SSECustomerKeySize {
		return domain.SSECustomerKey{}, UsageError("invalid SSE-C key: expected %d bytes in base64 (or %d hexadecimal digits)",
			domain.SSECustomerKeySize, 2*domain.SSECustomerKeySize)
	}
	key, err := domain.NewSSECustomerKey(raw, "")
	if err != nil {
		return domain.SSECustomerKey{}, UsageError("invalid SSE-C key: %v", err)
	}
	return key, nil
}

// EncryptionName renders the encryption of a version for humans: "SSE-S3",
// "SSE-KMS (key)", "SSE-C", or "" when not encrypted.
func EncryptionName(e domain.EncryptionInfo) string {
	switch {
	case e.Type == domain.SSEKMS && e.KMSKeyID != "":
		return "SSE-KMS (" + e.KMSKeyID + ")"
	default:
		return string(e.Type)
	}
}
