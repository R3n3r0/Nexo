package admin

import (
	"context"
	"errors"
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// headerBypassGovernance is the S3 header asking to bypass a GOVERNANCE
// retention: the console operations that bypass it are evaluated with the
// condition values of a request carrying it.
const headerBypassGovernance = "x-amz-bypass-governance-retention" // #nosec G101 -- an S3 header name, not a credential

// maxAttributeParts is the largest page of parts of Attributes.
const maxAttributeParts = 1000

// bypassRequest describes the S3 request of an operation on a version that
// bypasses the governance retention (or not).
func bypassRequest(versionID string, bypass bool) ObjectRequest {
	or := ObjectRequest{VersionID: versionID}
	if bypass {
		or.Headers = map[string]string{headerBypassGovernance: "true"}
	}
	return or
}

// Retention implements ObjectBrowser.
func (b *objectBrowser) Retention(ctx context.Context, ref VersionRef) (domain.ObjectRetention, error) {
	if err := domain.ValidateObjectKey(ref.Key); err != nil {
		return domain.ObjectRetention{}, err
	}
	r, err := b.deps.Objects.GetObjectRetention(ctx, ref.Bucket, ref.Key, object.LockOptions{VersionID: ref.VersionID})
	if errors.Is(err, domain.ErrPolicyNotFound) {
		return domain.ObjectRetention{}, nil
	}
	return r, err
}

// SetRetention implements ObjectBrowser.
func (b *objectBrowser) SetRetention(ctx context.Context, in SetRetentionInput) (domain.ObjectInfo, error) {
	ref := in.Object
	if err := domain.ValidateObjectKey(ref.Key); err != nil {
		return domain.ObjectInfo{}, err
	}
	ret := in.Retention
	switch {
	case ret.Mode == "" && ret.RetainUntil.IsZero():
	case ret.Mode == "" || ret.RetainUntil.IsZero():
		return domain.ObjectInfo{}, fmt.Errorf("%w: a retention needs both a mode and a retain-until date", domain.ErrInvalidArgument)
	default:
		mode, err := domain.ParseRetentionMode(string(ret.Mode))
		if err != nil {
			return domain.ObjectInfo{}, err
		}
		ret = domain.ObjectRetention{Mode: mode, RetainUntil: ret.RetainUntil.UTC()}
	}
	or := bypassRequest(ref.VersionID, in.BypassGovernance)
	if err := authorizeObject(ctx, b.deps.Authz, in.Requester, ref.Bucket, ref.Key, or, policy.ActionPutObjectRetention); err != nil {
		return domain.ObjectInfo{}, err
	}
	if in.BypassGovernance {
		// Asked explicitly: a requester that may not bypass is told so,
		// rather than failing later on the retention it cannot shorten.
		if err := authorizeObject(ctx, b.deps.Authz, in.Requester, ref.Bucket, ref.Key, or, policy.ActionBypassGovernanceRetention); err != nil {
			return domain.ObjectInfo{}, err
		}
	}
	info, err := b.deps.Objects.PutObjectRetention(ctx, ref.Bucket, ref.Key, ret, object.LockOptions{VersionID: ref.VersionID, BypassGovernance: in.BypassGovernance})
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	b.deps.Logger.InfoContext(ctx, "admin: object retention set", "bucket", ref.Bucket, "key", ref.Key, "version", info.VersionID,
		"mode", ret.Mode, "bypassGovernance", in.BypassGovernance)
	return info, nil
}

// LegalHold implements ObjectBrowser.
func (b *objectBrowser) LegalHold(ctx context.Context, ref VersionRef) (domain.LegalHoldStatus, error) {
	if err := domain.ValidateObjectKey(ref.Key); err != nil {
		return "", err
	}
	hold, err := b.deps.Objects.GetObjectLegalHold(ctx, ref.Bucket, ref.Key, object.LockOptions{VersionID: ref.VersionID})
	if errors.Is(err, domain.ErrPolicyNotFound) {
		return domain.LegalHoldOff, nil
	}
	return hold, err
}

// SetLegalHold implements ObjectBrowser.
func (b *objectBrowser) SetLegalHold(ctx context.Context, in SetLegalHoldInput) (domain.ObjectInfo, error) {
	ref := in.Object
	if err := domain.ValidateObjectKey(ref.Key); err != nil {
		return domain.ObjectInfo{}, err
	}
	hold, err := domain.ParseLegalHold(string(in.Hold))
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	info, err := b.deps.Objects.PutObjectLegalHold(ctx, ref.Bucket, ref.Key, hold, object.LockOptions{VersionID: ref.VersionID})
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	b.deps.Logger.InfoContext(ctx, "admin: object legal hold set", "bucket", ref.Bucket, "key", ref.Key, "version", info.VersionID, "status", hold)
	return info, nil
}

// Attributes implements ObjectBrowser.
func (b *objectBrowser) Attributes(ctx context.Context, in AttributesInput) (*object.ObjectAttributes, error) {
	ref := in.Object
	if err := domain.ValidateObjectKey(ref.Key); err != nil {
		return nil, err
	}
	if in.MaxParts < 0 || in.MaxParts > maxAttributeParts {
		return nil, fmt.Errorf("%w: maxParts must be between 1 and %d", domain.ErrInvalidArgument, maxAttributeParts)
	}
	if in.PartNumberMarker < 0 {
		return nil, fmt.Errorf("%w: partNumberMarker must not be negative", domain.ErrInvalidArgument)
	}
	attrs, err := b.deps.Objects.GetObjectAttributes(ctx, ref.Bucket, ref.Key, object.AttributesOptions{
		VersionID: ref.VersionID, MaxParts: in.MaxParts, PartNumberMarker: in.PartNumberMarker,
	})
	if err != nil {
		return nil, err
	}
	return &attrs, nil
}
