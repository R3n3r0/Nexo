package admin

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/r3n3r0/nexo/internal/domain"
)

// pageLimit validates the limit of a page (0 is MaxPageLimit).
func pageLimit(p Page) (int, error) {
	switch {
	case p.Limit == 0:
		return MaxPageLimit, nil
	case p.Limit < 0 || p.Limit > MaxPageLimit:
		return 0, fmt.Errorf("%w: limit must be between 1 and %d", domain.ErrInvalidArgument, MaxPageLimit)
	}
	return p.Limit, nil
}

// encodeCursor makes the opaque cursor of a sort key.
func encodeCursor(key string) string { return base64.RawURLEncoding.EncodeToString([]byte(key)) }

// decodeCursor returns the sort key of a cursor ("" for none).
func decodeCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(raw) == 0 {
		return "", fmt.Errorf("%w: invalid cursor", domain.ErrInvalidArgument)
	}
	return string(raw), nil
}

// encodeJSONCursor makes the opaque cursor of a composite position (a key
// and a version or upload ID...).
func encodeJSONCursor(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		// The positions are plain structs and numbers: unreachable.
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeJSONCursor decodes a cursor of encodeJSONCursor into v.
func decodeJSONCursor(cursor string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err == nil {
		err = json.Unmarshal(raw, v)
	}
	if err != nil {
		return fmt.Errorf("%w: invalid cursor", domain.ErrInvalidArgument)
	}
	return nil
}

// pageOf selects the page p of items ordered by key, which must be unique
// per item: the items whose key follows the cursor, at most the limit of
// them, and the cursor of the next page. Only the items after the cursor
// are sorted, so that the later pages of a long list cost less.
func pageOf[T any](items []T, key func(T) string, p Page) (*Paged[T], error) {
	limit, err := pageLimit(p)
	if err != nil {
		return nil, err
	}
	after, err := decodeCursor(p.Cursor)
	if err != nil {
		return nil, err
	}
	rest := make([]T, 0, len(items))
	for _, it := range items {
		if after == "" || key(it) > after {
			rest = append(rest, it)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return key(rest[i]) < key(rest[j]) })
	out := &Paged[T]{Items: rest}
	if len(rest) > limit {
		out.Items = rest[:limit:limit]
		out.NextCursor = encodeCursor(key(out.Items[limit-1]))
	}
	return out, nil
}
