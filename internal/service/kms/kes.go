package kms

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
)

// KES API (v1) used by the client:
//
//	GET  /v1/status                     server status
//	POST /v1/key/create/<name>          create a key
//	GET  /v1/key/list/<prefix>          {"names": [...], "continue_at": "..."}
//	POST /v1/key/generate/<name>        {"context"} -> {"plaintext", "ciphertext"}
//	POST /v1/key/decrypt/<name>         {"ciphertext", "context"} -> {"plaintext"}
//	POST /v1/key/encrypt/<name>         {"plaintext", "context"} -> {"ciphertext"}
//
// Binary fields travel base64-encoded (JSON []byte); errors are
// {"message": "..."} with an HTTP status: 404 for an unknown key, 400 or 409
// for an existing one.
const (
	kesStatusPath   = "/v1/status"
	kesCreatePath   = "/v1/key/create/"
	kesListPath     = "/v1/key/list/"
	kesGeneratePath = "/v1/key/generate/"
	kesDecryptPath  = "/v1/key/decrypt/"
	kesEncryptPath  = "/v1/key/encrypt/"
	// maxKESResponse bounds the body read from a KES server.
	maxKESResponse = 4 << 20
	// maxKESListPages bounds the pages of a key listing.
	maxKESListPages = 1000
)

// kes is the client of an external KMS speaking the KES API.
type kes struct {
	deps       KESDeps
	endpoints  []string
	defaultKey string
}

var _ Service = (*kes)(nil)

// NewKES returns the client of the KES servers of WithEndpoints (tried in
// order: a server that cannot be reached is skipped), whose default key
// is WithDefaultKey.
func NewKES(deps KESDeps, opts ...Option) (Service, error) {
	o := newOptions(opts)
	if deps.HTTP == nil {
		return nil, fmt.Errorf("%w: kes: the HTTP client is required", domain.ErrInvalidArgument)
	}
	if len(o.endpoints) == 0 {
		return nil, fmt.Errorf("%w: kes: at least one endpoint is required (NEXO_KMS_ENDPOINT)", domain.ErrInvalidArgument)
	}
	for _, e := range o.endpoints {
		if u, err := url.Parse(e); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, fmt.Errorf("%w: kes: endpoint %q must be an http(s) URL", domain.ErrInvalidArgument, e)
		}
	}
	if err := validKeyName(o.defaultKey); err != nil {
		return nil, fmt.Errorf("kes: default key (NEXO_KMS_KEY_NAME): %w", err)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &kes{deps: deps, endpoints: o.endpoints, defaultKey: o.defaultKey}, nil
}

// NewKESClient returns the HTTP client of a KES server: mutual TLS with
// the client certificate certFile/keyFile (both or none), the server
// verified with the CA certificates of caPath (a PEM file, or a directory
// of them; the system roots when empty).
func NewKESClient(certFile, keyFile, caPath string) (*http.Client, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case certFile != "" && keyFile != "":
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("kes: client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	case certFile != "" || keyFile != "":
		return nil, fmt.Errorf("%w: kes: the client certificate and its key go together", domain.ErrInvalidArgument)
	}
	if caPath != "" {
		pool, err := loadCAs(caPath)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = cfg
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}, nil
}

// loadCAs reads the PEM certificates of a file or of every file of a
// directory.
func loadCAs(path string) (*x509.CertPool, error) {
	files := []string{path}
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, fmt.Errorf("kes: CA directory: %w", err)
		}
		files = files[:0]
		for _, e := range entries {
			if !e.IsDir() {
				files = append(files, filepath.Join(path, e.Name()))
			}
		}
	}
	pool := x509.NewCertPool()
	found := false
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("kes: CA certificate: %w", err)
		}
		if pool.AppendCertsFromPEM(data) {
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: kes: no CA certificate in %s", domain.ErrInvalidArgument, path)
	}
	return pool, nil
}

// kesError is the error body of a KES server.
type kesError struct {
	Message string `json:"message"`
}

