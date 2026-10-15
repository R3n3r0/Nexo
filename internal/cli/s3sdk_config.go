package cli

import (
	"context"
	"crypto/md5" // #nosec G501 -- the SSE-C key MD5 header required by S3
	"encoding/base64"
	"math"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/r3n3r0/nexo/internal/domain"
)

// This file holds the S3Client methods of the M7 bucket and object
// settings (Object Lock, lifecycle, CORS, notifications) and the helpers
// that map the server-side encryption, the checksums and the Object Lock
// state onto the inputs and outputs of aws-sdk-go-v2.

// customerKeyFields are the SSE-C headers of a request: the SDK sends them
// as given, so the key is base64-encoded and its MD5 computed here.
type customerKeyFields struct {
	algorithm, key, keyMD5 *string
}

// customerKeyOf returns the SSE-C headers of an encryption (none unless
// it is SSE-C).
func customerKeyOf(r domain.SSERequest) customerKeyFields {
	if r.Type != domain.SSEC || r.CustomerKey.IsZero() {
		return customerKeyFields{}
	}
	keyMD5 := r.CustomerKey.KeyMD5
	if keyMD5 == "" {
		sum := md5.Sum(r.CustomerKey.Key) // #nosec G401 -- the SSE-C key MD5 header required by S3, not a security primitive
		keyMD5 = base64.StdEncoding.EncodeToString(sum[:])
	}
	return customerKeyFields{
		algorithm: aws.String(domain.SSEAlgorithmAES256),
		key:       aws.String(base64.StdEncoding.EncodeToString(r.CustomerKey.Key)),
		keyMD5:    aws.String(keyMD5),
	}
}

// sseFields are the encryption headers of a write.
type sseFields struct {
	algorithm types.ServerSideEncryption
	kmsKeyID  *string
	customer  customerKeyFields
}

// sseOf returns the encryption headers of a write.
func sseOf(r domain.SSERequest) sseFields {
	switch r.Type {
	case domain.SSES3:
		return sseFields{algorithm: types.ServerSideEncryptionAes256}
	case domain.SSEKMS:
		out := sseFields{algorithm: types.ServerSideEncryptionAwsKms}
		if r.KMSKeyID != "" {
			out.kmsKeyID = aws.String(r.KMSKeyID)
		}
		return out
	case domain.SSEC:
		return sseFields{customer: customerKeyOf(r)}
	}
	return sseFields{}
}

// encryptionOf reads the encryption of a version from a response.
func encryptionOf(algorithm types.ServerSideEncryption, kmsKeyID, customerAlgorithm, customerKeyMD5 *string) domain.EncryptionInfo {
	switch {
	case aws.ToString(customerAlgorithm) != "":
		return domain.EncryptionInfo{Type: domain.SSEC, CustomerKeyMD5: aws.ToString(customerKeyMD5)}
	case algorithm == types.ServerSideEncryptionAes256:
		return domain.EncryptionInfo{Type: domain.SSES3}
	case algorithm == types.ServerSideEncryptionAwsKms || algorithm == types.ServerSideEncryptionAwsKmsDsse:
		return domain.EncryptionInfo{Type: domain.SSEKMS, KMSKeyID: kmsKeyFromARN(aws.ToString(kmsKeyID))}
	}
	return domain.EncryptionInfo{}
}

// kmsKeyFromARN returns the key of an ARN (arn:aws:kms:...:key/ID), or the
// value itself.
func kmsKeyFromARN(v string) string {
	if strings.HasPrefix(v, "arn:aws:kms:") {
		if i := strings.LastIndex(v, "key/"); i >= 0 {
			return v[i+len("key/"):]
		}
	}
	return v
}

// retentionOf reads the retention of a version from a response.
func retentionOf(mode types.ObjectLockMode, until *time.Time) domain.ObjectRetention {
	if mode == "" && until == nil {
		return domain.ObjectRetention{}
	}
	return domain.ObjectRetention{Mode: domain.RetentionMode(mode), RetainUntil: aws.ToTime(until).UTC()}
}

