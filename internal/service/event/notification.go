package event

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
)

// ARN components of the notification targets.
const (
	arnPrefix    = "arn"
	arnPartition = "nexo"
	arnService   = "sqs"
	// maxFilterValue bounds the prefix and suffix of a filter rule.
	maxFilterValue = 1024
	// maxConfigIDLength bounds the Id of a configuration.
	maxConfigIDLength = 255
	// s3Namespace is the XML namespace of the S3 documents.
	s3Namespace = "http://s3.amazonaws.com/doc/2006-03-01/"
)

// Kinds of bucket notification configurations, as their XML elements.
const (
	KindQueue    = "Queue"
	KindTopic    = "Topic"
	KindFunction = "CloudFunction"
)

// ARN identifies a notification target: arn:nexo:sqs:<region>:<id>:<type>.
// The region may be empty (any region).
type ARN struct {
	Region string
	ID     string
	Type   string
}

// String returns the ARN.
func (a ARN) String() string {
	return strings.Join([]string{arnPrefix, arnPartition, arnService, a.Region, a.ID, a.Type}, ":")
}

// TargetID returns the target the ARN names.
func (a ARN) TargetID() TargetID { return TargetID{ID: a.ID, Type: a.Type} }

// ParseARN parses a target ARN. The services sqs, sns and lambda are
// accepted (the Queue, Topic and CloudFunction configurations of the S3
// clients); they all name the same targets.
func ParseARN(s string) (ARN, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 6 || parts[0] != arnPrefix || parts[1] != arnPartition || parts[4] == "" || parts[5] == "" {
		return ARN{}, invalidDestination(s)
	}
	switch parts[2] {
	case "sqs", "sns", "lambda":
	default:
		return ARN{}, invalidDestination(s)
	}
	if !knownType(parts[5]) {
		return ARN{}, invalidDestination(s)
	}
	return ARN{Region: parts[3], ID: parts[4], Type: parts[5]}, nil
}

// knownType reports a supported target type.
func knownType(t string) bool {
	switch t {
	case TypeWebhook, TypeKafka, TypeNATS, TypeAMQP, TypeRedis, TypePostgres:
		return true
	}
	return false
}

// invalidDestination is the S3 error of a destination ARN that names no
// target.
func invalidDestination(arn string) error {
	return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument",
		"A specified destination ARN does not exist or is not well-formed. Verify the destination ARN: "+arn)
}

// invalidConfig is the S3 error of an invalid notification configuration.
func invalidConfig(format string, args ...any) error {
	return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", fmt.Sprintf(format, args...))
}

// BucketConfig is the parsed notification configuration of a bucket.
type BucketConfig struct {
	Rules []Rule
}

// Rule is one Queue, Topic or CloudFunction configuration: the events it
// subscribes to, its key filter and its target.
type Rule struct {
	ID     string
	Kind   string
	ARN    ARN
	Events []Name
	Prefix string
	Suffix string
}

// Matches reports whether the rule routes the event (raw object key).
func (r Rule) Matches(e Event) bool {
	if r.Prefix != "" && !strings.HasPrefix(e.S3.Object.Key, r.Prefix) {
		return false
	}
	if r.Suffix != "" && !strings.HasSuffix(e.S3.Object.Key, r.Suffix) {
		return false
	}
	for _, p := range r.Events {
		if e.EventName.Match(p) {
			return true
		}
	}
	return false
}

// XML documents of the notification configuration.
type notificationXML struct {
	XMLName     xml.Name            `xml:"NotificationConfiguration"`
	Xmlns       string              `xml:"xmlns,attr,omitempty"`
	Queues      []queueConfigXML    `xml:"QueueConfiguration"`
	Topics      []topicConfigXML    `xml:"TopicConfiguration"`
	Functions   []functionConfigXML `xml:"CloudFunctionConfiguration"`
	EventBridge *struct{}           `xml:"EventBridgeConfiguration"`
}

type queueConfigXML struct {
	ID     string     `xml:"Id,omitempty"`
	ARN    string     `xml:"Queue"`
	Events []string   `xml:"Event"`
	Filter *filterXML `xml:"Filter,omitempty"`
}

type topicConfigXML struct {
	ID     string     `xml:"Id,omitempty"`
	ARN    string     `xml:"Topic"`
	Events []string   `xml:"Event"`
	Filter *filterXML `xml:"Filter,omitempty"`
}

type functionConfigXML struct {
	ID     string     `xml:"Id,omitempty"`
	ARN    string     `xml:"CloudFunction"`
	Events []string   `xml:"Event"`
	Filter *filterXML `xml:"Filter,omitempty"`
}

type filterXML struct {
	S3Key s3KeyXML `xml:"S3Key"`
}

type s3KeyXML struct {
	Rules []filterRuleXML `xml:"FilterRule"`
}

type filterRuleXML struct {
	Name  string `xml:"Name"`
	Value string `xml:"Value"`
}

// rawRule is a configuration of any kind before validation.
type rawRule struct {
	kind   string
	id     string
	arn    string
	events []string
	filter *filterXML
}