// call sends a request to the first server that answers and decodes the
// JSON answer into out (when not nil). A server that cannot be reached is
// skipped; an HTTP error is mapped to the domain sentinels.
func (k *kes) call(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return fmt.Errorf("kes: encode request: %w", err)
		}
	}
	var lastErr error
	for _, endpoint := range k.endpoints {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint+path, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("kes: %w", err)
		}
		if in != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := k.deps.HTTP.Do(req)
		if err != nil {
			lastErr = err
			k.deps.Logger.DebugContext(ctx, "kes: server unreachable", "endpoint", endpoint, "err", err)
			continue
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, maxKESResponse))
		_ = resp.Body.Close()
		if rerr != nil {
			lastErr = rerr
			continue
		}
		if resp.StatusCode/100 != 2 {
			return kesStatusError(resp.StatusCode, data)
		}
		if out == nil {
			return nil
		}
		if f, ok := out.(func([]byte) error); ok {
			return f(data)
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("kes: %w: malformed answer: %v", domain.ErrKMSUnavailable, err)
		}
		return nil
	}
	return fmt.Errorf("kes: %w: %v", domain.ErrKMSUnavailable, lastErr)
}

// kesStatusError maps an HTTP error of a KES server.
func kesStatusError(status int, body []byte) error {
	var e kesError
	_ = json.Unmarshal(body, &e)
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		msg = http.StatusText(status)
	}
	lower := strings.ToLower(msg)
	switch {
	case status == http.StatusNotFound:
		return fmt.Errorf("kes: %s: %w", msg, domain.ErrKMSKeyNotFound)
	case status == http.StatusConflict || strings.Contains(lower, "already exists"):
		return fmt.Errorf("kes: %s: %w", msg, domain.ErrKMSKeyExists)
	case status == http.StatusBadRequest && (strings.Contains(lower, "not authentic") || strings.Contains(lower, "decrypt")):
		return fmt.Errorf("kes: %s: %w", msg, domain.ErrDecryption)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("kes: %w: access denied by the KMS: %s", domain.ErrKMSUnavailable, msg)
	default:
		return fmt.Errorf("kes: %w: %d %s", domain.ErrKMSUnavailable, status, msg)
	}
}

// keyID resolves the default key and validates the name.
func (k *kes) keyID(keyID string) (string, error) {
	if keyID == "" {
		return k.defaultKey, nil
	}
	if err := validKeyName(keyID); err != nil {
		return "", err
	}
	return keyID, nil
}

// Status implements Service. An unreachable server is reported in the
// status, not as an error.
func (k *kes) Status(ctx context.Context) (Status, error) {
	st := Status{Configured: true, Backend: BackendKES, Endpoints: append([]string(nil), k.endpoints...), DefaultKey: k.defaultKey}
	var out struct {
		Version string `json:"version"`
	}
	if err := k.call(ctx, http.MethodGet, kesStatusPath, nil, &out); err != nil {
		st.Error = err.Error()
		return st, nil
	}
	st.Online, st.Version = true, out.Version
	return st, nil
}

// DefaultKeyID implements Service.
func (k *kes) DefaultKeyID() string { return k.defaultKey }

// CreateKey implements Service.
func (k *kes) CreateKey(ctx context.Context, keyID string) error {
	if err := validKeyName(keyID); err != nil {
		return err
	}
	return k.call(ctx, http.MethodPost, kesCreatePath+url.PathEscape(keyID), nil, nil)
}

// kesList is a page of a key listing.
type kesList struct {
	Names      []string `json:"names"`
	ContinueAt string   `json:"continue_at"`
}

