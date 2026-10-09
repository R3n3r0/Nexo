package cli

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// The bucket configurations of the ilm, cors and event commands travel in
// the JSON form of the Admin API (pkg/nexoadmin): a configuration exported
// by nexoctl can be pasted in the JSON view of the console (D-BKT-03) and
// the other way round. The S3 API stores the same documents as XML; the
// import commands read both forms.
type (
	// LifecycleConfiguration is the lifecycle configuration of a bucket
	// (S-BKT-07).
	LifecycleConfiguration = nexoadmin.LifecycleConfiguration
	// LifecycleRule is one lifecycle rule.
	LifecycleRule = nexoadmin.LifecycleRule
	// LifecycleFilter selects the objects of a lifecycle rule.
	LifecycleFilter = nexoadmin.LifecycleFilter
	// LifecycleExpiration expires the current versions.
	LifecycleExpiration = nexoadmin.LifecycleExpiration
	// LifecycleNoncurrentExpiration removes the noncurrent versions.
	LifecycleNoncurrentExpiration = nexoadmin.LifecycleNoncurrentExpiration
	// LifecycleAbortUpload aborts the incomplete multipart uploads.
	LifecycleAbortUpload = nexoadmin.LifecycleAbortUpload
	// LifecycleTransition moves the current versions to a remote tier.
	LifecycleTransition = nexoadmin.LifecycleTransition
	// LifecycleNoncurrentTransition moves the noncurrent versions to a
	// remote tier.
	LifecycleNoncurrentTransition = nexoadmin.LifecycleNoncurrentTransition
	// CORSConfiguration is the CORS configuration of a bucket (S-BKT-06).
	CORSConfiguration = nexoadmin.CORSConfiguration
	// CORSRule is one CORS rule.
	CORSRule = nexoadmin.CORSRule
	// BucketNotifications are the event subscriptions of a bucket
	// (S-BKT-10).
	BucketNotifications = nexoadmin.BucketNotifications
	// EventSubscription routes the events of a bucket to a target.
	EventSubscription = nexoadmin.EventSubscription
)

// LifecycleDateLayout is the layout of the dates of the lifecycle rules
// (midnight UTC, ISO 8601).
const LifecycleDateLayout = "2006-01-02"

// s3Namespace is the XML namespace of the S3 documents.
const s3Namespace = "http://s3.amazonaws.com/doc/2006-03-01/"

// maxConfigDocument bounds a configuration document read by the import
// commands.
const maxConfigDocument = 1 << 20

// isXMLDocument reports a document whose first significant character opens
// an XML element.
func isXMLDocument(data []byte) bool {
	trimmed := bytes.TrimLeft(data, " \t\r\n\ufeff")
	return len(trimmed) > 0 && trimmed[0] == '<'
}

// ReadConfigDocument reads a configuration document (at most 1 MiB).
func ReadConfigDocument(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxConfigDocument+1))
	if err != nil {
		return nil, fmt.Errorf("read the configuration: %w", err)
	}
	if len(data) > maxConfigDocument {
		return nil, UsageError("the configuration is larger than %s", HumanSize(maxConfigDocument))
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, UsageError("the configuration is empty")
	}
	return data, nil
}

// decodeStrictJSON decodes a JSON document into v, refusing unknown
// members and trailing data.
func decodeStrictJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected data after the document")
	}
	return nil
}

// --- lifecycle -------------------------------------------------------------------

// lifecycleXML is the S3 LifecycleConfiguration document.
type lifecycleXML struct {
	XMLName xml.Name           `xml:"LifecycleConfiguration"`
	Xmlns   string             `xml:"xmlns,attr,omitempty"`
	Rules   []lifecycleRuleXML `xml:"Rule"`
}

