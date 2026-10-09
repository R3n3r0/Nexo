package ilm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Limits of a lifecycle configuration (S3).
const (
	maxRules                   = 1000
	maxRuleIDLength            = 255
	maxPrefixLength            = 1024
	maxTagKeyLength            = 128
	maxTagValueLength          = 256
	maxNewerNoncurrentVersions = 100
	s3Namespace                = "http://s3.amazonaws.com/doc/2006-03-01/"
	statusEnabled              = "Enabled"
	statusDisabled             = "Disabled"
)

// Configuration is a parsed LifecycleConfiguration.
type Configuration struct {
	Rules []Rule
}

// Rule is one lifecycle rule.
type Rule struct {
	ID      string
	Enabled bool
	Filter  Filter
	// Expiration expires the current versions (or removes the expired
	// object delete markers).
	Expiration *Expiration
	// NoncurrentExpiration removes the noncurrent versions.
	NoncurrentExpiration *NoncurrentExpiration
	// AbortIncompleteUpload aborts the incomplete multipart uploads.
	AbortIncompleteUpload *AbortIncompleteUpload
	// Transitions and NoncurrentTransitions move versions to a remote
	// tier (P2): accepted only for the storage classes of a configured
	// tier (WithStorageClasses), not applied yet.
	Transitions           []Transition
	NoncurrentTransitions []NoncurrentTransition
}

// Filter selects the objects of a rule: every set criterion must match.
type Filter struct {
	Prefix string
	// Legacy reports a rule written with the deprecated top-level Prefix
	// instead of a Filter: the stored document keeps that form, as S3
	// returns the rules as they were set.
	Legacy bool
	Tags   map[string]string
	// SizeGreaterThan and SizeLessThan bound the object size (exclusive);
	// nil means no bound.
	SizeGreaterThan *int64
	SizeLessThan    *int64
}

// Expiration is the Expiration action: Days after the creation, on Date,
// or the removal of the expired object delete markers.
type Expiration struct {
	Days                      int
	Date                      time.Time
	ExpiredObjectDeleteMarker bool
}

// NoncurrentExpiration removes the noncurrent versions NoncurrentDays after
// they became noncurrent, keeping the NewerNoncurrentVersions newest ones.
type NoncurrentExpiration struct {
	NoncurrentDays          int
	NewerNoncurrentVersions int
}

// AbortIncompleteUpload aborts the uploads DaysAfterInitiation after they
// started.
type AbortIncompleteUpload struct {
	DaysAfterInitiation int
}

// Transition moves the current versions to StorageClass.
type Transition struct {
	Days         int
	Date         time.Time
	StorageClass string
}

// NoncurrentTransition moves the noncurrent versions to StorageClass.
type NoncurrentTransition struct {
	NoncurrentDays          int
	NewerNoncurrentVersions int
	StorageClass            string
}

// XML documents of the lifecycle configuration.
type lifecycleXML struct {
	XMLName xml.Name  `xml:"LifecycleConfiguration"`
	Xmlns   string    `xml:"xmlns,attr,omitempty"`
	Rules   []ruleXML `xml:"Rule"`
}

type ruleXML struct {
	ID                    string                    `xml:"ID,omitempty"`
	Filter                *filterXML                `xml:"Filter,omitempty"`
	Prefix                *string                   `xml:"Prefix,omitempty"`
	Status                string                    `xml:"Status"`
	Expiration            *expirationXML            `xml:"Expiration,omitempty"`
	Transitions           []transitionXML           `xml:"Transition,omitempty"`
	NoncurrentTransitions []noncurrentTransitionXML `xml:"NoncurrentVersionTransition,omitempty"`
	NoncurrentExpiration  *noncurrentExpirationXML  `xml:"NoncurrentVersionExpiration,omitempty"`
	AbortIncomplete       *abortXML                 `xml:"AbortIncompleteMultipartUpload,omitempty"`
}

type filterXML struct {
	Prefix                *string `xml:"Prefix,omitempty"`
	Tag                   *tagXML `xml:"Tag,omitempty"`
	And                   *andXML `xml:"And,omitempty"`
	ObjectSizeGreaterThan *int64  `xml:"ObjectSizeGreaterThan,omitempty"`
	ObjectSizeLessThan    *int64  `xml:"ObjectSizeLessThan,omitempty"`
}