// ListKeys implements Service. It accepts the paged listing of the
// current KES API and the stream of {"name"} objects of the older one.
func (k *kes) ListKeys(ctx context.Context, prefix string) ([]KeyInfo, error) {
	var names []string
	cursor := prefix
	for page := 0; page < maxKESListPages; page++ {
		var res kesList
		decode := func(data []byte) error {
			trimmed := bytes.TrimSpace(data)
			if err := json.Unmarshal(trimmed, &res); err == nil && (res.Names != nil || res.ContinueAt != "") {
				return nil
			}
			res = kesList{}
			sc := bufio.NewScanner(bytes.NewReader(trimmed))
			for sc.Scan() {
				var item struct {
					Name string `json:"name"`
				}
				if line := bytes.TrimSpace(sc.Bytes()); len(line) > 0 {
					if err := json.Unmarshal(line, &item); err != nil {
						return fmt.Errorf("kes: %w: malformed key listing", domain.ErrKMSUnavailable)
					}
					res.Names = append(res.Names, item.Name)
				}
			}
			return nil
		}
		if err := k.call(ctx, http.MethodGet, kesListPath+url.PathEscape(cursor), nil, decode); err != nil {
			return nil, err
		}
		names = append(names, res.Names...)
		if res.ContinueAt == "" || res.ContinueAt == cursor {
			break
		}
		cursor = res.ContinueAt
	}
	seen := map[string]bool{}
	out := make([]KeyInfo, 0, len(names))
	for _, n := range names {
		if n == "" || seen[n] || !strings.HasPrefix(n, prefix) {
			continue
		}
		seen[n] = true
		out = append(out, KeyInfo{Name: n, Default: n == k.defaultKey})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// GenerateKey implements Service.
func (k *kes) GenerateKey(ctx context.Context, keyID string, kctx Context) (DEK, error) {
	id, err := k.keyID(keyID)
	if err != nil {
		return DEK{}, err
	}
	aad, err := encodeContext(kctx)
	if err != nil {
		return DEK{}, err
	}
	var out struct {
		Plaintext  []byte `json:"plaintext"`
		Ciphertext []byte `json:"ciphertext"`
	}
	if err := k.call(ctx, http.MethodPost, kesGeneratePath+url.PathEscape(id), map[string][]byte{"context": aad}, &out); err != nil {
		return DEK{}, err
	}
	if len(out.Plaintext) != dekSize || len(out.Ciphertext) == 0 {
		return DEK{}, fmt.Errorf("kes: %w: generated a key of %d bytes", domain.ErrKMSUnavailable, len(out.Plaintext))
	}
	return DEK{KeyID: id, Plaintext: out.Plaintext, Ciphertext: out.Ciphertext}, nil
}

// Decrypt implements Service.
func (k *kes) Decrypt(ctx context.Context, keyID string, ciphertext []byte, kctx Context) ([]byte, error) {
	id, err := k.keyID(keyID)
	if err != nil {
		return nil, err
	}
	aad, err := encodeContext(kctx)
	if err != nil {
		return nil, err
	}
	var out struct {
		Plaintext []byte `json:"plaintext"`
	}
	in := map[string][]byte{"ciphertext": ciphertext, "context": aad}
	if err := k.call(ctx, http.MethodPost, kesDecryptPath+url.PathEscape(id), in, &out); err != nil {
		return nil, err
	}
	if len(out.Plaintext) == 0 {
		return nil, fmt.Errorf("kes: %w: empty plaintext", domain.ErrDecryption)
	}
	return out.Plaintext, nil
}

// ReEncrypt implements Service: the data key is unsealed and sealed again
// with the current version of the key on the KES server.
func (k *kes) ReEncrypt(ctx context.Context, keyID string, ciphertext []byte, kctx Context) ([]byte, error) {
	plaintext, err := k.Decrypt(ctx, keyID, ciphertext, kctx)
	if err != nil {
		return nil, err
	}
	id, _ := k.keyID(keyID)
	aad, err := encodeContext(kctx)
	if err != nil {
		return nil, err
	}
	var out struct {
		Ciphertext []byte `json:"ciphertext"`
	}
	in := map[string][]byte{"plaintext": plaintext, "context": aad}
	if err := k.call(ctx, http.MethodPost, kesEncryptPath+url.PathEscape(id), in, &out); err != nil {
		return nil, err
	}
	if len(out.Ciphertext) == 0 {
		return nil, fmt.Errorf("kes: %w: empty ciphertext", domain.ErrKMSUnavailable)
	}
	return out.Ciphertext, nil
}
