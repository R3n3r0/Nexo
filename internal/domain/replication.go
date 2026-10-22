package domain

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Bucket replication (S-BKT-12): the ReplicationConfiguration document of
// a bucket, its rules and the replication state of the versions.

// ReplicationStatus is the replication state of a version
// (x-amz-replication-status).
type ReplicationStatus string

// Replication states. A version written on a bucket with a rule that
// selects it is PENDING until every destination of its rules has it
// (COMPLETED); a failed attempt makes it FAILED until a retry succeeds. A
// version written by the replication of another site is a REPLICA: it is
// never replicated again (no loop between two sites replicating to each
// other).
const (
	ReplicationPending   ReplicationStatus = "PENDING"
	ReplicationCompleted ReplicationStatus = "COMPLETED"
	ReplicationFailed    ReplicationStatus = "FAILED"
	ReplicationReplica   ReplicationStatus = "REPLICA"
)

// ValidReplicationStatus reports a known state.
func ValidReplicationStatus(s ReplicationStatus) bool {
	switch s {
	case ReplicationPending, ReplicationCompleted, ReplicationFailed, ReplicationReplica:
		return true
	}
	return false
}

// OverallReplicationStatus is the state of a version replicated to
// several destinations: FAILED when one failed, PENDING while one is
// pending, COMPLETED when all completed ("" for none).
func OverallReplicationStatus(states map[string]ReplicationStatus) ReplicationStatus {
	if len(states) == 0 {
		return ""
	}
	out := ReplicationCompleted
	for _, s := range states {
		switch s {
		case ReplicationFailed:
			return ReplicationFailed
		case ReplicationPending:
			out = ReplicationPending
		}
	}
	return out
}

// Headers of the writes of the replication: they ask the destination to
// store the version with the id and the time it has at the source and to
// mark it as a replica (s3:ReplicateObject, s3:ReplicateDelete).
const (
	// HeaderReplica ("true") marks every request of the replication.
	HeaderReplica             = "X-Nexo-Replica"
	HeaderReplicaVersionID    = "X-Nexo-Replica-Version-Id"
	HeaderReplicaModTime      = "X-Nexo-Replica-Mtime"
	HeaderReplicaETag         = "X-Nexo-Replica-Etag"
	HeaderReplicaDeleteMarker = "X-Nexo-Replica-Delete-Marker"
)

// ReplicaModTimeLayout formats HeaderReplicaModTime.
const ReplicaModTimeLayout = time.RFC3339Nano

// Limits of a replication configuration.
const (
	MaxReplicationRules  = 1000
	maxReplicationRuleID = 255
)

// ReplicationRuleStatus enables or disables a rule or one of its options.
const (
	ReplicationEnabled  = "Enabled"
	ReplicationDisabled = "Disabled"
)

// ReplicationConfig is a parsed ReplicationConfiguration document.
type ReplicationConfig struct {
	XMLName xml.Name          `xml:"ReplicationConfiguration"`
	Role    string            `xml:"Role,omitempty"`
	Rules   []ReplicationRule `xml:"Rule"`
}

// ReplicationRule is one rule: the versions it selects (Filter, or the
// Prefix of the first version of the schema) go to Destination.
type ReplicationRule struct {
	ID                        string                     `xml:"ID,omitempty"`
	Priority                  int                        `xml:"Priority,omitempty"`
	Status                    string                     `xml:"Status"`
	Prefix                    *string                    `xml:"Prefix,omitempty"`
	Filter                    *ReplicationFilter         `xml:"Filter,omitempty"`
	DeleteMarkerReplication   *ReplicationToggle         `xml:"DeleteMarkerReplication,omitempty"`
	ExistingObjectReplication *ReplicationToggle         `xml:"ExistingObjectReplication,omitempty"`
	DeleteReplication         *ReplicationToggle         `xml:"DeleteReplication,omitempty"`
	Destination               ReplicationRuleDestination `xml:"Destination"`
}

// ReplicationToggle is an <...><Status>Enabled|Disabled</Status></...> option.
type ReplicationToggle struct {
	Status string `xml:"Status"`
}

// ReplicationFilter selects the keys of a rule: a prefix, a tag, or both
// (And).
type ReplicationFilter struct {
	Prefix *string               `xml:"Prefix,omitempty"`
	Tag    *ReplicationTag       `xml:"Tag,omitempty"`
	And    *ReplicationFilterAnd `xml:"And,omitempty"`
}

