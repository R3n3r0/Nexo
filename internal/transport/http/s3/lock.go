package s3

import (
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// retainUntilFormat renders the retain-until dates of the responses (ISO
// 8601 with milliseconds, UTC).
const retainUntilFormat = "2006-01-02T15:04:05.000Z"

// Retention is the body of GetObjectRetention.
type Retention struct {
	XMLName         xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ Retention"`
	Mode            string   `xml:"Mode,omitempty"`
	RetainUntilDate string   `xml:"RetainUntilDate,omitempty"`
}

// retentionRequest is Retention as accepted by PutObjectRetention.
type retentionRequest struct {
	XMLName         xml.Name `xml:"Retention"`
	Mode            string   `xml:"Mode"`
	RetainUntilDate string   `xml:"RetainUntilDate"`
}

// LegalHold is the body of GetObjectLegalHold.
type LegalHold struct {
	XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ LegalHold"`
	Status  string   `xml:"Status"`
}

// legalHoldRequest is LegalHold as accepted by PutObjectLegalHold.
type legalHoldRequest struct {
	XMLName xml.Name `xml:"LegalHold"`
	Status  string   `xml:"Status"`
}

// objectLockRequest is ObjectLockConfiguration as accepted by
// PutObjectLockConfiguration.
type objectLockRequest struct {
	XMLName           xml.Name `xml:"ObjectLockConfiguration"`
	ObjectLockEnabled string   `xml:"ObjectLockEnabled"`
	Rule              *struct {
		DefaultRetention struct {
			Mode  string `xml:"Mode"`
			Days  *int   `xml:"Days"`
			Years *int   `xml:"Years"`
		} `xml:"DefaultRetention"`
	} `xml:"Rule"`
}

// xmlRetentionMode reads the Mode of a retention document: GOVERNANCE or
// COMPLIANCE, case-sensitive as in S3 (MalformedXML otherwise).
func xmlRetentionMode(s string) (domain.RetentionMode, error) {
	m := domain.RetentionMode(s)
	if !m.IsValid() {
		return "", domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML",
			"The XML you provided was not well-formed or did not validate against our published schema: unknown retention mode "+strconv.Quote(s))
	}
	return m, nil
}

// retentionPeriod reads the period of a default retention: exactly one of
// Days and Years (MalformedXML otherwise), a positive number
// (InvalidRetentionPeriod otherwise).
func retentionPeriod(days, years *int) (int, int, error) {
	if (days == nil) == (years == nil) {
		return 0, 0, domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML",
			"The XML you provided was not well-formed or did not validate against our published schema: the DefaultRetention needs either Days or Years")
	}
	if days != nil {
		if *days <= 0 {
			return 0, 0, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRetentionPeriod", "Default retention period must be a positive integer value for 'Days'")
		}
		return *days, 0, nil
	}
	if *years <= 0 {
		return 0, 0, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRetentionPeriod", "Default retention period must be a positive integer value for 'Years'")
	}
	return 0, *years, nil
}

// parseRetainUntil parses an ISO 8601 retain-until date.
func parseRetainUntil(v string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(v))
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// lockFromHeaders reads the Object Lock settings of a write
// (x-amz-object-lock-mode with x-amz-object-lock-retain-until-date, and
// x-amz-object-lock-legal-hold).
func lockFromHeaders(h http.Header) (domain.ObjectRetention, domain.LegalHoldStatus, error) {
	var (
		r    domain.ObjectRetention
		hold domain.LegalHoldStatus
	)
	mode, until := h.Get(httpx.HeaderAmzObjectLockMode), h.Get(httpx.HeaderAmzObjectLockRetainUntil)
	if (mode == "") != (until == "") {
		return r, hold, invalidArgument("x-amz-object-lock-retain-until-date and x-amz-object-lock-mode must both be supplied")
	}
	if mode != "" {
		m, err := domain.ParseRetentionMode(mode)
		if err != nil {
			return r, hold, invalidArgument("Unknown wormMode directive.")
		}
		t, ok := parseRetainUntil(until)
		if !ok {
			return r, hold, invalidArgument("The retain until date must be provided in ISO 8601 format")
		}
		r = domain.ObjectRetention{Mode: m, RetainUntil: t}
	}
	if v := h.Get(httpx.HeaderAmzObjectLockLegalHold); v != "" {
		parsed, err := domain.ParseLegalHold(v)
		if err != nil {
			return r, hold, invalidArgument("Legal Hold must be either of 'ON' or 'OFF'")
		}
		hold = parsed
	}
	return r, hold, nil
}

// authorizeLockHeaders checks the permissions the Object Lock settings of
// a write need on top of s3:PutObject: s3:PutObjectRetention for a
// retention, s3:PutObjectLegalHold for a legal hold.
func (a *authorizer) authorizeLockHeaders(w http.ResponseWriter, r *http.Request, bucketName, key string, ret domain.ObjectRetention, hold domain.LegalHoldStatus) bool {
	if !ret.IsZero() && !a.authorizeVersion(w, r, actionPutObjectRetention, bucketName, key, "", nil) {
		return false
	}
	return hold == "" || a.authorizeVersion(w, r, actionPutObjectLegalHold, bucketName, key, "", nil)
}

