package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/kms"
)

// Metadata keys of an encrypted version (or multipart upload). They are
// internal: never returned to clients, never copied by CopyObject.
const (
	// MetaPrefix starts every key below.
	MetaPrefix = "x-nexo-internal-sse"
	// MetaType is the encryption kind (domain.SSEType).
	MetaType = MetaPrefix
	// MetaCipher is the cipher of the data packages.
	MetaCipher = MetaPrefix + "-cipher"
	// MetaSealAlgorithm is the sealing scheme of the object key.
	MetaSealAlgorithm = MetaPrefix + "-seal-algorithm"
	// MetaSealedKey is the sealed object key (base64).
	MetaSealedKey = MetaPrefix + "-sealed-key"
	// MetaIV is the random salt of the key encryption key (base64).
	MetaIV = MetaPrefix + "-iv"
	// MetaKMSKeyID is the KMS key the data key is sealed under.
	MetaKMSKeyID = MetaPrefix + "-kms-key-id"
	// MetaKMSSealedKey is the data key sealed by the KMS (base64).
	MetaKMSSealedKey = MetaPrefix + "-kms-sealed-key"
	// MetaKMSContext is the SSE-KMS context of the client (base64 JSON).
	MetaKMSContext = MetaPrefix + "-kms-context"
	// MetaSSECKeyHMAC verifies the SSE-C key of a request (base64 HMAC
	// of the key, keyed by the salt): the key itself is never stored.
	MetaSSECKeyHMAC = MetaPrefix + "-c-key-hmac"
)

// Sealing scheme and domain separation strings.
const (
	sealAlgorithm = "nexo-sse-seal-v1"
	kekLabel      = "nexo-sse-kek-v1/"
	ssecVerify    = "nexo-sse-c-verify-v1"
	// ContextARNKey is the key of the encryption context that binds a KMS
	// data key to its object.
	ContextARNKey = "aws:s3:arn"
	objectKeySize = 32
	saltSize      = 32
)

// IsEncrypted reports whether a version (or upload) metadata describes an
// encrypted object.
func IsEncrypted(meta map[string]string) bool { return meta[MetaType] != "" }

// IsMetadataKey reports whether a metadata key belongs to the encryption.
func IsMetadataKey(key string) bool { return strings.HasPrefix(key, MetaPrefix) }

// InfoOf describes the encryption of a version from its metadata (zero
// for an unencrypted one).
func InfoOf(meta map[string]string) domain.EncryptionInfo {
	typ := domain.SSEType(meta[MetaType])
	if typ == domain.SSENone {
		return domain.EncryptionInfo{}
	}
	info := domain.EncryptionInfo{Type: typ}
	if typ == domain.SSES3 || typ == domain.SSEKMS {
		info.KMSKeyID = meta[MetaKMSKeyID]
	}
	if typ == domain.SSEKMS {
		info.Context, _ = decodeClientContext(meta[MetaKMSContext])
	}
	return info
}

// Option configures the crypto service.
type Option func(*options)

type options struct {
	cipher Cipher
}

// WithCipher forces the cipher of the data packages of new objects
// (default: DefaultCipher).
func WithCipher(c Cipher) Option {
	return func(o *options) {
		if c == CipherAESGCM || c == CipherChaCha20Poly1305 {
			o.cipher = c
		}
	}
}

// service is the default crypto service.
type service struct {
	deps Deps
	opts options
}

var _ Service = (*service)(nil)

// New returns the crypto service.
func New(deps Deps, opts ...Option) Service {
	if deps.KMS == nil {
		deps.KMS = kms.NewDisabled()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := options{cipher: DefaultCipher()}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return &service{deps: deps, opts: o}
}

// Status implements Service.
func (s *service) Status(ctx context.Context) (kms.Status, error) {
	return s.deps.KMS.Status(ctx)
}

// kmsContext is the encryption context of a KMS data key: the client's
// SSE-KMS context with the object ARN, which binds the key to its object.
func kmsContext(bucket, object string, client map[string]string) kms.Context {
	out := make(kms.Context, len(client)+1)
	maps.Copy(out, client)
	out[ContextARNKey] = "arn:aws:s3:::" + bucket + "/" + object
	return out
}

func encodeClientContext(c map[string]string) (string, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("%w: encryption context: %v", domain.ErrInvalidArgument, err)
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func decodeClientContext(v string) (map[string]string, error) {
	if v == "" {
		return nil, nil
	}
	data, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("crypto: %w: encryption context", domain.ErrDecryption)
	}
	var c map[string]string
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("crypto: %w: encryption context", domain.ErrDecryption)
	}
	return c, nil
}