type andXML struct {
	Prefix                *string  `xml:"Prefix,omitempty"`
	Tags                  []tagXML `xml:"Tag,omitempty"`
	ObjectSizeGreaterThan *int64   `xml:"ObjectSizeGreaterThan,omitempty"`
	ObjectSizeLessThan    *int64   `xml:"ObjectSizeLessThan,omitempty"`
}

type tagXML struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

type expirationXML struct {
	Days                      *int    `xml:"Days,omitempty"`
	Date                      *string `xml:"Date,omitempty"`
	ExpiredObjectDeleteMarker *bool   `xml:"ExpiredObjectDeleteMarker,omitempty"`
}

type noncurrentExpirationXML struct {
	NoncurrentDays          *int `xml:"NoncurrentDays,omitempty"`
	NewerNoncurrentVersions *int `xml:"NewerNoncurrentVersions,omitempty"`
}

type abortXML struct {
	DaysAfterInitiation *int `xml:"DaysAfterInitiation,omitempty"`
}

type transitionXML struct {
	Days         *int    `xml:"Days,omitempty"`
	Date         *string `xml:"Date,omitempty"`
	StorageClass string  `xml:"StorageClass"`
}

type noncurrentTransitionXML struct {
	NoncurrentDays          *int   `xml:"NoncurrentDays,omitempty"`
	NewerNoncurrentVersions *int   `xml:"NewerNoncurrentVersions,omitempty"`
	StorageClass            string `xml:"StorageClass"`
}

// Errors of the lifecycle documents.
func malformed(format string, args ...any) error {
	msg := "The XML you provided was not well-formed or did not validate against our published schema"
	if format != "" {
		msg += ": " + fmt.Sprintf(format, args...)
	}
	return domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML", msg)
}

func invalidArgument(format string, args ...any) error {
	return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", fmt.Sprintf(format, args...))
}

func invalidRequest(format string, args ...any) error {
	return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest", fmt.Sprintf(format, args...))
}

// parseConfiguration decodes and validates a document; tiers are the
// storage classes the transitions may name.
func parseConfiguration(doc []byte, tiers map[string]bool) (*Configuration, error) {
	var x lifecycleXML
	if err := xml.NewDecoder(bytes.NewReader(doc)).Decode(&x); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, malformed("")
		}
		return nil, malformed("%v", err)
	}
	if len(x.Rules) == 0 {
		return nil, malformed("a lifecycle configuration needs at least one Rule")
	}
	if len(x.Rules) > maxRules {
		return nil, invalidArgument("A lifecycle configuration cannot have more than %d rules", maxRules)
	}
	cfg := &Configuration{Rules: make([]Rule, 0, len(x.Rules))}
	ids := map[string]bool{}
	for _, rx := range x.Rules {
		r, err := parseRule(rx, tiers)
		if err != nil {
			return nil, err
		}
		if r.ID != "" {
			if ids[r.ID] {
				return nil, invalidArgument("Rule ID must be unique. Found same ID for more than one rule: %q", r.ID)
			}
			ids[r.ID] = true
		}
		cfg.Rules = append(cfg.Rules, r)
	}
	for i := range cfg.Rules {
		if cfg.Rules[i].ID != "" {
			continue
		}
		base := ruleID(cfg.Rules[i])
		id := base
		for n := 1; ids[id]; n++ {
			id = fmt.Sprintf("%s-%d", base, n)
		}
		ids[id] = true
		cfg.Rules[i].ID = id
	}
	return cfg, nil
}