type lifecycleRuleXML struct {
	ID string `xml:"ID,omitempty"`
	// Prefix is the legacy filter of the rule (without Filter).
	Prefix                *string                    `xml:"Prefix,omitempty"`
	Filter                *lifecycleFilterXML        `xml:"Filter,omitempty"`
	Status                string                     `xml:"Status"`
	Expiration            *lifecycleExpirationXML    `xml:"Expiration,omitempty"`
	Transitions           []lifecycleTransitionXML   `xml:"Transition,omitempty"`
	NoncurrentTransitions []lifecycleNoncurrentTrXML `xml:"NoncurrentVersionTransition,omitempty"`
	NoncurrentExpiration  *lifecycleNoncurrentXML    `xml:"NoncurrentVersionExpiration,omitempty"`
	AbortIncomplete       *lifecycleAbortXML         `xml:"AbortIncompleteMultipartUpload,omitempty"`
}

type lifecycleFilterXML struct {
	Prefix                *string          `xml:"Prefix,omitempty"`
	Tag                   *tagXML          `xml:"Tag,omitempty"`
	And                   *lifecycleAndXML `xml:"And,omitempty"`
	ObjectSizeGreaterThan *int64           `xml:"ObjectSizeGreaterThan,omitempty"`
	ObjectSizeLessThan    *int64           `xml:"ObjectSizeLessThan,omitempty"`
}

type lifecycleAndXML struct {
	Prefix                *string  `xml:"Prefix,omitempty"`
	Tags                  []tagXML `xml:"Tag,omitempty"`
	ObjectSizeGreaterThan *int64   `xml:"ObjectSizeGreaterThan,omitempty"`
	ObjectSizeLessThan    *int64   `xml:"ObjectSizeLessThan,omitempty"`
}

type tagXML struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

type lifecycleExpirationXML struct {
	Days                      *int    `xml:"Days,omitempty"`
	Date                      *string `xml:"Date,omitempty"`
	ExpiredObjectDeleteMarker *bool   `xml:"ExpiredObjectDeleteMarker,omitempty"`
}

type lifecycleNoncurrentXML struct {
	NoncurrentDays          *int `xml:"NoncurrentDays,omitempty"`
	NewerNoncurrentVersions *int `xml:"NewerNoncurrentVersions,omitempty"`
}

type lifecycleAbortXML struct {
	DaysAfterInitiation *int `xml:"DaysAfterInitiation,omitempty"`
}

type lifecycleTransitionXML struct {
	Days         *int    `xml:"Days,omitempty"`
	Date         *string `xml:"Date,omitempty"`
	StorageClass string  `xml:"StorageClass"`
}

type lifecycleNoncurrentTrXML struct {
	NoncurrentDays          *int   `xml:"NoncurrentDays,omitempty"`
	NewerNoncurrentVersions *int   `xml:"NewerNoncurrentVersions,omitempty"`
	StorageClass            string `xml:"StorageClass"`
}

// ParseLifecycle parses a lifecycle configuration: the JSON form of the
// Admin API ({"rules": [...]}) or the S3 XML document
// (<LifecycleConfiguration>). Malformed documents are usage errors.
func ParseLifecycle(data []byte) (LifecycleConfiguration, error) {
	if isXMLDocument(data) {
		var doc lifecycleXML
		if err := xml.Unmarshal(data, &doc); err != nil {
			return LifecycleConfiguration{}, UsageError("invalid lifecycle XML: %v", err)
		}
		return lifecycleFromXML(doc)
	}
	var cfg LifecycleConfiguration
	if err := decodeStrictJSON(data, &cfg); err != nil {
		return LifecycleConfiguration{}, UsageError("invalid lifecycle JSON: %v", err)
	}
	if cfg.Rules == nil {
		cfg.Rules = []LifecycleRule{}
	}
	for i, r := range cfg.Rules {
		if err := checkLifecycleDates(r); err != nil {
			return LifecycleConfiguration{}, UsageError("rule %d: %v", i+1, err)
		}
	}
	return cfg, nil
}

