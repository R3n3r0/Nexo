package accessrule

import (
	"context"
	"fmt"
	"strings"
)

// staticSecrets is a SecretStore of fixed values.
type staticSecrets struct {
	values map[string]string
}

var _ SecretStore = (*staticSecrets)(nil)

// NewStaticSecrets returns a SecretStore of fixed values, such as the
// NEXO_SECRET_<NAME> variables of the static configuration. Names match
// ignoring the letter case, with '-', '.' and '_' equivalent:
// ${secret:proxy-token} is NEXO_SECRET_PROXY_TOKEN.
func NewStaticSecrets(values map[string]string) SecretStore {
	s := &staticSecrets{values: make(map[string]string, len(values))}
	for k, v := range values {
		s.values[secretKey(k)] = v
	}
	return s
}

// Secret implements SecretStore.
func (s *staticSecrets) Secret(_ context.Context, name string) (string, error) {
	v, ok := s.values[secretKey(name)]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrSecretNotFound, name)
	}
	return v, nil
}

// secretKey is the canonical form of a secret name.
func secretKey(name string) string {
	return strings.NewReplacer("-", "_", ".", "_").Replace(strings.ToLower(strings.TrimSpace(name)))
}