// checksumValues are the x-amz-checksum-* values of a response, in the
// order CRC32, CRC32C, CRC64NVME, SHA1, SHA256.
type checksumValues [5]*string

// checksumAlgorithmsOrder is the algorithm of each checksumValues entry.
func checksumAlgorithmsOrder() [5]domain.ChecksumAlgorithm {
	return [5]domain.ChecksumAlgorithm{domain.ChecksumCRC32, domain.ChecksumCRC32C, domain.ChecksumCRC64NVME, domain.ChecksumSHA1, domain.ChecksumSHA256}
}

// checksumOf returns the checksum of a response (zero when none).
func checksumOf(values checksumValues, typ types.ChecksumType) domain.Checksum {
	algos := checksumAlgorithmsOrder()
	for i, v := range values {
		if s := aws.ToString(v); s != "" {
			return domain.Checksum{Algorithm: algos[i], Value: s, Type: domain.ChecksumType(typ)}
		}
	}
	return domain.Checksum{}
}

// setPartChecksum copies the checksum of an uploaded part into its entry
// of CompleteMultipartUpload.
func setPartChecksum(p *types.CompletedPart, values checksumValues) {
	p.ChecksumCRC32, p.ChecksumCRC32C, p.ChecksumCRC64NVME, p.ChecksumSHA1, p.ChecksumSHA256 = values[0], values[1], values[2], values[3], values[4]
}

// --- Object Lock -----------------------------------------------------------------

// GetObjectLock implements S3Client.
func (c *sdkClient) GetObjectLock(ctx context.Context, bucket string) (*ObjectLockConfig, error) {
	out, err := c.api.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: aws.String(bucket)})
	if err != nil {
		if isCode(err, "ObjectLockConfigurationNotFoundError", "NoSuchObjectLockConfiguration") {
			return &ObjectLockConfig{}, nil
		}
		return nil, mapS3Error(err, bucket, "")
	}
	cfg := &ObjectLockConfig{}
	if lc := out.ObjectLockConfiguration; lc != nil {
		cfg.Enabled = lc.ObjectLockEnabled == types.ObjectLockEnabledEnabled
		if lc.Rule != nil && lc.Rule.DefaultRetention != nil {
			d := lc.Rule.DefaultRetention
			cfg.Default = domain.DefaultRetention{
				Mode: domain.RetentionMode(d.Mode), Days: int(aws.ToInt32(d.Days)), Years: int(aws.ToInt32(d.Years)),
			}
		}
	}
	return cfg, nil
}

// SetObjectLock implements S3Client.
func (c *sdkClient) SetObjectLock(ctx context.Context, bucket string, def domain.DefaultRetention) error {
	if err := def.Validate(); err != nil {
		return UsageError("invalid default retention: %v", err)
	}
	cfg := &types.ObjectLockConfiguration{ObjectLockEnabled: types.ObjectLockEnabledEnabled}
	if !def.IsZero() {
		d := &types.DefaultRetention{Mode: types.ObjectLockRetentionMode(def.Mode)}
		if def.Days > 0 {
			d.Days = aws.Int32(int32(min(def.Days, maxRetentionDays)))
		}
		if def.Years > 0 {
			d.Years = aws.Int32(int32(min(def.Years, maxRetentionYears)))
		}
		cfg.Rule = &types.ObjectLockRule{DefaultRetention: d}
	}
	_, err := c.api.PutObjectLockConfiguration(ctx, &s3.PutObjectLockConfigurationInput{Bucket: aws.String(bucket), ObjectLockConfiguration: cfg})
	return mapS3Error(err, bucket, "")
}

// Bounds of a default retention (S3: 36500 days, 100 years).
const (
	maxRetentionDays  = 36500
	maxRetentionYears = 100
)