// ReplicationFilterAnd is the conjunction of a prefix and tags.
type ReplicationFilterAnd struct {
	Prefix string           `xml:"Prefix,omitempty"`
	Tags   []ReplicationTag `xml:"Tag"`
}

// ReplicationTag is a tag of a filter.
type ReplicationTag struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

// ReplicationRuleDestination is where a rule replicates: Bucket is the ARN
// of a replication target of the bucket.
type ReplicationRuleDestination struct {
	Bucket       string `xml:"Bucket"`
	StorageClass string `xml:"StorageClass,omitempty"`
}

// Enabled reports an enabled rule.
func (r ReplicationRule) Enabled() bool { return r.Status == ReplicationEnabled }

// ReplicatesDeleteMarkers reports whether the delete markers of the
// selected keys are replicated.
func (r ReplicationRule) ReplicatesDeleteMarkers() bool { return toggled(r.DeleteMarkerReplication) }

// ReplicatesDeletes reports whether the permanent deletions of versions
// are replicated (DeleteReplication, an extension of the schema).
func (r ReplicationRule) ReplicatesDeletes() bool { return toggled(r.DeleteReplication) }

// ReplicatesExisting reports whether a resync replicates the versions
// written before the rule.
func (r ReplicationRule) ReplicatesExisting() bool { return toggled(r.ExistingObjectReplication) }

func toggled(t *ReplicationToggle) bool { return t != nil && t.Status == ReplicationEnabled }

// prefix returns the key prefix the rule selects ("" for every key).
func (r ReplicationRule) prefix() string {
	switch {
	case r.Filter != nil && r.Filter.And != nil:
		return r.Filter.And.Prefix
	case r.Filter != nil && r.Filter.Prefix != nil:
		return *r.Filter.Prefix
	case r.Prefix != nil:
		return *r.Prefix
	}
	return ""
}

// tags returns the tags the rule requires.
func (r ReplicationRule) tags() []ReplicationTag {
	switch {
	case r.Filter == nil:
		return nil
	case r.Filter.And != nil:
		return r.Filter.And.Tags
	case r.Filter.Tag != nil:
		return []ReplicationTag{*r.Filter.Tag}
	}
	return nil
}

// Selects reports whether the rule selects a version of key with tags
// (enabled or not).
func (r ReplicationRule) Selects(key string, tags map[string]string) bool {
	if !strings.HasPrefix(key, r.prefix()) {
		return false
	}
	for _, t := range r.tags() {
		if v, ok := tags[t.Key]; !ok || v != t.Value {
			return false
		}
	}
	return true
}

// UsesTags reports whether a rule of the configuration selects by tag:
// changing the tags of a version may then change its destinations.
func (c *ReplicationConfig) UsesTags() bool {
	if c == nil {
		return false
	}
	for _, r := range c.Rules {
		if len(r.tags()) > 0 {
			return true
		}
	}
	return false
}

// Destinations returns, for every destination, the enabled rule of the
// highest priority that selects a version of key with tags, in the order
// of the destinations in the document. A disabled rule of a higher
// priority hides the destination.
func (c *ReplicationConfig) Destinations(key string, tags map[string]string) []ReplicationRule {
	if c == nil {
		return nil
	}
	best := map[string]int{}
	var order []string
	for i, r := range c.Rules {
		if !r.Selects(key, tags) {
			continue
		}
		arn := r.Destination.Bucket
		j, seen := best[arn]
		if !seen {
			order = append(order, arn)
			best[arn] = i
			continue
		}
		if r.Priority > c.Rules[j].Priority {
			best[arn] = i
		}
	}
	var out []ReplicationRule
	for _, arn := range order {
		if r := c.Rules[best[arn]]; r.Enabled() {
			out = append(out, r)
		}
	}
	return out
}

// DestinationARNs returns the destinations of the configuration, sorted.
func (c *ReplicationConfig) DestinationARNs() []string {
	if c == nil {
		return nil
	}
	var out []string
	for _, r := range c.Rules {
		if !slices.Contains(out, r.Destination.Bucket) {
			out = append(out, r.Destination.Bucket)
		}
	}
	slices.Sort(out)
	return out
}

