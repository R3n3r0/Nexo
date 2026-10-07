package domain

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestS3Error(t *testing.T) {
	e := NewS3Error(ErrInvalidPart, "InvalidPart", "part 3 does not match").WithResource("/b/k")
	assert.True(t, errors.Is(e, ErrInvalidPart))
	assert.Equal(t, "/b/k", e.Resource)
	assert.Contains(t, e.Error(), "part 3 does not match")
	wrapped := fmt.Errorf("complete: %w", e)
	var target *S3Error
	assert.True(t, errors.As(wrapped, &target))
	assert.Equal(t, "InvalidPart", target.Code)

	assert.Equal(t, "X", (&S3Error{Code: "X"}).Error())
	assert.Equal(t, ErrConflict.Error(), (&S3Error{Err: ErrConflict}).Error())
	assert.Equal(t, "msg", (&S3Error{Message: "msg"}).Error())
}

func TestIdentityHelpers(t *testing.T) {
	a := Anonymous()
	assert.True(t, a.IsAnonymous)
	assert.Equal(t, "", a.EffectiveUser())
	sa := &Identity{AccessKey: "svc", Parent: "alice"}
	assert.Equal(t, "alice", sa.EffectiveUser())
	assert.Equal(t, "bob", (&Identity{AccessKey: "bob"}).EffectiveUser())
}