// sealer returns the AEAD sealing the object key: AES-256-GCM keyed with
// HKDF-SHA256(secret, salt, label/type/bucket/object). The key is fresh
// for every seal (random salt), so the nonce is constant.
func sealer(secret, salt []byte, typ domain.SSEType, bucket, object string) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, secret, salt, kekLabel+string(typ)+"/"+bucket+"/"+object, 32)
	if err != nil {
		return nil, fmt.Errorf("crypto: derive key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: %w", err)
	}
	return cipher.NewGCM(block)
}

// ssecHMAC is the verification value of an SSE-C key.
func ssecHMAC(key, salt []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(ssecVerify))
	_, _ = mac.Write(salt)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// Seal implements Service.
func (s *service) Seal(ctx context.Context, in SealInput) (ObjectKey, error) {
	req := in.Request
	k := ObjectKey{cipher: s.opts.cipher, info: domain.EncryptionInfo{Type: req.Type}, meta: map[string]string{}}
	if _, err := rand.Read(k.key[:]); err != nil {
		return ObjectKey{}, fmt.Errorf("crypto: random key: %w", err)
	}
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return ObjectKey{}, fmt.Errorf("crypto: random salt: %w", err)
	}
	var secret []byte
	switch req.Type {
	case domain.SSEC:
		if len(req.CustomerKey.Key) != domain.SSECustomerKeySize {
			return ObjectKey{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument",
				"Requests specifying Server Side Encryption with Customer provided keys must provide a valid encryption key.")
		}
		secret = req.CustomerKey.Key
		k.meta[MetaSSECKeyHMAC] = ssecHMAC(secret, salt)
		k.info.CustomerKeyMD5 = req.CustomerKey.KeyMD5
	case domain.SSES3, domain.SSEKMS:
		keyID := req.KMSKeyID
		if req.Type == domain.SSES3 {
			keyID = ""
		}
		dek, err := s.deps.KMS.GenerateKey(ctx, keyID, kmsContext(in.Bucket, in.Object, req.Context))
		if err != nil {
			return ObjectKey{}, kmsError(err)
		}
		secret = dek.Plaintext
		k.info.KMSKeyID = dek.KeyID
		k.meta[MetaKMSKeyID] = dek.KeyID
		k.meta[MetaKMSSealedKey] = base64.StdEncoding.EncodeToString(dek.Ciphertext)
		if req.Type == domain.SSEKMS && len(req.Context) > 0 {
			enc, err := encodeClientContext(req.Context)
			if err != nil {
				return ObjectKey{}, err
			}
			k.meta[MetaKMSContext] = enc
			k.info.Context = maps.Clone(req.Context)
		}
	default:
		return ObjectKey{}, fmt.Errorf("%w: crypto: unsupported encryption %q", domain.ErrInvalidArgument, req.Type)
	}
	aead, err := sealer(secret, salt, req.Type, in.Bucket, in.Object)
	if err != nil {
		return ObjectKey{}, err
	}
	sealed := aead.Seal(nil, make([]byte, aead.NonceSize()), k.key[:], nil)
	k.meta[MetaType] = string(req.Type)
	k.meta[MetaCipher] = string(k.cipher)
	k.meta[MetaSealAlgorithm] = sealAlgorithm
	k.meta[MetaSealedKey] = base64.StdEncoding.EncodeToString(sealed)
	k.meta[MetaIV] = base64.StdEncoding.EncodeToString(salt)
	return k, nil
}

// kmsError keeps the KMS sentinels and gives the unknown key its S3 code.
func kmsError(err error) error {
	switch {
	case errors.Is(err, domain.ErrKMSKeyNotFound):
		return domain.NewS3Error(err, "KMS.NotFoundException", "Invalid keyId: "+err.Error())
	case errors.Is(err, domain.ErrKMSNotConfigured):
		return domain.NewS3Error(err, "NotImplemented", "Server side encryption specified but KMS is not configured")
	default:
		return fmt.Errorf("crypto: %w", err)
	}
}

// errSSECRequired is the read of an SSE-C object without its key.
func errSSECRequired() error {
	return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest",
		"The object was stored using a form of Server Side Encryption. The correct parameters must be provided to retrieve the object.")
}

// errSSECWrongKey is the request of an SSE-C object with another key.
func errSSECWrongKey() error {
	return domain.NewS3Error(domain.ErrAccessDenied, "AccessDenied",
		"The SSE-C key provided does not match the key the object was encrypted with.")
}

// errNotSSEC is an SSE-C key sent for an object that is not SSE-C.
func errNotSSEC() error {
	return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest",
		"The encryption parameters are not applicable to this object.")
}

