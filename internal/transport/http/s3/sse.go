package s3

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// Server-side encryption headers (docs/02 S-OBJ-13).
const (
	headerSSEContext            = "x-amz-server-side-encryption-context"
	headerSSECustomerKeyMD5     = "x-amz-server-side-encryption-customer-key-md5"
	headerCopySSECustomerPrefix = "x-amz-copy-source-server-side-encryption-customer-"
	headerSSECustomerPrefix     = "x-amz-server-side-encryption-customer-"
	// kmsARNPrefix starts the ARN form of a KMS key id.
	kmsARNPrefix = "arn:aws:kms:"
	// maxSSEContext bounds the encryption context of SSE-KMS.
	maxSSEContext = 4 << 10
)

// errSSEInsecure is an SSE-C request over plain HTTP.
func errSSEInsecure() error {
	return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest",
		"Requests specifying Server Side Encryption with Customer provided keys must be made over a secure connection.")
}

// kmsKeyID normalizes a KMS key id: the ARN form
// (arn:aws:kms:<region>:<account>:key/<id>) names the key <id>.
func kmsKeyID(v string) string {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, kmsARNPrefix) {
		return v
	}
	v = strings.TrimPrefix(v, kmsARNPrefix)
	if i := strings.LastIndex(v, "key/"); i >= 0 {
		return v[i+len("key/"):]
	}
	return v
}

// customerKeyFromHeaders reads the SSE-C key of a request from the
// headers starting with prefix (x-amz-server-side-encryption-customer- for
// the object, x-amz-copy-source-server-side-encryption-customer- for the
// source of a copy): zero when none of them is set. SSE-C needs a secure
// connection (TLS, or a trusted proxy that terminated it) unless the
// routes allow it in clear (tests).
func customerKeyFromHeaders(r *http.Request, prefix string, allowInsecure bool) (domain.SSECustomerKey, error) {
	h := r.Header
	algo, key, keyMD5 := h.Get(prefix+"algorithm"), h.Get(prefix+"key"), h.Get(prefix+"key-MD5")
	if algo == "" && key == "" && keyMD5 == "" {
		return domain.SSECustomerKey{}, nil
	}
	if !allowInsecure && !middleware.ClientOf(r).Secure {
		return domain.SSECustomerKey{}, errSSEInsecure()
	}
	if algo == "" || key == "" {
		return domain.SSECustomerKey{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument",
			"Requests specifying Server Side Encryption with Customer provided keys must provide an appropriate secret key and algorithm.")
	}
	if keyMD5 == "" {
		return domain.SSECustomerKey{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument",
			"Requests specifying Server Side Encryption with Customer provided keys must provide the client calculated MD5 of the secret key.")
	}
	if algo != domain.SSEAlgorithmAES256 {
		return domain.SSECustomerKey{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument",
			"The encryption algorithm specified is not valid: only AES256 is supported for customer provided keys.")
	}
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return domain.SSECustomerKey{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument",
			"The secret key was invalid for the specified algorithm.")
	}
	return domain.NewSSECustomerKey(raw, keyMD5)
}

// sseFromHeaders reads the encryption a write asks for: SSE-S3 or SSE-KMS
// (x-amz-server-side-encryption, -aws-kms-key-id, -context) or SSE-C
// (x-amz-server-side-encryption-customer-*), never both. The zero request
// lets the default encryption of the bucket apply.
func sseFromHeaders(r *http.Request, allowInsecure bool) (domain.SSERequest, error) {
	h := r.Header
	ckey, err := customerKeyFromHeaders(r, headerSSECustomerPrefix, allowInsecure)
	if err != nil {
		return domain.SSERequest{}, err
	}
	algo := strings.TrimSpace(h.Get(httpx.HeaderAmzSSE))
	keyID, ctxHeader := h.Get(httpx.HeaderAmzSSEKMSKeyID), h.Get(headerSSEContext)
	if !ckey.IsZero() {
		if algo != "" || keyID != "" || ctxHeader != "" {
			return domain.SSERequest{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument",
				"Server Side Encryption with Customer provided key is incompatible with the encryption method specified.")
		}
		return domain.SSERequest{Type: domain.SSEC, CustomerKey: ckey}, nil
	}
	switch algo {
	case "":
		if keyID != "" || ctxHeader != "" {
			return domain.SSERequest{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument",
				"Specifying a KMS key or an encryption context requires x-amz-server-side-encryption: aws:kms.")
		}
		return domain.SSERequest{}, nil
	case domain.SSEAlgorithmAES256:
		if keyID != "" || ctxHeader != "" {
			return domain.SSERequest{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument",
				"Server Side Encryption with AWS KMS managed key requires HTTP header x-amz-server-side-encryption : aws:kms")
		}
		return domain.SSERequest{Type: domain.SSES3}, nil
	case domain.SSEAlgorithmKMS, "aws:kms:dsse":
		req := domain.SSERequest{Type: domain.SSEKMS, KMSKeyID: kmsKeyID(keyID)}
		if ctxHeader != "" {
			if req.Context, err = parseSSEContext(ctxHeader); err != nil {
				return domain.SSERequest{}, err
			}
		}
		return req, nil
	default:
		return domain.SSERequest{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument",
			"The encryption method specified is not supported: "+algo)
	}
}

// parseSSEContext decodes x-amz-server-side-encryption-context: base64 of
// a JSON object of strings.
func parseSSEContext(v string) (map[string]string, error) {
	invalid := domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument",
		"The x-amz-server-side-encryption-context header must be the base64 encoding of a JSON object of strings.")
	if len(v) > maxSSEContext {
		return nil, invalid
	}
	data, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, invalid
	}
	var out map[string]string
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, invalid
	}
	return out, nil
}

// writeSSEHeaders reports the encryption of an object in a response.
func writeSSEHeaders(w http.ResponseWriter, info domain.EncryptionInfo) {
	h := w.Header()
	switch info.Type {
	case domain.SSES3:
		h.Set(httpx.HeaderAmzSSE, domain.SSEAlgorithmAES256)
	case domain.SSEKMS:
		h.Set(httpx.HeaderAmzSSE, domain.SSEAlgorithmKMS)
		if info.KMSKeyID != "" {
			h.Set(httpx.HeaderAmzSSEKMSKeyID, info.KMSKeyID)
		}
		if len(info.Context) > 0 {
			if data, err := json.Marshal(info.Context); err == nil {
				h.Set(headerSSEContext, base64.StdEncoding.EncodeToString(data))
			}
		}
	case domain.SSEC:
		h.Set(httpx.HeaderAmzSSECustomerAlgorithm, domain.SSEAlgorithmAES256)
		if info.CustomerKeyMD5 != "" {
			h.Set(headerSSECustomerKeyMD5, info.CustomerKeyMD5)
		}
	}
}