// writeLockHeaders reports the Object Lock state of a version on GET and
// HEAD, to the callers allowed to read it (s3:GetObjectRetention,
// s3:GetObjectLegalHold), as S3 does.
func (a *authorizer) writeLockHeaders(w http.ResponseWriter, r *http.Request, info domain.ObjectInfo) {
	if !info.Retention.IsZero() && a.allowed(r, actionGetObjectRetention, info.Bucket, info.Key, info.VersionID) {
		w.Header().Set(httpx.HeaderAmzObjectLockMode, string(info.Retention.Mode))
		w.Header().Set(httpx.HeaderAmzObjectLockRetainUntil, info.Retention.RetainUntil.UTC().Format(retainUntilFormat))
	}
	if info.LegalHold != "" && a.allowed(r, actionGetObjectLegalHold, info.Bucket, info.Key, info.VersionID) {
		w.Header().Set(httpx.HeaderAmzObjectLockLegalHold, string(info.LegalHold))
	}
}

// lockOptions reads the version and the governance bypass of an Object
// Lock request.
func (h *objectRoutes) lockOptions(r *http.Request, bucketName, key string) object.LockOptions {
	versionID := r.URL.Query().Get("versionId")
	return object.LockOptions{VersionID: versionID, BypassGovernance: h.authz.bypassGovernance(r, bucketName, key, versionID)}
}

func (h *objectRoutes) getRetention(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, bucketName, key) {
		return
	}
	ret, err := h.deps.Objects.GetObjectRetention(r.Context(), bucketName, key, object.LockOptions{VersionID: r.URL.Query().Get("versionId")})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, Retention{Mode: string(ret.Mode), RetainUntilDate: ret.RetainUntil.UTC().Format(retainUntilFormat)})
}

func (h *objectRoutes) putRetention(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, bucketName, key) {
		return
	}
	var doc retentionRequest
	if err := decodeXMLBody(r, h.deps.Enc, maxXMLBody, &doc); err != nil {
		h.fail(w, r, err)
		return
	}
	var ret domain.ObjectRetention
	if doc.Mode != "" || doc.RetainUntilDate != "" {
		mode, err := xmlRetentionMode(doc.Mode)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		until, ok := parseRetainUntil(doc.RetainUntilDate)
		if !ok && doc.RetainUntilDate != "" {
			h.fail(w, r, domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema: invalid RetainUntilDate"))
			return
		}
		ret = domain.ObjectRetention{Mode: mode, RetainUntil: until}
	}
	info, err := h.deps.Objects.PutObjectRetention(r.Context(), bucketName, key, ret, h.lockOptions(r, bucketName, key))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if info.VersionID != "" {
		w.Header().Set(httpx.HeaderAmzVersionID, info.VersionID)
	}
	w.WriteHeader(http.StatusOK)
}

func (h *objectRoutes) getLegalHold(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, bucketName, key) {
		return
	}
	hold, err := h.deps.Objects.GetObjectLegalHold(r.Context(), bucketName, key, object.LockOptions{VersionID: r.URL.Query().Get("versionId")})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, LegalHold{Status: string(hold)})
}

func (h *objectRoutes) putLegalHold(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, bucketName, key) {
		return
	}
	var doc legalHoldRequest
	if err := decodeXMLBody(r, h.deps.Enc, maxXMLBody, &doc); err != nil {
		h.fail(w, r, err)
		return
	}
	hold, err := domain.ParseLegalHold(doc.Status)
	if err != nil {
		h.fail(w, r, domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema: Status must be ON or OFF"))
		return
	}
	info, err := h.deps.Objects.PutObjectLegalHold(r.Context(), bucketName, key, hold, object.LockOptions{VersionID: r.URL.Query().Get("versionId")})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if info.VersionID != "" {
		w.Header().Set(httpx.HeaderAmzVersionID, info.VersionID)
	}
	w.WriteHeader(http.StatusOK)
}

// putObjectLock sets the default retention of a bucket created with
// object locking.
func (h *bucketRoutes) putObjectLock(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	var doc objectLockRequest
	if err := decodeXMLBody(r, h.deps.Enc, maxXMLBody, &doc); err != nil {
		h.fail(w, r, err)
		return
	}
	if doc.ObjectLockEnabled != "Enabled" {
		h.fail(w, r, domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema: ObjectLockEnabled must be Enabled"))
		return
	}
	cfg := bucket.ObjectLockConfig{Enabled: true}
	if doc.Rule != nil {
		d := doc.Rule.DefaultRetention
		mode, err := xmlRetentionMode(d.Mode)
		if err == nil {
			cfg.Mode = mode
			cfg.Days, cfg.Years, err = retentionPeriod(d.Days, d.Years)
		}
		if err != nil {
			h.fail(w, r, err)
			return
		}
	}
	if err := h.deps.Buckets.SetObjectLock(r.Context(), name, cfg); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}