// ParseReplicationConfig parses and validates a ReplicationConfiguration
// document. Rules without ID get one ("rule-<n>"); the options default to
// Disabled. knownARN, when set, must accept every destination (an
// InvalidArgument error names the first one it refuses).
func ParseReplicationConfig(doc []byte, knownARN func(string) bool) (*ReplicationConfig, error) {
	var c ReplicationConfig
	dec := xml.NewDecoder(bytes.NewReader(doc))
	if err := dec.Decode(&c); err != nil {
		if err == io.EOF {
			return nil, malformedReplication("empty document")
		}
		return nil, malformedReplication(err.Error())
	}
	if len(c.Rules) == 0 {
		return nil, malformedReplication("no rule")
	}
	if len(c.Rules) > MaxReplicationRules {
		return nil, invalidReplication(fmt.Sprintf("at most %d rules", MaxReplicationRules))
	}
	ids := map[string]bool{}
	priorities := map[int]bool{}
	for i := range c.Rules {
		r := &c.Rules[i]
		if r.ID == "" {
			r.ID = "rule-" + strconv.Itoa(i+1)
		}
		if len(r.ID) > maxReplicationRuleID {
			return nil, invalidReplication(fmt.Sprintf("rule ID %q longer than %d characters", r.ID[:32]+"...", maxReplicationRuleID))
		}
		if ids[r.ID] {
			return nil, invalidReplication(fmt.Sprintf("duplicate rule ID %q", r.ID))
		}
		ids[r.ID] = true
		if r.Status != ReplicationEnabled && r.Status != ReplicationDisabled {
			return nil, malformedReplication(fmt.Sprintf("rule %q: Status must be Enabled or Disabled", r.ID))
		}
		if r.Prefix != nil && r.Filter != nil {
			return nil, malformedReplication(fmt.Sprintf("rule %q: Prefix and Filter are exclusive", r.ID))
		}
		if f := r.Filter; f != nil {
			n := 0
			for _, set := range []bool{f.Prefix != nil, f.Tag != nil, f.And != nil} {
				if set {
					n++
				}
			}
			if n > 1 {
				return nil, malformedReplication(fmt.Sprintf("rule %q: the Filter holds one of Prefix, Tag and And", r.ID))
			}
			if f.And != nil && len(f.And.Tags) == 0 && f.And.Prefix == "" {
				return nil, malformedReplication(fmt.Sprintf("rule %q: an empty And", r.ID))
			}
			// The schema with a Filter orders its rules by priority.
			if priorities[r.Priority] {
				return nil, invalidReplication(fmt.Sprintf("rule %q: priority %d used twice", r.ID, r.Priority))
			}
			priorities[r.Priority] = true
		}
		seen := map[string]bool{}
		for _, t := range r.tags() {
			if t.Key == "" || len(t.Key) > 128 || len(t.Value) > 256 {
				return nil, invalidReplication(fmt.Sprintf("rule %q: invalid tag %q", r.ID, t.Key))
			}
			if seen[t.Key] {
				return nil, invalidReplication(fmt.Sprintf("rule %q: tag %q given twice", r.ID, t.Key))
			}
			seen[t.Key] = true
		}
		for name, t := range map[string]**ReplicationToggle{
			"DeleteMarkerReplication": &r.DeleteMarkerReplication, "ExistingObjectReplication": &r.ExistingObjectReplication,
			"DeleteReplication": &r.DeleteReplication,
		} {
			if *t == nil {
				*t = &ReplicationToggle{Status: ReplicationDisabled}
			}
			if s := (*t).Status; s != ReplicationEnabled && s != ReplicationDisabled {
				return nil, malformedReplication(fmt.Sprintf("rule %q: %s Status must be Enabled or Disabled", r.ID, name))
			}
		}
		if r.Destination.Bucket == "" {
			return nil, malformedReplication(fmt.Sprintf("rule %q: no Destination Bucket", r.ID))
		}
		if knownARN != nil && !knownARN(r.Destination.Bucket) {
			return nil, invalidReplication(fmt.Sprintf("rule %q: %q is not a replication target of the bucket", r.ID, r.Destination.Bucket))
		}
	}
	return &c, nil
}

// Encode returns the canonical document of the configuration.
func (c *ReplicationConfig) Encode() ([]byte, error) {
	out := *c
	out.XMLName = xml.Name{Space: "http://s3.amazonaws.com/doc/2006-03-01/", Local: "ReplicationConfiguration"}
	doc, err := xml.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encode the replication configuration: %w", err)
	}
	return doc, nil
}

func malformedReplication(msg string) error {
	return NewS3Error(ErrMalformedXML, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema: "+msg)
}

func invalidReplication(msg string) error {
	return NewS3Error(ErrInvalidArgument, "InvalidArgument", "Invalid replication configuration: "+msg)
}