// checkLifecycleDates validates the dates of a rule.
func checkLifecycleDates(r LifecycleRule) error {
	var dates []string
	if r.Expiration != nil && r.Expiration.Date != "" {
		dates = append(dates, r.Expiration.Date)
	}
	for _, t := range r.Transitions {
		if t.Date != "" {
			dates = append(dates, t.Date)
		}
	}
	for _, d := range dates {
		if _, err := ParseLifecycleDate(d); err != nil {
			return err
		}
	}
	return nil
}

// ParseLifecycleDate parses the date of a lifecycle rule: YYYY-MM-DD, or
// an RFC 3339 time at midnight UTC. It returns the normalized
// YYYY-MM-DD form.
func ParseLifecycleDate(s string) (string, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(LifecycleDateLayout, s); err == nil {
		return t.Format(LifecycleDateLayout), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return "", UsageError("invalid date %q (YYYY-MM-DD)", s)
	}
	t = t.UTC()
	if t.Hour() != 0 || t.Minute() != 0 || t.Second() != 0 || t.Nanosecond() != 0 {
		return "", UsageError("date %q must be at midnight UTC", s)
	}
	return t.Format(LifecycleDateLayout), nil
}

func lifecycleFromXML(doc lifecycleXML) (LifecycleConfiguration, error) {
	cfg := LifecycleConfiguration{Rules: make([]LifecycleRule, 0, len(doc.Rules))}
	for i, rx := range doc.Rules {
		r := LifecycleRule{ID: rx.ID}
		switch strings.TrimSpace(rx.Status) {
		case "Enabled":
			r.Enabled = boolPtr(true)
		case "Disabled":
			r.Enabled = boolPtr(false)
		default:
			return LifecycleConfiguration{}, UsageError("rule %d: Status must be Enabled or Disabled", i+1)
		}
		r.Filter = filterFromXML(rx.Prefix, rx.Filter)
		if e := rx.Expiration; e != nil {
			exp := &LifecycleExpiration{Days: e.Days}
			if e.Date != nil {
				d, err := ParseLifecycleDate(*e.Date)
				if err != nil {
					return LifecycleConfiguration{}, UsageError("rule %d: %v", i+1, err)
				}
				exp.Date = d
			}
			if e.ExpiredObjectDeleteMarker != nil {
				exp.ExpiredObjectDeleteMarker = *e.ExpiredObjectDeleteMarker
			}
			r.Expiration = exp
		}
		if n := rx.NoncurrentExpiration; n != nil {
			r.NoncurrentExpiration = &LifecycleNoncurrentExpiration{NoncurrentDays: n.NoncurrentDays, NewerNoncurrentVersions: n.NewerNoncurrentVersions}
		}
		if a := rx.AbortIncomplete; a != nil {
			r.AbortIncompleteUpload = &LifecycleAbortUpload{DaysAfterInitiation: a.DaysAfterInitiation}
		}
		for _, t := range rx.Transitions {
			tr := LifecycleTransition{Days: t.Days, StorageClass: t.StorageClass}
			if t.Date != nil {
				d, err := ParseLifecycleDate(*t.Date)
				if err != nil {
					return LifecycleConfiguration{}, UsageError("rule %d: %v", i+1, err)
				}
				tr.Date = d
			}
			r.Transitions = append(r.Transitions, tr)
		}
		for _, t := range rx.NoncurrentTransitions {
			r.NoncurrentTransitions = append(r.NoncurrentTransitions, LifecycleNoncurrentTransition{
				NoncurrentDays: t.NoncurrentDays, NewerNoncurrentVersions: t.NewerNoncurrentVersions, StorageClass: t.StorageClass,
			})
		}
		cfg.Rules = append(cfg.Rules, r)
	}
	return cfg, nil
}