// GetRetention implements S3Client.
func (c *sdkClient) GetRetention(ctx context.Context, ref ObjectRef) (domain.ObjectRetention, error) {
	in := &s3.GetObjectRetentionInput{Bucket: aws.String(ref.Bucket), Key: aws.String(ref.Key)}
	if ref.VersionID != "" {
		in.VersionId = aws.String(ref.VersionID)
	}
	out, err := c.api.GetObjectRetention(ctx, in)
	if err != nil {
		if isCode(err, "NoSuchObjectLockConfiguration", "ObjectLockConfigurationNotFoundError") {
			return domain.ObjectRetention{}, nil
		}
		return domain.ObjectRetention{}, mapS3Error(err, ref.Bucket, ref.Key)
	}
	if out.Retention == nil {
		return domain.ObjectRetention{}, nil
	}
	return retentionOf(types.ObjectLockMode(out.Retention.Mode), out.Retention.RetainUntilDate), nil
}

// PutRetention implements S3Client.
func (c *sdkClient) PutRetention(ctx context.Context, ref ObjectRef, retention domain.ObjectRetention, bypass bool) error {
	in := &s3.PutObjectRetentionInput{Bucket: aws.String(ref.Bucket), Key: aws.String(ref.Key), Retention: &types.ObjectLockRetention{}}
	if ref.VersionID != "" {
		in.VersionId = aws.String(ref.VersionID)
	}
	if !retention.IsZero() {
		in.Retention.Mode = types.ObjectLockRetentionMode(retention.Mode)
		in.Retention.RetainUntilDate = aws.Time(retention.RetainUntil.UTC())
	}
	if bypass {
		in.BypassGovernanceRetention = aws.Bool(true)
	}
	_, err := c.api.PutObjectRetention(ctx, in)
	return mapS3Error(err, ref.Bucket, ref.Key)
}

// GetLegalHold implements S3Client.
func (c *sdkClient) GetLegalHold(ctx context.Context, ref ObjectRef) (domain.LegalHoldStatus, error) {
	in := &s3.GetObjectLegalHoldInput{Bucket: aws.String(ref.Bucket), Key: aws.String(ref.Key)}
	if ref.VersionID != "" {
		in.VersionId = aws.String(ref.VersionID)
	}
	out, err := c.api.GetObjectLegalHold(ctx, in)
	if err != nil {
		if isCode(err, "NoSuchObjectLockConfiguration", "ObjectLockConfigurationNotFoundError") {
			return domain.LegalHoldOff, nil
		}
		return "", mapS3Error(err, ref.Bucket, ref.Key)
	}
	if out.LegalHold == nil || out.LegalHold.Status == "" {
		return domain.LegalHoldOff, nil
	}
	return domain.LegalHoldStatus(out.LegalHold.Status), nil
}

// PutLegalHold implements S3Client.
func (c *sdkClient) PutLegalHold(ctx context.Context, ref ObjectRef, hold domain.LegalHoldStatus) error {
	if !hold.IsValid() {
		return UsageError("invalid legal hold %q (ON, OFF)", hold)
	}
	in := &s3.PutObjectLegalHoldInput{
		Bucket: aws.String(ref.Bucket), Key: aws.String(ref.Key),
		LegalHold: &types.ObjectLockLegalHold{Status: types.ObjectLockLegalHoldStatus(hold)},
	}
	if ref.VersionID != "" {
		in.VersionId = aws.String(ref.VersionID)
	}
	_, err := c.api.PutObjectLegalHold(ctx, in)
	return mapS3Error(err, ref.Bucket, ref.Key)
}

// --- lifecycle ---------------------------------------------------------------------

