package bucket

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
)

// emptyAccessRules is the document of a bucket without access rules.
const emptyAccessRules = "[]"

// AccessRulesETag returns the ETag of an access rules document: the first
// 128 bits of its SHA-256, in hex. An empty document is "[]".
func AccessRulesETag(doc []byte) string {
	if len(bytes.TrimSpace(doc)) == 0 {
		doc = []byte(emptyAccessRules)
	}
	sum := sha256.Sum256(doc)
	return hex.EncodeToString(sum[:16])
}

// currentAccessRules returns the access rules of the metadata, the empty
// document when none.
func currentAccessRules(m *Metadata) AccessRules {
	if m.AccessRules == nil || len(m.AccessRules.Document) == 0 {
		out := AccessRules{Document: json.RawMessage(emptyAccessRules), ETag: AccessRulesETag(nil)}
		if m.AccessRules != nil {
			out.UpdatedAt, out.UpdatedBy = m.AccessRules.UpdatedAt, m.AccessRules.UpdatedBy
		}
		return out
	}
	return *m.AccessRules.clone()
}

// GetAccessRules implements Service.
func (s *service) GetAccessRules(ctx context.Context, name string) (AccessRules, error) {
	m, err := s.load(ctx, name)
	if err != nil {
		return AccessRules{}, err
	}
	return currentAccessRules(m), nil
}

// SetAccessRules implements Service.
func (s *service) SetAccessRules(ctx context.Context, name string, in SetAccessRulesInput) (AccessRulesChange, error) {
	doc := bytes.TrimSpace(in.Document)
	if len(doc) == 0 {
		doc = []byte(emptyAccessRules)
	}
	if len(doc) > s.opts.maxDocumentSize || !json.Valid(doc) {
		return AccessRulesChange{}, fmt.Errorf("%w: the access rules must be a JSON document of at most %d bytes", domain.ErrInvalidArgument, s.opts.maxDocumentSize)
	}
	var change AccessRulesChange
	err := s.update(ctx, name, func(m *Metadata) error {
		change.Previous = currentAccessRules(m)
		if in.IfMatch != "" && change.Previous.ETag != in.IfMatch {
			return fmt.Errorf("%w: the access rules of %s changed (ETag %s, not %s)", domain.ErrPreconditionFailed, name, change.Previous.ETag, in.IfMatch)
		}
		stored := AccessRules{
			Document:  append(json.RawMessage(nil), doc...),
			ETag:      AccessRulesETag(doc),
			UpdatedAt: s.deps.Clock.Now().UTC(),
			UpdatedBy: in.UpdatedBy,
		}
		m.AccessRules = stored.clone()
		change.Current = stored
		return nil
	})
	if err != nil {
		return AccessRulesChange{}, err
	}
	return change, nil
}

// GetBlockPublicAccess implements Service.
func (s *service) GetBlockPublicAccess(ctx context.Context, name string) (bool, error) {
	m, err := s.load(ctx, name)
	if err != nil {
		return false, err
	}
	return m.BlockPublicAccess, nil
}

// SetBlockPublicAccess implements Service.
func (s *service) SetBlockPublicAccess(ctx context.Context, name string, on bool) error {
	return s.update(ctx, name, func(m *Metadata) error {
		m.BlockPublicAccess = on
		return nil
	})
}

// clone returns a deep copy of the stored rules.
func (r *AccessRules) clone() *AccessRules {
	if r == nil {
		return nil
	}
	c := *r
	c.Document = cloneBytes(r.Document)
	return &c
}