// filterFromXML converts the filter of a rule (the legacy Prefix of the
// rule, or its Filter).
func filterFromXML(legacy *string, f *lifecycleFilterXML) *LifecycleFilter {
	out := &LifecycleFilter{}
	switch {
	case f == nil && legacy != nil:
		out.Prefix = *legacy
	case f == nil:
		return nil
	case f.And != nil:
		if f.And.Prefix != nil {
			out.Prefix = *f.And.Prefix
		}
		for _, t := range f.And.Tags {
			if out.Tags == nil {
				out.Tags = map[string]string{}
			}
			out.Tags[t.Key] = t.Value
		}
		out.SizeGreaterThan, out.SizeLessThan = f.And.ObjectSizeGreaterThan, f.And.ObjectSizeLessThan
	default:
		if f.Prefix != nil {
			out.Prefix = *f.Prefix
		}
		if f.Tag != nil {
			out.Tags = map[string]string{f.Tag.Key: f.Tag.Value}
		}
		out.SizeGreaterThan, out.SizeLessThan = f.ObjectSizeGreaterThan, f.ObjectSizeLessThan
	}
	if out.Prefix == "" && len(out.Tags) == 0 && out.SizeGreaterThan == nil && out.SizeLessThan == nil {
		return nil
	}
	return out
}

// EncodeLifecycleXML renders a lifecycle configuration as the S3 XML
// document.
func EncodeLifecycleXML(cfg LifecycleConfiguration) ([]byte, error) {
	doc := lifecycleXML{Xmlns: s3Namespace}
	for _, r := range cfg.Rules {
		rx := lifecycleRuleXML{ID: r.ID, Status: "Enabled", Filter: filterToXML(r.Filter)}
		if r.Enabled != nil && !*r.Enabled {
			rx.Status = "Disabled"
		}
		if e := r.Expiration; e != nil {
			ex := &lifecycleExpirationXML{Days: e.Days}
			if e.Date != "" {
				ex.Date = stringPtr(lifecycleDateTime(e.Date))
			}
			if e.ExpiredObjectDeleteMarker {
				ex.ExpiredObjectDeleteMarker = boolPtr(true)
			}
			rx.Expiration = ex
		}
		if n := r.NoncurrentExpiration; n != nil {
			rx.NoncurrentExpiration = &lifecycleNoncurrentXML{NoncurrentDays: n.NoncurrentDays, NewerNoncurrentVersions: n.NewerNoncurrentVersions}
		}
		if a := r.AbortIncompleteUpload; a != nil {
			rx.AbortIncomplete = &lifecycleAbortXML{DaysAfterInitiation: a.DaysAfterInitiation}
		}
		for _, t := range r.Transitions {
			tx := lifecycleTransitionXML{Days: t.Days, StorageClass: t.StorageClass}
			if t.Date != "" {
				tx.Date = stringPtr(lifecycleDateTime(t.Date))
			}
			rx.Transitions = append(rx.Transitions, tx)
		}
		for _, t := range r.NoncurrentTransitions {
			rx.NoncurrentTransitions = append(rx.NoncurrentTransitions, lifecycleNoncurrentTrXML{
				NoncurrentDays: t.NoncurrentDays, NewerNoncurrentVersions: t.NewerNoncurrentVersions, StorageClass: t.StorageClass,
			})
		}
		doc.Rules = append(doc.Rules, rx)
	}
	return encodeXML(doc)
}

// lifecycleDateTime renders a YYYY-MM-DD date as the midnight UTC time of
// the S3 documents.
func lifecycleDateTime(date string) string {
	t, err := time.Parse(LifecycleDateLayout, date)
	if err != nil {
		return date
	}
	return t.UTC().Format(time.RFC3339)
}