// GetLifecycle implements S3Client.
func (c *sdkClient) GetLifecycle(ctx context.Context, bucket string) (*LifecycleConfiguration, error) {
	out, err := c.api.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(bucket)})
	if err != nil {
		if isCode(err, "NoSuchLifecycleConfiguration") {
			return &LifecycleConfiguration{Rules: []LifecycleRule{}}, nil
		}
		return nil, mapS3Error(err, bucket, "")
	}
	cfg := &LifecycleConfiguration{Rules: make([]LifecycleRule, 0, len(out.Rules))}
	for _, r := range out.Rules {
		cfg.Rules = append(cfg.Rules, lifecycleRuleFromSDK(r))
	}
	return cfg, nil
}

// PutLifecycle implements S3Client.
func (c *sdkClient) PutLifecycle(ctx context.Context, bucket string, cfg LifecycleConfiguration) error {
	if len(cfg.Rules) == 0 {
		_, err := c.api.DeleteBucketLifecycle(ctx, &s3.DeleteBucketLifecycleInput{Bucket: aws.String(bucket)})
		return mapS3Error(err, bucket, "")
	}
	rules := make([]types.LifecycleRule, 0, len(cfg.Rules))
	for i, r := range cfg.Rules {
		sr, err := lifecycleRuleToSDK(r)
		if err != nil {
			return UsageError("rule %d: %v", i+1, err)
		}
		rules = append(rules, sr)
	}
	_, err := c.api.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket: aws.String(bucket), LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: rules},
	})
	return mapS3Error(err, bucket, "")
}

// intPtr32 converts an optional number of the JSON form.
func intPtr32(n *int) *int32 {
	if n == nil {
		return nil
	}
	// A number out of the int32 range is clamped rather than wrapped: the
	// server refuses it instead of reading another value (days of 2^32+1
	// would otherwise become 1).
	v := min(max(*n, math.MinInt32), math.MaxInt32)
	return aws.Int32(int32(v)) // #nosec G115 -- clamped to the int32 range above
}

// intPtrOf converts an optional number of the SDK.
func intPtrOf(n *int32) *int {
	if n == nil {
		return nil
	}
	v := int(*n)
	return &v
}

// lifecycleTime parses the YYYY-MM-DD date of a rule.
func lifecycleTime(date string) (*time.Time, error) {
	if date == "" {
		return nil, nil
	}
	norm, err := ParseLifecycleDate(date)
	if err != nil {
		return nil, err
	}
	t, _ := time.Parse(LifecycleDateLayout, norm)
	return aws.Time(t.UTC()), nil
}

// lifecycleDate renders the date of a rule (empty for none).
func lifecycleDate(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(LifecycleDateLayout)
}

func lifecycleRuleToSDK(r LifecycleRule) (types.LifecycleRule, error) {
	out := types.LifecycleRule{Status: types.ExpirationStatusEnabled, Filter: lifecycleFilterToSDK(r.Filter)}
	if r.ID != "" {
		out.ID = aws.String(r.ID)
	}
	if !RuleEnabled(r) {
		out.Status = types.ExpirationStatusDisabled
	}
	if e := r.Expiration; e != nil {
		date, err := lifecycleTime(e.Date)
		if err != nil {
			return out, err
		}
		out.Expiration = &types.LifecycleExpiration{Days: intPtr32(e.Days), Date: date}
		if e.ExpiredObjectDeleteMarker {
			out.Expiration.ExpiredObjectDeleteMarker = aws.Bool(true)
		}
	}
	if n := r.NoncurrentExpiration; n != nil {
		out.NoncurrentVersionExpiration = &types.NoncurrentVersionExpiration{
			NoncurrentDays: intPtr32(n.NoncurrentDays), NewerNoncurrentVersions: intPtr32(n.NewerNoncurrentVersions),
		}
	}
	if a := r.AbortIncompleteUpload; a != nil {
		out.AbortIncompleteMultipartUpload = &types.AbortIncompleteMultipartUpload{DaysAfterInitiation: intPtr32(a.DaysAfterInitiation)}
	}
	for _, t := range r.Transitions {
		date, err := lifecycleTime(t.Date)
		if err != nil {
			return out, err
		}
		out.Transitions = append(out.Transitions, types.Transition{Days: intPtr32(t.Days), Date: date, StorageClass: types.TransitionStorageClass(t.StorageClass)})
	}
	for _, t := range r.NoncurrentTransitions {
		out.NoncurrentVersionTransitions = append(out.NoncurrentVersionTransitions, types.NoncurrentVersionTransition{
			NoncurrentDays: intPtr32(t.NoncurrentDays), NewerNoncurrentVersions: intPtr32(t.NewerNoncurrentVersions),
			StorageClass: types.TransitionStorageClass(t.StorageClass),
		})
	}
	return out, nil
}

