package object

import (
	"context"
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
)

// GetObjectAttributes implements Service.
func (l *layer) GetObjectAttributes(ctx context.Context, bucket, key string, opts AttributesOptions) (ObjectAttributes, error) {
	if err := validObject(bucket, key); err != nil {
		return ObjectAttributes{}, err
	}
	if opts.PartNumberMarker < 0 {
		return ObjectAttributes{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "Part number marker must be a non-negative integer")
	}
	ctx, unlock, err := l.lock(ctx, bucket, key, false)
	if err != nil {
		return ObjectAttributes{}, err
	}
	defer unlock()
	st, err := l.place.forVersion(ctx, bucket, key, opts.VersionID)
	if err != nil {
		return ObjectAttributes{}, fmt.Errorf("%s: attributes of %s/%s: %w", l.name, bucket, key, err)
	}
	fi, err := l.readVersion(ctx, st, bucket, key, opts.VersionID, false)
	if err != nil {
		return ObjectAttributes{}, fmt.Errorf("%s: attributes of %s/%s: %w", l.name, bucket, key, err)
	}
	res := ObjectAttributes{Info: objectInfo(bucket, key, fi)}
	if res.Info.Parts == 0 {
		return res, nil
	}
	res.PartsCount = len(fi.Parts)
	limit := clampMaxKeys(opts.MaxParts)
	for i, p := range fi.Parts {
		if p.Number <= opts.PartNumberMarker {
			continue
		}
		if len(res.Parts) == limit {
			if limit > 0 {
				res.IsTruncated = true
				res.NextPartNumberMarker = res.Parts[len(res.Parts)-1].Number
			}
			break
		}
		res.Parts = append(res.Parts, domain.ObjectPart{Number: p.Number, Size: partPlainSize(fi, p), Checksum: partChecksum(fi, i+1)})
	}
	return res, nil
}