// Unseal implements Service.
func (s *service) Unseal(ctx context.Context, in UnsealInput) (ObjectKey, error) {
	meta := in.Metadata
	typ := domain.SSEType(meta[MetaType])
	if typ == domain.SSENone {
		return ObjectKey{}, fmt.Errorf("%w: crypto: the object is not encrypted", domain.ErrInvalidArgument)
	}
	if typ != domain.SSEC && !in.CustomerKey.IsZero() {
		return ObjectKey{}, errNotSSEC()
	}
	if alg := meta[MetaSealAlgorithm]; alg != sealAlgorithm {
		return ObjectKey{}, fmt.Errorf("crypto: %w: unknown sealing scheme %q", domain.ErrDecryption, alg)
	}
	salt, err := base64.StdEncoding.DecodeString(meta[MetaIV])
	if err != nil || len(salt) != saltSize {
		return ObjectKey{}, fmt.Errorf("crypto: %w: malformed salt", domain.ErrDecryption)
	}
	sealed, err := base64.StdEncoding.DecodeString(meta[MetaSealedKey])
	if err != nil {
		return ObjectKey{}, fmt.Errorf("crypto: %w: malformed sealed key", domain.ErrDecryption)
	}
	k := ObjectKey{cipher: Cipher(meta[MetaCipher]), info: InfoOf(meta), meta: map[string]string{}}
	for key, v := range meta {
		if IsMetadataKey(key) {
			k.meta[key] = v
		}
	}
	var secret []byte
	switch typ {
	case domain.SSEC:
		if in.CustomerKey.IsZero() {
			return ObjectKey{}, errSSECRequired()
		}
		want, err := base64.StdEncoding.DecodeString(meta[MetaSSECKeyHMAC])
		got, _ := base64.StdEncoding.DecodeString(ssecHMAC(in.CustomerKey.Key, salt))
		if err != nil || !hmac.Equal(want, got) {
			return ObjectKey{}, errSSECWrongKey()
		}
		secret = in.CustomerKey.Key
		k.info.CustomerKeyMD5 = in.CustomerKey.KeyMD5
	case domain.SSES3, domain.SSEKMS:
		ciphertext, err := base64.StdEncoding.DecodeString(meta[MetaKMSSealedKey])
		if err != nil {
			return ObjectKey{}, fmt.Errorf("crypto: %w: malformed data key", domain.ErrDecryption)
		}
		client, err := decodeClientContext(meta[MetaKMSContext])
		if err != nil {
			return ObjectKey{}, err
		}
		if secret, err = s.deps.KMS.Decrypt(ctx, meta[MetaKMSKeyID], ciphertext, kmsContext(in.Bucket, in.Object, client)); err != nil {
			return ObjectKey{}, fmt.Errorf("crypto: unseal the data key: %w", err)
		}
	default:
		return ObjectKey{}, fmt.Errorf("crypto: %w: unknown encryption %q", domain.ErrDecryption, typ)
	}
	aead, err := sealer(secret, salt, typ, in.Bucket, in.Object)
	if err != nil {
		return ObjectKey{}, err
	}
	key, err := aead.Open(nil, make([]byte, aead.NonceSize()), sealed, nil)
	if err != nil || len(key) != objectKeySize {
		if typ == domain.SSEC {
			return ObjectKey{}, errSSECWrongKey()
		}
		return ObjectKey{}, fmt.Errorf("crypto: %w: the object key does not authenticate", domain.ErrDecryption)
	}
	copy(k.key[:], key)
	if _, err := newAEAD(k.cipher, k.key[:]); err != nil {
		return ObjectKey{}, err
	}
	return k, nil
}

// Reseal implements Service.
func (s *service) Reseal(ctx context.Context, in UnsealInput) (map[string]string, error) {
	meta := in.Metadata
	typ := domain.SSEType(meta[MetaType])
	if typ != domain.SSES3 && typ != domain.SSEKMS {
		return nil, fmt.Errorf("%w: crypto: only SSE-S3 and SSE-KMS objects are re-sealed (got %q)", domain.ErrInvalidArgument, typ)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(meta[MetaKMSSealedKey])
	if err != nil {
		return nil, fmt.Errorf("crypto: %w: malformed data key", domain.ErrDecryption)
	}
	client, err := decodeClientContext(meta[MetaKMSContext])
	if err != nil {
		return nil, err
	}
	resealed, err := s.deps.KMS.ReEncrypt(ctx, meta[MetaKMSKeyID], ciphertext, kmsContext(in.Bucket, in.Object, client))
	if err != nil {
		return nil, fmt.Errorf("crypto: re-seal the data key: %w", err)
	}
	out := maps.Clone(meta)
	out[MetaKMSSealedKey] = base64.StdEncoding.EncodeToString(resealed)
	return out, nil
}