// parseRule validates one rule.
func parseRule(rx ruleXML, tiers map[string]bool) (Rule, error) {
	r := Rule{ID: strings.TrimSpace(rx.ID)}
	if len(r.ID) > maxRuleIDLength {
		return Rule{}, invalidArgument("ID length should not exceed allowed limit of %d", maxRuleIDLength)
	}
	switch rx.Status {
	case statusEnabled:
		r.Enabled = true
	case statusDisabled:
	default:
		return Rule{}, malformed("Status must be %s or %s", statusEnabled, statusDisabled)
	}
	filter, err := parseFilter(rx)
	if err != nil {
		return Rule{}, err
	}
	r.Filter = filter
	if e := rx.Expiration; e != nil {
		exp, err := parseExpiration(*e, filter)
		if err != nil {
			return Rule{}, err
		}
		r.Expiration = exp
	}
	if n := rx.NoncurrentExpiration; n != nil {
		if n.NoncurrentDays == nil || *n.NoncurrentDays <= 0 {
			return Rule{}, invalidArgument("'NoncurrentDays' in NoncurrentVersionExpiration action must be a positive integer")
		}
		ne := &NoncurrentExpiration{NoncurrentDays: *n.NoncurrentDays}
		if n.NewerNoncurrentVersions != nil {
			if v := *n.NewerNoncurrentVersions; v <= 0 || v > maxNewerNoncurrentVersions {
				return Rule{}, invalidArgument("'NewerNoncurrentVersions' must be between 1 and %d", maxNewerNoncurrentVersions)
			}
			ne.NewerNoncurrentVersions = *n.NewerNoncurrentVersions
		}
		r.NoncurrentExpiration = ne
	}
	if a := rx.AbortIncomplete; a != nil {
		if a.DaysAfterInitiation == nil || *a.DaysAfterInitiation <= 0 {
			return Rule{}, invalidArgument("'DaysAfterInitiation' for AbortIncompleteMultipartUpload action must be a positive integer")
		}
		if len(filter.Tags) > 0 || filter.SizeGreaterThan != nil || filter.SizeLessThan != nil {
			return Rule{}, invalidRequest("AbortIncompleteMultipartUpload cannot be specified with Tags or object size filters")
		}
		r.AbortIncompleteUpload = &AbortIncompleteUpload{DaysAfterInitiation: *a.DaysAfterInitiation}
	}
	for _, t := range rx.Transitions {
		tr, err := parseTransition(t, tiers)
		if err != nil {
			return Rule{}, err
		}
		r.Transitions = append(r.Transitions, tr)
	}
	for _, t := range rx.NoncurrentTransitions {
		if t.NoncurrentDays == nil || *t.NoncurrentDays < 0 {
			return Rule{}, invalidArgument("'NoncurrentDays' in NoncurrentVersionTransition action must be a non-negative integer")
		}
		if err := checkTier(t.StorageClass, tiers); err != nil {
			return Rule{}, err
		}
		nt := NoncurrentTransition{NoncurrentDays: *t.NoncurrentDays, StorageClass: t.StorageClass}
		if t.NewerNoncurrentVersions != nil {
			nt.NewerNoncurrentVersions = *t.NewerNoncurrentVersions
		}
		r.NoncurrentTransitions = append(r.NoncurrentTransitions, nt)
	}
	if r.Expiration == nil && r.NoncurrentExpiration == nil && r.AbortIncompleteUpload == nil &&
		len(r.Transitions) == 0 && len(r.NoncurrentTransitions) == 0 {
		return Rule{}, invalidRequest("At least one action needs to be specified in a rule")
	}
	return r, nil
}

// parseFilter validates the Filter of a rule, or its legacy Prefix.
func parseFilter(rx ruleXML) (Filter, error) {
	if rx.Filter != nil && rx.Prefix != nil {
		return Filter{}, malformed("a rule cannot have both a Filter and a Prefix")
	}
	if rx.Filter == nil {
		// As S3: a rule names the objects it applies to, if only with an
		// empty prefix (a typo must not expire a whole bucket).
		if rx.Prefix == nil {
			return Filter{}, malformed("a rule must have a Filter or a Prefix")
		}
		f := Filter{Legacy: true, Prefix: *rx.Prefix}
		return f, checkPrefix(f.Prefix)
	}
	fx := rx.Filter
	set := 0
	for _, present := range []bool{fx.Prefix != nil, fx.Tag != nil, fx.And != nil, fx.ObjectSizeGreaterThan != nil, fx.ObjectSizeLessThan != nil} {
		if present {
			set++
		}
	}
	if set > 1 {
		return Filter{}, malformed("Filter must have exactly one of Prefix, Tag, And, ObjectSizeGreaterThan or ObjectSizeLessThan")
	}
	f := Filter{SizeGreaterThan: fx.ObjectSizeGreaterThan, SizeLessThan: fx.ObjectSizeLessThan}
	if fx.Prefix != nil {
		f.Prefix = *fx.Prefix
	}
	var tags []tagXML
	if fx.Tag != nil {
		tags = append(tags, *fx.Tag)
	}
	if a := fx.And; a != nil {
		if a.Prefix != nil {
			f.Prefix = *a.Prefix
		}
		tags = append(tags, a.Tags...)
		f.SizeGreaterThan, f.SizeLessThan = a.ObjectSizeGreaterThan, a.ObjectSizeLessThan
	}
	if err := checkPrefix(f.Prefix); err != nil {
		return Filter{}, err
	}
	for _, t := range tags {
		if t.Key == "" || len(t.Key) > maxTagKeyLength || len(t.Value) > maxTagValueLength {
			return Filter{}, invalidArgument("The TagKey or TagValue you have provided is invalid")
		}
		if f.Tags == nil {
			f.Tags = map[string]string{}
		}
		if _, dup := f.Tags[t.Key]; dup {
			return Filter{}, invalidArgument("Duplicate Tag Keys are not allowed")
		}
		f.Tags[t.Key] = t.Value
	}
	for _, v := range []*int64{f.SizeGreaterThan, f.SizeLessThan} {
		if v != nil && *v < 0 {
			return Filter{}, invalidArgument("Object size filters must be non-negative")
		}
	}
	if f.SizeGreaterThan != nil && f.SizeLessThan != nil && *f.SizeGreaterThan >= *f.SizeLessThan {
		return Filter{}, invalidArgument("ObjectSizeGreaterThan must be less than ObjectSizeLessThan")
	}
	return f, nil
}

