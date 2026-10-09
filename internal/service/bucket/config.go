package bucket

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/r3n3r0/nexo/internal/domain"
)

// corsDocument is the S3 CORSConfiguration document. The namespace is
// not declared: clients may omit it.
type corsDocument struct {
	XMLName xml.Name      `xml:"CORSConfiguration"`
	Rules   []corsRuleXML `xml:"CORSRule"`
}

// corsRuleXML is one CORSRule of a CORSConfiguration document.
type corsRuleXML struct {
	ID             string   `xml:"ID"`
	AllowedOrigins []string `xml:"AllowedOrigin"`
	AllowedMethods []string `xml:"AllowedMethod"`
	AllowedHeaders []string `xml:"AllowedHeader"`
	ExposeHeaders  []string `xml:"ExposeHeader"`
	MaxAgeSeconds  *int     `xml:"MaxAgeSeconds"`
}

// parseCORS decodes a CORSConfiguration document (S3 MalformedXML when it
// cannot be decoded).
func parseCORS(doc []byte) (domain.CORSConfiguration, error) {
	var d corsDocument
	if err := xml.NewDecoder(bytes.NewReader(doc)).Decode(&d); err != nil {
		return domain.CORSConfiguration{}, domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML",
			fmt.Sprintf("The XML you provided was not well-formed or did not validate against our published schema: %v", err))
	}
	cfg := domain.CORSConfiguration{Rules: make([]domain.CORSRule, 0, len(d.Rules))}
	for _, r := range d.Rules {
		cfg.Rules = append(cfg.Rules, domain.CORSRule{
			ID:             strings.TrimSpace(r.ID),
			AllowedOrigins: trimAll(r.AllowedOrigins),
			AllowedMethods: trimAll(r.AllowedMethods),
			AllowedHeaders: trimAll(r.AllowedHeaders),
			ExposeHeaders:  trimAll(r.ExposeHeaders),
			MaxAgeSeconds:  r.MaxAgeSeconds,
		})
	}
	return cfg, nil
}

// trimAll trims the surrounding spaces of every value.
func trimAll(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = strings.TrimSpace(v)
	}
	return out
}

// validateTags applies the S3 rules of bucket tags: at most 50 tags, keys
// of 1-128 and values of 0-256 Unicode characters (valid UTF-8), no key
// in the reserved "aws:" namespace.
func validateTags(tags map[string]string) error {
	if len(tags) > maxBucketTags {
		return domain.NewS3Error(domain.ErrInvalidArgument, "BadRequest", "Bucket tag count cannot be greater than 50")
	}
	for k, v := range tags {
		if k == "" || !utf8.ValidString(k) || utf8.RuneCountInString(k) > maxTagKeyLength {
			return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidTag", "The TagKey you have provided is invalid")
		}
		if strings.HasPrefix(strings.ToLower(k), "aws:") {
			return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidTag", "System tags cannot be added/updated by requester")
		}
		if !utf8.ValidString(v) || utf8.RuneCountInString(v) > maxTagValueLength {
			return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidTag", "The TagValue you have provided is invalid")
		}
	}
	return nil
}
