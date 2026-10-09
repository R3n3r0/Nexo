package domain

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func intPtr(n int) *int { return &n }

func TestCORSConfigurationValidate(t *testing.T) {
	valid := CORSRule{AllowedOrigins: []string{"https://*.example.com"}, AllowedMethods: []string{"GET", "PUT"}, AllowedHeaders: []string{"*"}}
	many := make([]CORSRule, MaxCORSRules+1)
	for i := range many {
		many[i] = valid
	}
	tests := []struct {
		name string
		cfg  CORSConfiguration
		code string
	}{
		{"valid", CORSConfiguration{Rules: []CORSRule{valid}}, ""},
		{"max rules", CORSConfiguration{Rules: many[:MaxCORSRules]}, ""},
		{"no rules", CORSConfiguration{}, "MalformedXML"},
		{"too many rules", CORSConfiguration{Rules: many}, "InvalidRequest"},
		{"no origin", CORSConfiguration{Rules: []CORSRule{{AllowedMethods: []string{"GET"}}}}, "MalformedXML"},
		{"no method", CORSConfiguration{Rules: []CORSRule{{AllowedOrigins: []string{"*"}}}}, "MalformedXML"},
		{"bad method", CORSConfiguration{Rules: []CORSRule{{AllowedOrigins: []string{"*"}, AllowedMethods: []string{"PATCH"}}}}, "InvalidRequest"},
		{"lowercase method", CORSConfiguration{Rules: []CORSRule{{AllowedOrigins: []string{"*"}, AllowedMethods: []string{"get"}}}}, "InvalidRequest"},
		{"two origin wildcards", CORSConfiguration{Rules: []CORSRule{{AllowedOrigins: []string{"https://*.*.com"}, AllowedMethods: []string{"GET"}}}}, "InvalidRequest"},
		{"two header wildcards", CORSConfiguration{Rules: []CORSRule{{AllowedOrigins: []string{"*"}, AllowedMethods: []string{"GET"}, AllowedHeaders: []string{"x-*-*"}}}}, "InvalidRequest"},
		{"long id", CORSConfiguration{Rules: []CORSRule{{ID: strings.Repeat("a", 256), AllowedOrigins: []string{"*"}, AllowedMethods: []string{"GET"}}}}, "InvalidArgument"},
		{"negative max age", CORSConfiguration{Rules: []CORSRule{{AllowedOrigins: []string{"*"}, AllowedMethods: []string{"GET"}, MaxAgeSeconds: intPtr(-1)}}}, "InvalidArgument"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.code == "" {
				require.NoError(t, err)
				return
			}
			var s3err *S3Error
			require.ErrorAs(t, err, &s3err)
			assert.Equal(t, tt.code, s3err.Code)
		})
	}
}

func TestCORSConfigurationMatch(t *testing.T) {
	cfg := CORSConfiguration{Rules: []CORSRule{
		{ID: "app", AllowedOrigins: []string{"https://app.example.com"}, AllowedMethods: []string{"GET", "PUT"}, AllowedHeaders: []string{"Content-Type", "x-amz-*"}},
		{ID: "sub", AllowedOrigins: []string{"https://*.example.org"}, AllowedMethods: []string{"GET"}},
		{ID: "any", AllowedOrigins: []string{"*"}, AllowedMethods: []string{"HEAD"}},
	}}
	tests := []struct {
		name    string
		origin  string
		method  string
		headers []string
		want    string
	}{
		{"exact origin", "https://app.example.com", "PUT", nil, "app"},
		{"allowed headers case-insensitive", "https://app.example.com", "PUT", []string{"content-type", "X-Amz-Date"}, "app"},
		{"header not allowed", "https://app.example.com", "PUT", []string{"authorization"}, ""},
		{"method not allowed", "https://app.example.com", "DELETE", nil, ""},
		{"wildcard origin", "https://cdn.example.org", "GET", nil, "sub"},
		{"wildcard needs a match", "https://example.org", "GET", nil, ""},
		{"no headers allowed", "https://cdn.example.org", "GET", []string{"x-custom"}, ""},
		{"any origin", "http://evil.test", "HEAD", nil, "any"},
		{"other origin", "http://evil.test", "GET", nil, ""},
		{"empty header names ignored", "https://cdn.example.org", "GET", []string{"", " "}, "sub"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, ok := cfg.Match(tt.origin, tt.method, tt.headers)
			assert.Equal(t, tt.want != "", ok)
			assert.Equal(t, tt.want, r.ID)
		})
	}
	assert.True(t, cfg.Rules[2].AllowsAnyOrigin())
	assert.False(t, cfg.Rules[1].AllowsAnyOrigin())
}