func checkPrefix(p string) error {
	if len(p) > maxPrefixLength {
		return invalidArgument("The prefix cannot be longer than %d characters", maxPrefixLength)
	}
	return nil
}

// parseExpiration validates an Expiration action.
func parseExpiration(e expirationXML, f Filter) (*Expiration, error) {
	exp := &Expiration{}
	set := 0
	if e.Days != nil {
		set++
		if *e.Days <= 0 {
			return nil, invalidArgument("'Days' for Expiration action must be a positive integer")
		}
		exp.Days = *e.Days
	}
	if e.Date != nil {
		set++
		d, err := parseDate(*e.Date)
		if err != nil {
			return nil, err
		}
		exp.Date = d
	}
	if e.ExpiredObjectDeleteMarker != nil {
		if *e.ExpiredObjectDeleteMarker {
			set++
			exp.ExpiredObjectDeleteMarker = true
			if len(f.Tags) > 0 {
				return nil, invalidRequest("ExpiredObjectDeleteMarker cannot be specified with Tags")
			}
		}
	}
	if set > 1 {
		return nil, invalidRequest("Expiration must have exactly one of Days, Date or ExpiredObjectDeleteMarker")
	}
	if set == 0 {
		return nil, nil
	}
	return exp, nil
}

// parseTransition validates a Transition action.
func parseTransition(t transitionXML, tiers map[string]bool) (Transition, error) {
	tr := Transition{StorageClass: t.StorageClass}
	if (t.Days == nil) == (t.Date == nil) {
		return Transition{}, invalidRequest("Transition must have exactly one of Days or Date")
	}
	if t.Days != nil {
		if *t.Days < 0 {
			return Transition{}, invalidArgument("'Days' for Transition action must be a non-negative integer")
		}
		tr.Days = *t.Days
	}
	if t.Date != nil {
		d, err := parseDate(*t.Date)
		if err != nil {
			return Transition{}, err
		}
		tr.Date = d
	}
	return tr, checkTier(t.StorageClass, tiers)
}

// checkTier refuses the transitions to a storage class without tier: the
// remote tiers are P2.
func checkTier(class string, tiers map[string]bool) error {
	if class == "" {
		return malformed("a transition needs a StorageClass")
	}
	if !tiers[class] {
		return domain.NewS3Error(domain.ErrNotImplemented, "NotImplemented",
			fmt.Sprintf("Transition to the storage class %q is not supported: no remote tier is configured", class))
	}
	return nil
}

// parseDate parses an ISO 8601 date at midnight UTC.
func parseDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	var t time.Time
	var err error
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z", "2006-01-02"} {
		if t, err = time.Parse(layout, s); err == nil {
			break
		}
	}
	if err != nil {
		return time.Time{}, invalidArgument("'Date' must be an ISO 8601 date: %q", s)
	}
	t = t.UTC()
	if !t.Equal(t.Truncate(24 * time.Hour)) {
		return time.Time{}, invalidArgument("'Date' must be at midnight GMT")
	}
	return t, nil
}

