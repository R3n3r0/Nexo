package auth

import "sync"

// keyCacheKey identifies a derived signing key. The secret is part of the
// key so that a rotated secret never reuses a stale derivation.
type keyCacheKey struct {
	secret  string
	date    string
	region  string
	service string
}

// keyCache memoizes SigV4 signing keys: deriving one costs four HMACs and
// a key is valid for a whole day, so a busy client reuses it for every
// request. The cache is bounded; when full an arbitrary entry is evicted
// (stale days age out this way). A nil cache or a zero size disables it.
type keyCache struct {
	mu   sync.Mutex
	max  int
	keys map[keyCacheKey][]byte
}

func newKeyCache(maxEntries int) *keyCache {
	if maxEntries <= 0 {
		return nil
	}
	return &keyCache{max: maxEntries, keys: make(map[keyCacheKey][]byte, min(maxEntries, 64))}
}

// get returns the signing key of the secret for the scope. The returned
// slice is shared and must not be modified.
func (c *keyCache) get(secret string, scope credentialScope) []byte {
	if c == nil {
		return deriveSigningKey(secret, scope)
	}
	k := keyCacheKey{secret: secret, date: scope.date, region: scope.region, service: scope.service}
	c.mu.Lock()
	key, ok := c.keys[k]
	c.mu.Unlock()
	if ok {
		return key
	}
	key = deriveSigningKey(secret, scope)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.keys[k]; !exists && len(c.keys) >= c.max {
		for victim := range c.keys {
			delete(c.keys, victim)
			break
		}
	}
	c.keys[k] = key
	return key
}

// len returns the number of cached keys.
func (c *keyCache) len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.keys)
}