// ParseBucketConfig parses and validates a NotificationConfiguration
// document: known events, prefix/suffix filters, well-formed ARNs of
// region (an ARN without region matches any) and unique ids (missing ids
// are generated from the content of the rule). The destinations are not
// resolved (see Service.ValidateBucketConfig). Malformed XML is S3
// MalformedXML, invalid content InvalidArgument.
func ParseBucketConfig(doc []byte, region string) (*BucketConfig, error) {
	var x notificationXML
	dec := xml.NewDecoder(bytes.NewReader(doc))
	if err := dec.Decode(&x); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.")
		}
		return nil, domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema: "+err.Error())
	}
	if x.EventBridge != nil {
		return nil, domain.NewS3Error(domain.ErrNotImplemented, "NotImplemented", "EventBridge notifications are not supported")
	}
	var raws []rawRule
	for _, q := range x.Queues {
		raws = append(raws, rawRule{kind: KindQueue, id: q.ID, arn: q.ARN, events: q.Events, filter: q.Filter})
	}
	for _, t := range x.Topics {
		raws = append(raws, rawRule{kind: KindTopic, id: t.ID, arn: t.ARN, events: t.Events, filter: t.Filter})
	}
	for _, f := range x.Functions {
		raws = append(raws, rawRule{kind: KindFunction, id: f.ID, arn: f.ARN, events: f.Events, filter: f.Filter})
	}
	cfg := &BucketConfig{}
	ids := map[string]bool{}
	for _, raw := range raws {
		rule, err := parseRule(raw, region)
		if err != nil {
			return nil, err
		}
		if rule.ID != "" {
			if ids[rule.ID] {
				return nil, invalidConfig("Configuration Id %q is used more than once", rule.ID)
			}
			ids[rule.ID] = true
		}
		cfg.Rules = append(cfg.Rules, rule)
	}
	for i := range cfg.Rules {
		if cfg.Rules[i].ID != "" {
			continue
		}
		id := ruleID(cfg.Rules[i])
		for n := 1; ids[id]; n++ {
			id = fmt.Sprintf("%s-%d", ruleID(cfg.Rules[i]), n)
		}
		ids[id] = true
		cfg.Rules[i].ID = id
	}
	return cfg, nil
}

// parseRule validates one configuration.
func parseRule(raw rawRule, region string) (Rule, error) {
	if len(raw.id) > maxConfigIDLength {
		return Rule{}, invalidConfig("The Id of a configuration cannot be longer than %d characters", maxConfigIDLength)
	}
	arn, err := ParseARN(strings.TrimSpace(raw.arn))
	if err != nil {
		return Rule{}, err
	}
	if arn.Region != "" && region != "" && arn.Region != region {
		return Rule{}, invalidDestination(raw.arn)
	}
	rule := Rule{ID: raw.id, Kind: raw.kind, ARN: arn}
	if len(raw.events) == 0 {
		return Rule{}, invalidConfig("A %s configuration must name at least one event", raw.kind)
	}
	seen := map[Name]bool{}
	for _, ev := range raw.events {
		n := Name(strings.TrimSpace(ev))
		if !n.IsNotifiable() {
			return Rule{}, invalidConfig("The event %q is not supported for notifications", ev)
		}
		if !seen[n] {
			seen[n] = true
			rule.Events = append(rule.Events, n)
		}
	}
	if raw.filter != nil {
		var prefixSet, suffixSet bool
		for _, fr := range raw.filter.S3Key.Rules {
			if len(fr.Value) > maxFilterValue {
				return Rule{}, invalidConfig("The value of a filter rule cannot be longer than %d characters", maxFilterValue)
			}
			switch strings.ToLower(strings.TrimSpace(fr.Name)) {
			case "prefix":
				if prefixSet {
					return Rule{}, invalidConfig("Cannot specify more than one prefix rule in a filter.")
				}
				prefixSet, rule.Prefix = true, fr.Value
			case "suffix":
				if suffixSet {
					return Rule{}, invalidConfig("Cannot specify more than one suffix rule in a filter.")
				}
				suffixSet, rule.Suffix = true, fr.Value
			default:
				return Rule{}, invalidConfig("filter rule name must be either prefix or suffix")
			}
		}
	}
	return rule, nil
}

// ruleID derives a stable id from the content of a rule without one.
func ruleID(r Rule) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\n%s\n%s\n%s\n", r.Kind, r.ARN, r.Prefix, r.Suffix)
	for _, e := range r.Events {
		_, _ = fmt.Fprintf(h, "%s\n", e)
	}
	return "nexo-" + hex.EncodeToString(h.Sum(nil))[:16]
}

// EncodeBucketConfig returns the canonical XML document of cfg.
func EncodeBucketConfig(cfg *BucketConfig) ([]byte, error) {
	x := notificationXML{Xmlns: s3Namespace}
	if cfg != nil {
		for _, r := range cfg.Rules {
			events := make([]string, 0, len(r.Events))
			for _, e := range r.Events {
				events = append(events, string(e))
			}
			var f *filterXML
			if r.Prefix != "" || r.Suffix != "" {
				f = &filterXML{}
				if r.Prefix != "" {
					f.S3Key.Rules = append(f.S3Key.Rules, filterRuleXML{Name: "prefix", Value: r.Prefix})
				}
				if r.Suffix != "" {
					f.S3Key.Rules = append(f.S3Key.Rules, filterRuleXML{Name: "suffix", Value: r.Suffix})
				}
			}
			switch r.Kind {
			case KindTopic:
				x.Topics = append(x.Topics, topicConfigXML{ID: r.ID, ARN: r.ARN.String(), Events: events, Filter: f})
			case KindFunction:
				x.Functions = append(x.Functions, functionConfigXML{ID: r.ID, ARN: r.ARN.String(), Events: events, Filter: f})
			default:
				x.Queues = append(x.Queues, queueConfigXML{ID: r.ID, ARN: r.ARN.String(), Events: events, Filter: f})
			}
		}
	}
	out, err := xml.Marshal(x)
	if err != nil {
		return nil, fmt.Errorf("event: encode notification configuration: %w", err)
	}
	return append([]byte(xml.Header), out...), nil
}
