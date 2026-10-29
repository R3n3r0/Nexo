package playback

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
)

// The advanced playback (domain.MediaPlaybackAdvanced): the player of Nexo
// opens a session with a playback token and an ephemeral ECDH P-256 key;
// the server answers with its own ephemeral key and a session ticket.
// Both sides derive the wrapping key of the session from the shared
// secret (HKDF-SHA256), so the keys of the segments travel wrapped with
// it (AES-128-GCM) and never in clear; the player unwraps them into
// WebCrypto keys it cannot read back. The segments of a session are
// encrypted with keys of that session alone (media.Source.Session).
//
// The ticket is signed like the tokens and carries the wrapping key sealed
// with a key of the deployment: any node verifies it and wraps the keys of
// the session, without state shared between the nodes.

// Labels of the keys derived for the sessions.
const (
	ticketKeyLabel = "nexo/media/session-ticket-v1"
	sealKeyLabel   = "nexo/media/session-seal-v1"
	wrapInfo       = "nexo/media/session-wrap-v1\x00"
)

// ticketPrefix starts every session ticket (a token never does: its
// payload is base64 of a JSON object, "ey...").
const ticketPrefix = "s1."

// Lengths of the parts of a session.
const (
	sessionIDLen = 16
	saltLen      = 16
	wrapKeyLen   = 16
)

// Session is a playback session of the player of Nexo (OpenSession).
type Session struct {
	// Ticket authorizes the requests of the session (the st query
	// parameter of the media routes).
	Ticket string
	// Claims are those of the token, with the id of the session.
	Claims Claims
	// ServerKey is the ephemeral ECDH P-256 public key of the server
	// (uncompressed point) and Salt the salt of the HKDF: with them the
	// player derives the wrapping key of the session.
	ServerKey []byte
	Salt      []byte
}

// ticketPayload is the payload of a session ticket: the claims of the
// token, the id of the session and its wrapping key, sealed.
type ticketPayload struct {
	Claims
	Sealed string `json:"wk"`
}

// OpenSession opens a session of the player for the claims of a verified
// token: clientKey is the ephemeral ECDH P-256 public key of the player
// (65 bytes, uncompressed). domain.ErrInvalidArgument for a malformed
// key.
func (s *service) OpenSession(_ context.Context, c *Claims, clientKey []byte) (*Session, error) {
	if c == nil {
		return nil, fmt.Errorf("%w: a session needs a playback token", domain.ErrAccessDenied)
	}
	peer, err := ecdh.P256().NewPublicKey(clientKey)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid ECDH P-256 public key", domain.ErrInvalidArgument)
	}
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("playback: session key: %w", err)
	}
	secret, err := priv.ECDH(peer)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid ECDH P-256 public key", domain.ErrInvalidArgument)
	}
	id := make([]byte, sessionIDLen)
	salt := make([]byte, saltLen)
	if _, err := rand.Read(id); err != nil {
		return nil, fmt.Errorf("playback: session id: %w", err)
	}
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("playback: session salt: %w", err)
	}
	sc := *c
	sc.Session = hex.EncodeToString(id)
	wrap, err := hkdf.Key(sha256.New, secret, salt, wrapInfo+sc.Session, wrapKeyLen)
	if err != nil {
		return nil, fmt.Errorf("playback: session wrapping key: %w", err)
	}
	sealed, err := s.seal(wrap, sc.Session)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(ticketPayload{Claims: sc, Sealed: base64.RawURLEncoding.EncodeToString(sealed)})
	if err != nil {
		return nil, fmt.Errorf("playback: encode ticket: %w", err)
	}
	enc := base64.RawURLEncoding.EncodeToString(payload)
	sc.wrap = wrap
	return &Session{
		Ticket: ticketPrefix + enc + "." + base64.RawURLEncoding.EncodeToString(s.mac(ticketKeyLabel, enc)),
		Claims: sc, ServerKey: priv.PublicKey().Bytes(), Salt: salt,
	}, nil
}

// IsTicket reports whether a value is a session ticket rather than a
// playback token.
func IsTicket(v string) bool { return strings.HasPrefix(v, ticketPrefix) }

// VerifyTicket implements Service.
func (s *service) VerifyTicket(ctx context.Context, ticket, bucket, key, versionID string) (*Claims, error) {
	invalid := fmt.Errorf("%w: invalid playback session", domain.ErrAccessDenied)
	body, ok := strings.CutPrefix(ticket, ticketPrefix)
	if !ok || len(ticket) > 8192 {
		return nil, invalid
	}
	enc, sig, ok := strings.Cut(body, ".")
	if !ok {
		return nil, invalid
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.mac(ticketKeyLabel, enc)) {
		return nil, invalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return nil, invalid
	}
	var p ticketPayload
	if err := json.Unmarshal(raw, &p); err != nil || p.Version != tokenVersion || p.SessionID == "" || p.Session == "" {
		return nil, invalid
	}
	sealed, err := base64.RawURLEncoding.DecodeString(p.Sealed)
	if err != nil {
		return nil, invalid
	}
	wrap, err := s.open(sealed, p.Session)
	if err != nil {
		return nil, invalid
	}
	c, err := s.check(ctx, &p.Claims, bucket, key, versionID)
	if err != nil {
		return nil, err
	}
	c.wrap = wrap
	return c, nil
}

// WrapKey implements Service.
func (s *service) WrapKey(c *Claims, key []byte, aad string) ([]byte, error) {
	if c == nil || len(c.wrap) == 0 {
		return nil, fmt.Errorf("%w: no playback session", domain.ErrAccessDenied)
	}
	return gcmSeal(c.wrap, key, []byte(aad))
}

// mac signs data with the key of a label, derived from the protection key.
func (s *service) mac(label, data string) []byte {
	k := hmac.New(sha256.New, s.opts.key)
	k.Write([]byte(label))
	mac := hmac.New(sha256.New, k.Sum(nil))
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

// sealKey is the key sealing the wrapping keys in the tickets.
func (s *service) sealKey() []byte {
	k := hmac.New(sha256.New, s.opts.key)
	k.Write([]byte(sealKeyLabel))
	return k.Sum(nil)[:16]
}

// seal encrypts the wrapping key of a session for its ticket, bound to
// the id of the session.
func (s *service) seal(wrap []byte, session string) ([]byte, error) {
	return gcmSeal(s.sealKey(), wrap, []byte(session))
}

func (s *service) open(sealed []byte, session string) ([]byte, error) {
	return gcmOpen(s.sealKey(), sealed, []byte(session))
}

// gcmSeal encrypts with AES-GCM under a random nonce: nonce || ciphertext
// || tag.
func gcmSeal(key, plaintext, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("playback: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("playback: cipher: %w", err)
	}
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plaintext)+aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("playback: nonce: %w", err)
	}
	return aead.Seal(nonce, nonce, plaintext, aad), nil
}

func gcmOpen(key, sealed, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(sealed) < aead.NonceSize()+aead.Overhead() {
		return nil, fmt.Errorf("%w: sealed data too short", domain.ErrAccessDenied)
	}
	return aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], aad)
}