// ruleID derives a stable id from the content of a rule without one.
func ruleID(r Rule) string {
	data, _ := json.Marshal(r)
	sum := sha256.Sum256(data)
	return "nexo-" + hex.EncodeToString(sum[:])[:16]
}

// encodeConfiguration returns the canonical XML document of cfg.
func encodeConfiguration(cfg *Configuration) ([]byte, error) {
	x := lifecycleXML{Xmlns: s3Namespace}
	for _, r := range cfg.Rules {
		rx := ruleXML{ID: r.ID, Status: statusDisabled}
		if r.Filter.Legacy {
			rx.Prefix = ptr(r.Filter.Prefix)
		} else {
			rx.Filter = encodeFilter(r.Filter)
		}
		if r.Enabled {
			rx.Status = statusEnabled
		}
		if e := r.Expiration; e != nil {
			ex := &expirationXML{}
			switch {
			case e.Days > 0:
				ex.Days = ptr(e.Days)
			case !e.Date.IsZero():
				ex.Date = ptr(e.Date.UTC().Format(time.RFC3339))
			case e.ExpiredObjectDeleteMarker:
				ex.ExpiredObjectDeleteMarker = ptr(true)
			}
			rx.Expiration = ex
		}
		if n := r.NoncurrentExpiration; n != nil {
			nx := &noncurrentExpirationXML{NoncurrentDays: ptr(n.NoncurrentDays)}
			if n.NewerNoncurrentVersions > 0 {
				nx.NewerNoncurrentVersions = ptr(n.NewerNoncurrentVersions)
			}
			rx.NoncurrentExpiration = nx
		}
		if a := r.AbortIncompleteUpload; a != nil {
			rx.AbortIncomplete = &abortXML{DaysAfterInitiation: ptr(a.DaysAfterInitiation)}
		}
		for _, t := range r.Transitions {
			tx := transitionXML{StorageClass: t.StorageClass}
			if t.Date.IsZero() {
				tx.Days = ptr(t.Days)
			} else {
				tx.Date = ptr(t.Date.UTC().Format(time.RFC3339))
			}
			rx.Transitions = append(rx.Transitions, tx)
		}
		for _, t := range r.NoncurrentTransitions {
			tx := noncurrentTransitionXML{NoncurrentDays: ptr(t.NoncurrentDays), StorageClass: t.StorageClass}
			if t.NewerNoncurrentVersions > 0 {
				tx.NewerNoncurrentVersions = ptr(t.NewerNoncurrentVersions)
			}
			rx.NoncurrentTransitions = append(rx.NoncurrentTransitions, tx)
		}
		x.Rules = append(x.Rules, rx)
	}
	out, err := xml.Marshal(x)
	if err != nil {
		return nil, fmt.Errorf("ilm: encode lifecycle configuration: %w", err)
	}
	return append([]byte(xml.Header), out...), nil
}

// encodeFilter returns the Filter element of a filter: the simplest form
// that expresses it.
func encodeFilter(f Filter) *filterXML {
	conditions := 0
	if f.Prefix != "" {
		conditions++
	}
	conditions += len(f.Tags)
	if f.SizeGreaterThan != nil {
		conditions++
	}
	if f.SizeLessThan != nil {
		conditions++
	}
	tags := sortedTags(f.Tags)
	switch {
	case conditions == 0:
		return &filterXML{Prefix: ptr("")}
	case conditions > 1:
		a := &andXML{Tags: tags, ObjectSizeGreaterThan: f.SizeGreaterThan, ObjectSizeLessThan: f.SizeLessThan}
		if f.Prefix != "" {
			a.Prefix = ptr(f.Prefix)
		}
		return &filterXML{And: a}
	case f.Prefix != "":
		return &filterXML{Prefix: ptr(f.Prefix)}
	case len(tags) == 1:
		return &filterXML{Tag: &tags[0]}
	default:
		return &filterXML{ObjectSizeGreaterThan: f.SizeGreaterThan, ObjectSizeLessThan: f.SizeLessThan}
	}
}

// sortedTags returns the tags sorted by key.
func sortedTags(tags map[string]string) []tagXML {
	out := make([]tagXML, 0, len(tags))
	for k, v := range tags {
		out = append(out, tagXML{Key: k, Value: v})
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Key < out[j-1].Key; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func ptr[T any](v T) *T { return &v }
