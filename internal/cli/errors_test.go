package cli_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
)

// covers: TCL-U-04
func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code int
		cls  string
	}{
		{"nil", nil, cli.ExitOK, ""},
		{"generic", errors.New("boom"), cli.ExitError, "Error"},
		{"usage", cli.UsageError("bad flag %s", "x"), cli.ExitUsage, "UsageError"},
		{"wrapped usage", fmt.Errorf("cp: %w", cli.UsageError("x")), cli.ExitUsage, "UsageError"},
		{"bucket", fmt.Errorf("x: %w", domain.ErrBucketNotFound), cli.ExitNotFound, "NotFound"},
		{"object", domain.ErrObjectNotFound, cli.ExitNotFound, "NotFound"},
		{"version", domain.ErrNoSuchVersion, cli.ExitNotFound, "NotFound"},
		{"alias", &cli.AliasNotFoundError{Name: "x"}, cli.ExitNotFound, "NotFound"},
		{"local file", fs.ErrNotExist, cli.ExitNotFound, "NotFound"},
		{"denied", domain.ErrAccessDenied, cli.ExitAccess, "AccessDenied"},
		{"bad key", domain.ErrInvalidAccessKey, cli.ExitAccess, "AccessDenied"},
		{"signature", domain.ErrSignatureMismatch, cli.ExitAccess, "AccessDenied"},
		{"expired", domain.ErrRequestExpired, cli.ExitAccess, "AccessDenied"},
		{"local permission", fs.ErrPermission, cli.ExitAccess, "AccessDenied"},
		{"interrupted", context.Canceled, cli.ExitInterrupted, "Interrupted"},
		{"aborted upload", &cli.AbortedUploadError{Bucket: "b", Key: "k", UploadID: "u", Err: context.Canceled}, cli.ExitInterrupted, "Interrupted"},
		{"invalid argument (server)", domain.ErrInvalidBucketName, cli.ExitError, "Error"},
		{"object locked", fmt.Errorf("rb: %w", domain.ErrObjectLocked), cli.ExitAccess, "AccessDenied"},
		{"missing configuration", domain.ErrNotFound, cli.ExitNotFound, "NotFound"},
		{"kms key", domain.ErrKMSKeyNotFound, cli.ExitError, "Error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.code, cli.ExitCode(tc.err))
			assert.Equal(t, tc.cls, cli.ErrorCode(tc.err))
		})
	}
}

func TestErrorMessages(t *testing.T) {
	assert.Equal(t, "bad flag x", cli.UsageError("bad flag %s", "x").Error(), "usage errors carry only their text")
	aborted := &cli.AbortedUploadError{Bucket: "b", Key: "k", UploadID: "u1", Err: errors.New("disk full")}
	assert.Equal(t, "b/k: multipart upload u1 aborted: disk full", aborted.Error())
	aborted.AbortErr = errors.New("timeout")
	assert.Contains(t, aborted.Error(), "could not be aborted: timeout")

	interrupted := &cli.AbortedUploadError{Bucket: "b", Key: "k", UploadID: "u1", Err: context.Canceled}
	assert.Equal(t, "interrupted: the multipart upload u1 of b/k was aborted", cli.ErrorMessage(interrupted))
	assert.Equal(t, "interrupted", cli.ErrorMessage(context.Canceled))
	assert.Equal(t, "boom", cli.ErrorMessage(errors.New("boom")))
	assert.Contains(t, (&cli.AliasNotFoundError{Name: "x", Arg: "x/b"}).Error(), `unknown alias "x" in "x/b"`)
	assert.ErrorIs(t, &cli.AliasNotFoundError{Name: "x"}, domain.ErrUserNotFound)
}