// lifecycleFilterToSDK renders the filter of a rule: always a Filter (an
// empty prefix selects every object).
func lifecycleFilterToSDK(f *LifecycleFilter) *types.LifecycleRuleFilter {
	if f == nil {
		return &types.LifecycleRuleFilter{Prefix: aws.String("")}
	}
	conditions := len(f.Tags)
	if f.Prefix != "" {
		conditions++
	}
	if f.SizeGreaterThan != nil {
		conditions++
	}
	if f.SizeLessThan != nil {
		conditions++
	}
	tags := make([]types.Tag, 0, len(f.Tags))
	for _, k := range SortedKeys(f.Tags) {
		tags = append(tags, types.Tag{Key: aws.String(k), Value: aws.String(f.Tags[k])})
	}
	switch {
	case conditions > 1:
		and := &types.LifecycleRuleAndOperator{Tags: tags, ObjectSizeGreaterThan: f.SizeGreaterThan, ObjectSizeLessThan: f.SizeLessThan}
		if f.Prefix != "" {
			and.Prefix = aws.String(f.Prefix)
		}
		return &types.LifecycleRuleFilter{And: and}
	case len(tags) == 1:
		return &types.LifecycleRuleFilter{Tag: &tags[0]}
	case f.SizeGreaterThan != nil || f.SizeLessThan != nil:
		return &types.LifecycleRuleFilter{ObjectSizeGreaterThan: f.SizeGreaterThan, ObjectSizeLessThan: f.SizeLessThan}
	default:
		return &types.LifecycleRuleFilter{Prefix: aws.String(f.Prefix)}
	}
}

func lifecycleRuleFromSDK(r types.LifecycleRule) LifecycleRule {
	out := LifecycleRule{ID: aws.ToString(r.ID)}
	if r.Status == types.ExpirationStatusDisabled {
		out.Enabled = boolPtr(false)
	} else {
		out.Enabled = boolPtr(true)
	}
	out.Filter = lifecycleFilterFromSDK(r.Filter, aws.ToString(r.Prefix)) //nolint:staticcheck // SA1019: the legacy Prefix of the rule is still read.
	if e := r.Expiration; e != nil {
		out.Expiration = &LifecycleExpiration{Days: intPtrOf(e.Days), Date: lifecycleDate(e.Date), ExpiredObjectDeleteMarker: aws.ToBool(e.ExpiredObjectDeleteMarker)}
	}
	if n := r.NoncurrentVersionExpiration; n != nil {
		out.NoncurrentExpiration = &LifecycleNoncurrentExpiration{NoncurrentDays: intPtrOf(n.NoncurrentDays), NewerNoncurrentVersions: intPtrOf(n.NewerNoncurrentVersions)}
	}
	if a := r.AbortIncompleteMultipartUpload; a != nil {
		out.AbortIncompleteUpload = &LifecycleAbortUpload{DaysAfterInitiation: intPtrOf(a.DaysAfterInitiation)}
	}
	for _, t := range r.Transitions {
		out.Transitions = append(out.Transitions, LifecycleTransition{Days: intPtrOf(t.Days), Date: lifecycleDate(t.Date), StorageClass: string(t.StorageClass)})
	}
	for _, t := range r.NoncurrentVersionTransitions {
		out.NoncurrentTransitions = append(out.NoncurrentTransitions, LifecycleNoncurrentTransition{
			NoncurrentDays: intPtrOf(t.NoncurrentDays), NewerNoncurrentVersions: intPtrOf(t.NewerNoncurrentVersions), StorageClass: string(t.StorageClass),
		})
	}
	return out
}