// filterToXML renders the filter of a rule: always a Filter element (an
// empty one selects every object).
func filterToXML(f *LifecycleFilter) *lifecycleFilterXML {
	if f == nil {
		return &lifecycleFilterXML{Prefix: stringPtr("")}
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
	tags := make([]tagXML, 0, len(f.Tags))
	for _, k := range SortedKeys(f.Tags) {
		tags = append(tags, tagXML{Key: k, Value: f.Tags[k]})
	}
	switch {
	case conditions > 1:
		and := &lifecycleAndXML{Tags: tags, ObjectSizeGreaterThan: f.SizeGreaterThan, ObjectSizeLessThan: f.SizeLessThan}
		if f.Prefix != "" {
			and.Prefix = stringPtr(f.Prefix)
		}
		return &lifecycleFilterXML{And: and}
	case len(tags) == 1:
		return &lifecycleFilterXML{Tag: &tags[0]}
	case f.SizeGreaterThan != nil || f.SizeLessThan != nil:
		return &lifecycleFilterXML{ObjectSizeGreaterThan: f.SizeGreaterThan, ObjectSizeLessThan: f.SizeLessThan}
	default:
		return &lifecycleFilterXML{Prefix: stringPtr(f.Prefix)}
	}
}

// RuleEnabled reports whether a lifecycle rule is enabled (nil means
// enabled).
func RuleEnabled(r LifecycleRule) bool { return r.Enabled == nil || *r.Enabled }

// NextLifecycleRuleID returns the first "rule-N" not used by the rules.
func NextLifecycleRuleID(rules []LifecycleRule) string {
	used := make(map[string]bool, len(rules))
	for _, r := range rules {
		used[r.ID] = true
	}
	for n := 1; ; n++ {
		id := fmt.Sprintf("rule-%d", n)
		if !used[id] {
			return id
		}
	}
}

// --- CORS --------------------------------------------------------------------

// corsXML is the S3 CORSConfiguration document.
type corsXML struct {
	XMLName xml.Name      `xml:"CORSConfiguration"`
	Xmlns   string        `xml:"xmlns,attr,omitempty"`
	Rules   []corsRuleXML `xml:"CORSRule"`
}

type corsRuleXML struct {
	ID             string   `xml:"ID,omitempty"`
	AllowedOrigins []string `xml:"AllowedOrigin"`
	AllowedMethods []string `xml:"AllowedMethod"`
	AllowedHeaders []string `xml:"AllowedHeader,omitempty"`
	ExposeHeaders  []string `xml:"ExposeHeader,omitempty"`
	MaxAgeSeconds  *int     `xml:"MaxAgeSeconds,omitempty"`
}

// corsJSON accepts the JSON form of the Admin API ({"rules": [...]}) and
// the one of the AWS CLI ({"CORSRules": [...]}): the member names of the
// rules match case-insensitively.
type corsJSON struct {
	Rules     []CORSRule `json:"rules"`
	CORSRules []CORSRule `json:"CORSRules"`
}

// ParseCORS parses a CORS configuration: the JSON form of the Admin API,
// the one of the AWS CLI or the S3 XML document (<CORSConfiguration>). The
// rules are validated as the server does (domain.CORSConfiguration).
func ParseCORS(data []byte) (CORSConfiguration, error) {
	var cfg CORSConfiguration
	if isXMLDocument(data) {
		var doc corsXML
		if err := xml.Unmarshal(data, &doc); err != nil {
			return CORSConfiguration{}, UsageError("invalid CORS XML: %v", err)
		}
		for _, r := range doc.Rules {
			cfg.Rules = append(cfg.Rules, CORSRule{
				ID: r.ID, AllowedOrigins: r.AllowedOrigins, AllowedMethods: r.AllowedMethods,
				AllowedHeaders: r.AllowedHeaders, ExposeHeaders: r.ExposeHeaders, MaxAgeSeconds: r.MaxAgeSeconds,
			})
		}
	} else {
		var doc corsJSON
		if err := decodeStrictJSON(data, &doc); err != nil {
			return CORSConfiguration{}, UsageError("invalid CORS JSON: %v", err)
		}
		cfg.Rules = append(doc.Rules, doc.CORSRules...)
	}
	if cfg.Rules == nil {
		cfg.Rules = []CORSRule{}
	}
	if len(cfg.Rules) == 0 {
		return CORSConfiguration{}, UsageError("the CORS configuration has no rule (use \"cors rm\" to remove it)")
	}
	if err := CORSDomain(cfg).Validate(); err != nil {
		return CORSConfiguration{}, UsageError("invalid CORS configuration: %v", err)
	}
	return cfg, nil
}

// CORSDomain converts a CORS configuration to the domain type (shared with
// the server validation).
func CORSDomain(cfg CORSConfiguration) domain.CORSConfiguration {
	out := domain.CORSConfiguration{Rules: make([]domain.CORSRule, 0, len(cfg.Rules))}
	for _, r := range cfg.Rules {
		out.Rules = append(out.Rules, domain.CORSRule{
			ID: r.ID, AllowedOrigins: r.AllowedOrigins, AllowedMethods: r.AllowedMethods,
			AllowedHeaders: r.AllowedHeaders, ExposeHeaders: r.ExposeHeaders, MaxAgeSeconds: r.MaxAgeSeconds,
		})
	}
	return out
}

// EncodeCORSXML renders a CORS configuration as the S3 XML document.
func EncodeCORSXML(cfg CORSConfiguration) ([]byte, error) {
	doc := corsXML{Xmlns: s3Namespace}
	for _, r := range cfg.Rules {
		doc.Rules = append(doc.Rules, corsRuleXML{
			ID: r.ID, AllowedOrigins: r.AllowedOrigins, AllowedMethods: r.AllowedMethods,
			AllowedHeaders: r.AllowedHeaders, ExposeHeaders: r.ExposeHeaders, MaxAgeSeconds: r.MaxAgeSeconds,
		})
	}
	return encodeXML(doc)
}

// --- notifications ------------------------------------------------------------

// Short names of the event types of "event add --event" (as mc).
const (
	EventPut     = "put"
	EventDelete  = "delete"
	EventGet     = "get"
	EventReplica = "replica"
	EventILM     = "ilm"
)

// eventShortNames maps the short event names to the S3 event types.
func eventShortNames() map[string][]string {
	return map[string][]string{
		EventPut:     {"s3:ObjectCreated:*"},
		EventDelete:  {"s3:ObjectRemoved:*"},
		EventGet:     {"s3:ObjectAccessed:*"},
		EventReplica: {"s3:Replication:*"},
		EventILM:     {"s3:LifecycleExpiration:*", "s3:LifecycleTransition"},
	}
}

// ParseEventTypes expands a comma-separated list of event types: the short
// names put, delete, get, replica and ilm, or S3 event types
// ("s3:ObjectCreated:Put"). The result is sorted and without duplicates.
func ParseEventTypes(s string) ([]string, error) {
	short := eventShortNames()
	seen := map[string]bool{}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if names, ok := short[strings.ToLower(item)]; ok {
			for _, n := range names {
				seen[n] = true
			}
			continue
		}
		if !strings.HasPrefix(item, "s3:") || strings.Count(item, ":") < 1 {
			return nil, UsageError("unknown event %q (put, delete, get, replica, ilm or an S3 event type such as s3:ObjectCreated:Put)", item)
		}
		seen[item] = true
	}
	if len(seen) == 0 {
		return nil, UsageError("no event type (put, delete, get, ...)")
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// ARNService returns the service of a notification ARN
// (arn:<partition>:<service>:...): sqs, sns or lambda.
func ARNService(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

// ValidateARN checks the form of a notification target ARN:
// arn:<partition>:<sqs|sns|lambda>:<region>:<id>:<type> for Nexo, any
// arn:...:<sqs|sns|lambda>:... elsewhere.
func ValidateARN(arn string) error {
	parts := strings.Split(arn, ":")
	if len(parts) < 6 || parts[0] != "arn" || parts[1] == "" {
		return UsageError("invalid target ARN %q (arn:nexo:sqs:REGION:ID:TYPE, see \"nexoctl admin notify targets ls\")", arn)
	}
	switch parts[2] {
	case "sqs", "sns", "lambda":
		return nil
	}
	return UsageError("invalid target ARN %q: the service must be sqs, sns or lambda", arn)
}

// SameEvents reports whether two lists hold the same event types.
func SameEvents(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// --- helpers -----------------------------------------------------------------------

// encodeXML renders an XML document with its header, indented.
func encodeXML(v any) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encode XML: %w", err)
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

func stringPtr(s string) *string { return &s }

func boolPtr(b bool) *bool { return &b }