func lifecycleFilterFromSDK(f *types.LifecycleRuleFilter, legacyPrefix string) *LifecycleFilter {
	out := &LifecycleFilter{Prefix: legacyPrefix}
	if f != nil {
		if f.And != nil {
			out.Prefix = aws.ToString(f.And.Prefix)
			for _, t := range f.And.Tags {
				if out.Tags == nil {
					out.Tags = map[string]string{}
				}
				out.Tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
			}
			out.SizeGreaterThan, out.SizeLessThan = f.And.ObjectSizeGreaterThan, f.And.ObjectSizeLessThan
		} else {
			if f.Prefix != nil {
				out.Prefix = aws.ToString(f.Prefix)
			}
			if f.Tag != nil {
				out.Tags = map[string]string{aws.ToString(f.Tag.Key): aws.ToString(f.Tag.Value)}
			}
			out.SizeGreaterThan, out.SizeLessThan = f.ObjectSizeGreaterThan, f.ObjectSizeLessThan
		}
	}
	if out.Prefix == "" && len(out.Tags) == 0 && out.SizeGreaterThan == nil && out.SizeLessThan == nil {
		return nil
	}
	return out
}

// --- CORS --------------------------------------------------------------------

// GetCORS implements S3Client.
func (c *sdkClient) GetCORS(ctx context.Context, bucket string) (*CORSConfiguration, error) {
	out, err := c.api.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: aws.String(bucket)})
	if err != nil {
		if isCode(err, "NoSuchCORSConfiguration") {
			return &CORSConfiguration{Rules: []CORSRule{}}, nil
		}
		return nil, mapS3Error(err, bucket, "")
	}
	cfg := &CORSConfiguration{Rules: make([]CORSRule, 0, len(out.CORSRules))}
	for _, r := range out.CORSRules {
		cfg.Rules = append(cfg.Rules, CORSRule{
			ID: aws.ToString(r.ID), AllowedOrigins: r.AllowedOrigins, AllowedMethods: r.AllowedMethods,
			AllowedHeaders: r.AllowedHeaders, ExposeHeaders: r.ExposeHeaders, MaxAgeSeconds: intPtrOf(r.MaxAgeSeconds),
		})
	}
	return cfg, nil
}

// PutCORS implements S3Client.
func (c *sdkClient) PutCORS(ctx context.Context, bucket string, cfg CORSConfiguration) error {
	if len(cfg.Rules) == 0 {
		_, err := c.api.DeleteBucketCors(ctx, &s3.DeleteBucketCorsInput{Bucket: aws.String(bucket)})
		return mapS3Error(err, bucket, "")
	}
	rules := make([]types.CORSRule, 0, len(cfg.Rules))
	for _, r := range cfg.Rules {
		rule := types.CORSRule{
			AllowedOrigins: r.AllowedOrigins, AllowedMethods: r.AllowedMethods,
			AllowedHeaders: r.AllowedHeaders, ExposeHeaders: r.ExposeHeaders, MaxAgeSeconds: intPtr32(r.MaxAgeSeconds),
		}
		if r.ID != "" {
			rule.ID = aws.String(r.ID)
		}
		rules = append(rules, rule)
	}
	_, err := c.api.PutBucketCors(ctx, &s3.PutBucketCorsInput{Bucket: aws.String(bucket), CORSConfiguration: &types.CORSConfiguration{CORSRules: rules}})
	return mapS3Error(err, bucket, "")
}

// --- notifications ------------------------------------------------------------

// GetNotifications implements S3Client.
func (c *sdkClient) GetNotifications(ctx context.Context, bucket string) (*BucketNotifications, error) {
	out, err := c.api.GetBucketNotificationConfiguration(ctx, &s3.GetBucketNotificationConfigurationInput{Bucket: aws.String(bucket)})
	if err != nil {
		return nil, mapS3Error(err, bucket, "")
	}
	cfg := &BucketNotifications{Subscriptions: []EventSubscription{}}
	add := func(id, arn *string, events []types.Event, filter *types.NotificationConfigurationFilter) {
		sub := EventSubscription{ID: aws.ToString(id), ARN: aws.ToString(arn), Events: make([]string, 0, len(events))}
		for _, e := range events {
			sub.Events = append(sub.Events, string(e))
		}
		if filter != nil && filter.Key != nil {
			for _, fr := range filter.Key.FilterRules {
				switch strings.ToLower(string(fr.Name)) {
				case "prefix":
					sub.Prefix = aws.ToString(fr.Value)
				case "suffix":
					sub.Suffix = aws.ToString(fr.Value)
				}
			}
		}
		cfg.Subscriptions = append(cfg.Subscriptions, sub)
	}
	for _, q := range out.QueueConfigurations {
		add(q.Id, q.QueueArn, q.Events, q.Filter)
	}
	for _, t := range out.TopicConfigurations {
		add(t.Id, t.TopicArn, t.Events, t.Filter)
	}
	for _, l := range out.LambdaFunctionConfigurations {
		add(l.Id, l.LambdaFunctionArn, l.Events, l.Filter)
	}
	return cfg, nil
}

// PutNotifications implements S3Client: each subscription is a Queue,
// Topic or CloudFunction configuration after the service of its ARN.
func (c *sdkClient) PutNotifications(ctx context.Context, bucket string, cfg BucketNotifications) error {
	doc := &types.NotificationConfiguration{}
	for i, sub := range cfg.Subscriptions {
		if err := ValidateARN(sub.ARN); err != nil {
			return err
		}
		if len(sub.Events) == 0 {
			return UsageError("subscription %d (%s): no event type", i+1, sub.ARN)
		}
		events := make([]types.Event, 0, len(sub.Events))
		for _, e := range sub.Events {
			events = append(events, types.Event(e))
		}
		var filter *types.NotificationConfigurationFilter
		if sub.Prefix != "" || sub.Suffix != "" {
			key := &types.S3KeyFilter{}
			if sub.Prefix != "" {
				key.FilterRules = append(key.FilterRules, types.FilterRule{Name: types.FilterRuleNamePrefix, Value: aws.String(sub.Prefix)})
			}
			if sub.Suffix != "" {
				key.FilterRules = append(key.FilterRules, types.FilterRule{Name: types.FilterRuleNameSuffix, Value: aws.String(sub.Suffix)})
			}
			filter = &types.NotificationConfigurationFilter{Key: key}
		}
		var id *string
		if sub.ID != "" {
			id = aws.String(sub.ID)
		}
		switch ARNService(sub.ARN) {
		case "sns":
			doc.TopicConfigurations = append(doc.TopicConfigurations, types.TopicConfiguration{Id: id, TopicArn: aws.String(sub.ARN), Events: events, Filter: filter})
		case "lambda":
			doc.LambdaFunctionConfigurations = append(doc.LambdaFunctionConfigurations, types.LambdaFunctionConfiguration{Id: id, LambdaFunctionArn: aws.String(sub.ARN), Events: events, Filter: filter})
		default:
			doc.QueueConfigurations = append(doc.QueueConfigurations, types.QueueConfiguration{Id: id, QueueArn: aws.String(sub.ARN), Events: events, Filter: filter})
		}
	}
	_, err := c.api.PutBucketNotificationConfiguration(ctx, &s3.PutBucketNotificationConfigurationInput{Bucket: aws.String(bucket), NotificationConfiguration: doc})
	return mapS3Error(err, bucket, "")
}
