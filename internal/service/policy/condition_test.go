package policy_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// TestConditionOperators covers every condition operator (docs/06
// TS-U-08): each case is an identity policy allowing s3:GetObject under
// one condition block, evaluated with the given request keys.
func TestConditionOperators(t *testing.T) {
	ctx := context.Background()
	epoch := strconv.FormatInt(testNow.Unix(), 10)
	type kv = map[string][]string
	tests := []struct {
		name string
		cond string
		keys kv
		want bool
	}{
		// StringEquals / StringNotEquals.
		{"StringEquals match", `{"StringEquals":{"s3:x":"a"}}`, kv{"s3:x": {"a"}}, true},
		{"StringEquals is case-sensitive", `{"StringEquals":{"s3:x":"a"}}`, kv{"s3:x": {"A"}}, false},
		{"StringEquals missing key", `{"StringEquals":{"s3:x":"a"}}`, nil, false},
		{"StringEquals any policy value", `{"StringEquals":{"s3:x":["a","b"]}}`, kv{"s3:x": {"b"}}, true},
		{"StringEquals multi-valued request", `{"StringEquals":{"s3:x":"a"}}`, kv{"s3:x": {"c", "a"}}, true},
		{"StringEquals empty value", `{"StringEquals":{"s3:x":""}}`, kv{"s3:x": {""}}, true},
		{"StringNotEquals equal", `{"StringNotEquals":{"s3:x":"a"}}`, kv{"s3:x": {"a"}}, false},
		{"StringNotEquals different", `{"StringNotEquals":{"s3:x":"a"}}`, kv{"s3:x": {"b"}}, true},
		{"StringNotEquals missing key", `{"StringNotEquals":{"s3:x":"a"}}`, nil, true},
		{"StringNotEquals any policy value", `{"StringNotEquals":{"s3:x":["a","b"]}}`, kv{"s3:x": {"b"}}, false},
		{"StringNotEquals multi-valued request", `{"StringNotEquals":{"s3:x":"a"}}`, kv{"s3:x": {"b", "a"}}, false},
		{"StringEqualsIgnoreCase", `{"StringEqualsIgnoreCase":{"s3:x":"abc"}}`, kv{"s3:x": {"ABC"}}, true},
		{"StringEqualsIgnoreCase mismatch", `{"StringEqualsIgnoreCase":{"s3:x":"abc"}}`, kv{"s3:x": {"abd"}}, false},
		{"StringNotEqualsIgnoreCase equal", `{"StringNotEqualsIgnoreCase":{"s3:x":"abc"}}`, kv{"s3:x": {"Abc"}}, false},
		{"StringNotEqualsIgnoreCase different", `{"StringNotEqualsIgnoreCase":{"s3:x":"abc"}}`, kv{"s3:x": {"xyz"}}, true},
		{"StringNotEqualsIgnoreCase missing", `{"StringNotEqualsIgnoreCase":{"s3:x":"abc"}}`, nil, true},
		// StringLike / StringNotLike.
		{"StringLike star", `{"StringLike":{"s3:x":"a*"}}`, kv{"s3:x": {"abc"}}, true},
		{"StringLike question", `{"StringLike":{"s3:x":"a?c"}}`, kv{"s3:x": {"abc"}}, true},
		{"StringLike question is one char", `{"StringLike":{"s3:x":"a?c"}}`, kv{"s3:x": {"abbc"}}, false},
		{"StringLike unicode question", `{"StringLike":{"s3:x":"caf?"}}`, kv{"s3:x": {"café"}}, true},
		{"StringLike is case-sensitive", `{"StringLike":{"s3:x":"a*"}}`, kv{"s3:x": {"ABC"}}, false},
		{"StringLike star matches empty", `{"StringLike":{"s3:x":"*"}}`, kv{"s3:x": {""}}, true},
		{"StringLike missing key", `{"StringLike":{"s3:x":"*"}}`, nil, false},
		{"StringLike inner star", `{"StringLike":{"s3:x":"a*c*e"}}`, kv{"s3:x": {"abcde"}}, true},
		{"StringNotLike match", `{"StringNotLike":{"s3:x":"a*"}}`, kv{"s3:x": {"abc"}}, false},
		{"StringNotLike no match", `{"StringNotLike":{"s3:x":"a*"}}`, kv{"s3:x": {"bcd"}}, true},
		{"StringNotLike missing", `{"StringNotLike":{"s3:x":"a*"}}`, nil, true},
		// Numeric.
		{"NumericEquals", `{"NumericEquals":{"s3:max-keys":"10"}}`, kv{"s3:max-keys": {"10"}}, true},
		{"NumericEquals float form", `{"NumericEquals":{"s3:max-keys":10}}`, kv{"s3:max-keys": {"10.0"}}, true},
		{"NumericEquals mismatch", `{"NumericEquals":{"s3:max-keys":"10"}}`, kv{"s3:max-keys": {"11"}}, false},
		{"NumericEquals not a number", `{"NumericEquals":{"s3:max-keys":"10"}}`, kv{"s3:max-keys": {"ten"}}, false},
		{"NumericEquals missing", `{"NumericEquals":{"s3:max-keys":"10"}}`, nil, false},
		{"NumericNotEquals different", `{"NumericNotEquals":{"s3:max-keys":"10"}}`, kv{"s3:max-keys": {"11"}}, true},
		{"NumericNotEquals equal", `{"NumericNotEquals":{"s3:max-keys":"10"}}`, kv{"s3:max-keys": {"10"}}, false},
		{"NumericNotEquals not a number fails closed", `{"NumericNotEquals":{"s3:max-keys":"10"}}`, kv{"s3:max-keys": {"x"}}, false},
		{"NumericNotEquals missing", `{"NumericNotEquals":{"s3:max-keys":"10"}}`, nil, true},
		{"NumericLessThan", `{"NumericLessThan":{"s3:max-keys":"10"}}`, kv{"s3:max-keys": {"5"}}, true},
		{"NumericLessThan equal", `{"NumericLessThan":{"s3:max-keys":"10"}}`, kv{"s3:max-keys": {"10"}}, false},
		{"NumericLessThanEquals equal", `{"NumericLessThanEquals":{"s3:max-keys":"10"}}`, kv{"s3:max-keys": {"10"}}, true},
		{"NumericLessThanEquals above", `{"NumericLessThanEquals":{"s3:max-keys":"10"}}`, kv{"s3:max-keys": {"10.5"}}, false},
		{"NumericGreaterThan", `{"NumericGreaterThan":{"s3:max-keys":"10"}}`, kv{"s3:max-keys": {"11"}}, true},
		{"NumericGreaterThan equal", `{"NumericGreaterThan":{"s3:max-keys":"10"}}`, kv{"s3:max-keys": {"10"}}, false},
		{"NumericGreaterThanEquals equal", `{"NumericGreaterThanEquals":{"s3:max-keys":"10"}}`, kv{"s3:max-keys": {"10"}}, true},
		{"NumericGreaterThanEquals below", `{"NumericGreaterThanEquals":{"s3:max-keys":"10"}}`, kv{"s3:max-keys": {"-1"}}, false},
		{"Numeric NaN is not a number", `{"NumericLessThan":{"s3:max-keys":"10"}}`, kv{"s3:max-keys": {"NaN"}}, false},
		{"Numeric on aws:EpochTime", `{"NumericGreaterThanEquals":{"aws:EpochTime":"` + epoch + `"}}`, nil, true},
		// Date (aws:CurrentTime is set by the engine from Args.Now).
		{"DateEquals", `{"DateEquals":{"aws:CurrentTime":"2026-10-08T12:00:00Z"}}`, nil, true},
		{"DateEquals other zone", `{"DateEquals":{"aws:CurrentTime":"2026-10-08T14:00:00+02:00"}}`, nil, true},
		{"DateEquals epoch seconds", `{"DateEquals":{"aws:CurrentTime":"` + epoch + `"}}`, nil, true},
		{"DateNotEquals", `{"DateNotEquals":{"aws:CurrentTime":"2026-10-08T12:00:00Z"}}`, nil, false},
		{"DateNotEquals different", `{"DateNotEquals":{"aws:CurrentTime":"2020-01-01T00:00:00Z"}}`, nil, true},
		{"DateLessThan", `{"DateLessThan":{"aws:CurrentTime":"2027-01-01T00:00:00Z"}}`, nil, true},
		{"DateLessThan past", `{"DateLessThan":{"aws:CurrentTime":"2026-01-01"}}`, nil, false},
		{"DateLessThanEquals equal", `{"DateLessThanEquals":{"aws:CurrentTime":"2026-10-08T12:00:00Z"}}`, nil, true},
		{"DateGreaterThan", `{"DateGreaterThan":{"aws:CurrentTime":"2026-01-01"}}`, nil, true},
		{"DateGreaterThan future", `{"DateGreaterThan":{"aws:CurrentTime":"2027-01-01T00:00:00Z"}}`, nil, false},
		{"DateGreaterThanEquals equal", `{"DateGreaterThanEquals":{"aws:CurrentTime":"2026-10-08T12:00:00Z"}}`, nil, true},
		{"Date request value overrides the clock", `{"DateLessThan":{"aws:CurrentTime":"2026-01-01T00:00:00Z"}}`, kv{"aws:CurrentTime": {"2025-06-01T00:00:00Z"}}, true},
		{"Date invalid request value", `{"DateLessThan":{"s3:object-lock-retain-until-date":"2026-01-01T00:00:00Z"}}`, kv{"s3:object-lock-retain-until-date": {"tomorrow"}}, false},
		{"Date missing key", `{"DateLessThan":{"s3:object-lock-retain-until-date":"2026-01-01T00:00:00Z"}}`, nil, false},
		// Bool.
		{"Bool true", `{"Bool":{"aws:SecureTransport":"true"}}`, kv{"aws:SecureTransport": {"true"}}, true},
		{"Bool JSON boolean", `{"Bool":{"aws:SecureTransport":true}}`, kv{"aws:SecureTransport": {"TRUE"}}, true},
		{"Bool false", `{"Bool":{"aws:SecureTransport":"false"}}`, kv{"aws:SecureTransport": {"true"}}, false},
		{"Bool false matches false", `{"Bool":{"aws:SecureTransport":"false"}}`, kv{"aws:SecureTransport": {"false"}}, true},
		{"Bool invalid request value", `{"Bool":{"aws:SecureTransport":"true"}}`, kv{"aws:SecureTransport": {"yes"}}, false},
		{"Bool missing", `{"Bool":{"aws:SecureTransport":"true"}}`, nil, false},
		// IpAddress / NotIpAddress, IPv4 and IPv6.
		{"IpAddress CIDR", `{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}`, kv{"aws:SourceIp": {"10.1.2.3"}}, true},
		{"IpAddress outside", `{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}`, kv{"aws:SourceIp": {"11.0.0.1"}}, false},
		{"IpAddress single host", `{"IpAddress":{"aws:SourceIp":"192.168.1.1"}}`, kv{"aws:SourceIp": {"192.168.1.1"}}, true},
		{"IpAddress single host other", `{"IpAddress":{"aws:SourceIp":"192.168.1.1"}}`, kv{"aws:SourceIp": {"192.168.1.2"}}, false},
		{"IpAddress list", `{"IpAddress":{"aws:SourceIp":["10.0.0.0/8","172.16.0.0/12"]}}`, kv{"aws:SourceIp": {"172.20.0.1"}}, true},
		{"IpAddress with port", `{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}`, kv{"aws:SourceIp": {"10.0.0.1:4321"}}, true},
		{"IpAddress IPv6 CIDR", `{"IpAddress":{"aws:SourceIp":"2001:db8::/32"}}`, kv{"aws:SourceIp": {"2001:db8::1"}}, true},
		{"IpAddress IPv6 outside", `{"IpAddress":{"aws:SourceIp":"2001:db8::/32"}}`, kv{"aws:SourceIp": {"2001:db9::1"}}, false},
		{"IpAddress IPv6 with port", `{"IpAddress":{"aws:SourceIp":"2001:db8::/32"}}`, kv{"aws:SourceIp": {"[2001:db8::7]:443"}}, true},
		{"IpAddress IPv6 zone", `{"IpAddress":{"aws:SourceIp":"fe80::/10"}}`, kv{"aws:SourceIp": {"fe80::1%eth0"}}, true},
		{"IpAddress IPv4-mapped request", `{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}`, kv{"aws:SourceIp": {"::ffff:10.0.0.1"}}, true},
		{"IpAddress IPv4-mapped policy", `{"IpAddress":{"aws:SourceIp":"::ffff:10.0.0.0/104"}}`, kv{"aws:SourceIp": {"10.9.9.9"}}, true},
		{"IpAddress IPv4 does not match IPv6 network", `{"IpAddress":{"aws:SourceIp":"::/0"}}`, kv{"aws:SourceIp": {"10.0.0.1"}}, false},
		{"IpAddress everything v4", `{"IpAddress":{"aws:SourceIp":"0.0.0.0/0"}}`, kv{"aws:SourceIp": {"8.8.8.8"}}, true},
		{"IpAddress invalid request value", `{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}`, kv{"aws:SourceIp": {"nope"}}, false},
		{"IpAddress missing", `{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}`, nil, false},
		{"NotIpAddress outside", `{"NotIpAddress":{"aws:SourceIp":"10.0.0.0/8"}}`, kv{"aws:SourceIp": {"8.8.8.8"}}, true},
		{"NotIpAddress inside", `{"NotIpAddress":{"aws:SourceIp":"10.0.0.0/8"}}`, kv{"aws:SourceIp": {"10.0.0.1"}}, false},
		{"NotIpAddress IPv6 inside", `{"NotIpAddress":{"aws:SourceIp":"::1/128"}}`, kv{"aws:SourceIp": {"::1"}}, false},
		{"NotIpAddress IPv6 outside", `{"NotIpAddress":{"aws:SourceIp":"::1/128"}}`, kv{"aws:SourceIp": {"::2"}}, true},
		{"NotIpAddress missing", `{"NotIpAddress":{"aws:SourceIp":"10.0.0.0/8"}}`, nil, true},
		{"NotIpAddress invalid request fails closed", `{"NotIpAddress":{"aws:SourceIp":"10.0.0.0/8"}}`, kv{"aws:SourceIp": {"nope"}}, false},
		// Null.
		{"Null true and absent", `{"Null":{"s3:x":"true"}}`, nil, true},
		{"Null true and present", `{"Null":{"s3:x":"true"}}`, kv{"s3:x": {"v"}}, false},
		{"Null false and present", `{"Null":{"s3:x":"false"}}`, kv{"s3:x": {"v"}}, true},
		{"Null false and absent", `{"Null":{"s3:x":false}}`, nil, false},
		{"Null empty set is absent", `{"Null":{"s3:x":true}}`, kv{"s3:x": {}}, true},
		// Arn operators.
		{"ArnLike account wildcard", `{"ArnLike":{"aws:SourceArn":"arn:aws:iam::*:user/bob"}}`, kv{"aws:SourceArn": {"arn:aws:iam::123456789012:user/bob"}}, true},
		{"ArnLike resource wildcard", `{"ArnLike":{"aws:SourceArn":"arn:aws:s3:::logs/*"}}`, kv{"aws:SourceArn": {"arn:aws:s3:::logs/2026/a"}}, true},
		{"ArnLike star stays in its component", `{"ArnLike":{"aws:SourceArn":"arn:aws:*:::x"}}`, kv{"aws:SourceArn": {"arn:aws:s3:eu:1:x"}}, false},
		{"ArnLike mismatch", `{"ArnLike":{"aws:SourceArn":"arn:aws:s3:::logs/*"}}`, kv{"aws:SourceArn": {"arn:aws:s3:::other/x"}}, false},
		{"ArnLike not an ARN", `{"ArnLike":{"aws:SourceArn":"arn:aws:s3:::logs/*"}}`, kv{"aws:SourceArn": {"logs/x"}}, false},
		{"ArnLike plain star", `{"ArnLike":{"aws:SourceArn":"*"}}`, kv{"aws:SourceArn": {"anything"}}, true},
		{"ArnEquals", `{"ArnEquals":{"aws:SourceArn":"arn:aws:sns:eu-south-1:1:topic"}}`, kv{"aws:SourceArn": {"arn:aws:sns:eu-south-1:1:topic"}}, true},
		{"ArnEquals resource with colons", `{"ArnEquals":{"aws:SourceArn":"arn:aws:lambda:eu:1:function:f:1"}}`, kv{"aws:SourceArn": {"arn:aws:lambda:eu:1:function:f:1"}}, true},
		{"ArnEquals mismatch", `{"ArnEquals":{"aws:SourceArn":"arn:aws:sns:eu-south-1:1:topic"}}`, kv{"aws:SourceArn": {"arn:aws:sns:eu-south-1:2:topic"}}, false},
		{"ArnNotLike", `{"ArnNotLike":{"aws:SourceArn":"arn:aws:s3:::logs/*"}}`, kv{"aws:SourceArn": {"arn:aws:s3:::other/x"}}, true},
		{"ArnNotLike match", `{"ArnNotLike":{"aws:SourceArn":"arn:aws:s3:::logs/*"}}`, kv{"aws:SourceArn": {"arn:aws:s3:::logs/x"}}, false},
		{"ArnNotLike missing", `{"ArnNotLike":{"aws:SourceArn":"arn:aws:s3:::logs/*"}}`, nil, true},
		{"ArnNotEquals", `{"ArnNotEquals":{"aws:SourceArn":"arn:aws:s3:::a"}}`, kv{"aws:SourceArn": {"arn:aws:s3:::b"}}, true},
		// IfExists.
		{"IfExists missing passes", `{"StringEqualsIfExists":{"aws:Referer":"https://ok"}}`, nil, true},
		{"IfExists present matching", `{"StringEqualsIfExists":{"aws:Referer":"https://ok"}}`, kv{"aws:Referer": {"https://ok"}}, true},
		{"IfExists present not matching", `{"StringEqualsIfExists":{"aws:Referer":"https://ok"}}`, kv{"aws:Referer": {"https://evil"}}, false},
		{"NumericIfExists missing", `{"NumericLessThanIfExists":{"s3:max-keys":"10"}}`, nil, true},
		{"NumericIfExists present", `{"NumericLessThanIfExists":{"s3:max-keys":"10"}}`, kv{"s3:max-keys": {"50"}}, false},
		{"IpAddressIfExists missing", `{"IpAddressIfExists":{"aws:SourceIp":"10.0.0.0/8"}}`, nil, true},
		{"BoolIfExists present false", `{"BoolIfExists":{"aws:SecureTransport":"true"}}`, kv{"aws:SecureTransport": {"false"}}, false},
		{"DateIfExists missing", `{"DateGreaterThanIfExists":{"s3:object-lock-retain-until-date":"2026-01-01"}}`, nil, true},
		{"ArnLikeIfExists missing", `{"ArnLikeIfExists":{"aws:SourceArn":"arn:aws:s3:::x"}}`, nil, true},
		// Set qualifiers.
		{"ForAnyValue match", `{"ForAnyValue:StringEquals":{"aws:TagKeys":["a","b"]}}`, kv{"aws:TagKeys": {"c", "b"}}, true},
		{"ForAnyValue no match", `{"ForAnyValue:StringEquals":{"aws:TagKeys":["a","b"]}}`, kv{"aws:TagKeys": {"c", "d"}}, false},
		{"ForAnyValue missing", `{"ForAnyValue:StringEquals":{"aws:TagKeys":["a"]}}`, nil, false},
		{"ForAnyValue negated", `{"ForAnyValue:StringNotEquals":{"aws:TagKeys":["a"]}}`, kv{"aws:TagKeys": {"a", "b"}}, true},
		{"ForAnyValue negated all equal", `{"ForAnyValue:StringNotEquals":{"aws:TagKeys":["a"]}}`, kv{"aws:TagKeys": {"a"}}, false},
		{"ForAllValues subset", `{"ForAllValues:StringEquals":{"aws:TagKeys":["a","b"]}}`, kv{"aws:TagKeys": {"a", "b"}}, true},
		{"ForAllValues not subset", `{"ForAllValues:StringEquals":{"aws:TagKeys":["a","b"]}}`, kv{"aws:TagKeys": {"a", "c"}}, false},
		{"ForAllValues missing key", `{"ForAllValues:StringEquals":{"aws:TagKeys":["a"]}}`, nil, true},
		{"ForAllValues empty set", `{"ForAllValues:StringEquals":{"aws:TagKeys":["a"]}}`, kv{"aws:TagKeys": {}}, true},
		{"ForAllValues like", `{"ForAllValues:StringLike":{"aws:TagKeys":["dept/*"]}}`, kv{"aws:TagKeys": {"dept/a", "dept/b"}}, true},
		{"ForAllValues negated", `{"ForAllValues:StringNotEquals":{"aws:TagKeys":["secret"]}}`, kv{"aws:TagKeys": {"a", "b"}}, true},
		{"ForAllValues negated hit", `{"ForAllValues:StringNotEquals":{"aws:TagKeys":["secret"]}}`, kv{"aws:TagKeys": {"a", "secret"}}, false},
		{"ForAnyValue IfExists missing", `{"ForAnyValue:StringEqualsIfExists":{"aws:TagKeys":["a"]}}`, nil, true},
		// Combination: every key and every operator must hold.
		{"two keys both hold", `{"StringEquals":{"s3:x":"a","s3:y":"b"}}`, kv{"s3:x": {"a"}, "s3:y": {"b"}}, true},
		{"two keys one missing", `{"StringEquals":{"s3:x":"a","s3:y":"b"}}`, kv{"s3:x": {"a"}}, false},
		{"two operators both hold", `{"StringEquals":{"s3:x":"a"},"NumericLessThan":{"s3:max-keys":"5"}}`, kv{"s3:x": {"a"}, "s3:max-keys": {"1"}}, true},
		{"two operators one fails", `{"StringEquals":{"s3:x":"a"},"NumericLessThan":{"s3:max-keys":"5"}}`, kv{"s3:x": {"a"}, "s3:max-keys": {"9"}}, false},
		// Keys are case-insensitive, tag keys keep their case.
		{"key name case-insensitive", `{"IpAddress":{"AWS:SOURCEIP":"10.0.0.0/8"}}`, kv{"aws:SourceIp": {"10.0.0.1"}}, true},
		{"tag key keeps case", `{"StringEquals":{"s3:ExistingObjectTag/Env":"prod"}}`, kv{"S3:EXISTINGOBJECTTAG/Env": {"prod"}}, true},
		{"tag key is case-sensitive", `{"StringEquals":{"s3:ExistingObjectTag/Env":"prod"}}`, kv{"s3:ExistingObjectTag/env": {"prod"}}, false},
		// Policy variables in condition values.
		{"variable username", `{"StringLike":{"s3:prefix":"${aws:username}/*"}}`, kv{"s3:prefix": {"alice/docs"}}, true},
		{"variable username other prefix", `{"StringLike":{"s3:prefix":"${aws:username}/*"}}`, kv{"s3:prefix": {"bob/docs"}}, false},
		{"variable userid", `{"StringEquals":{"s3:x":"${aws:userid}"}}`, kv{"s3:x": {"alice"}}, true},
		{"principaltype of an STS identity", `{"StringEquals":{"aws:principaltype":"AssumedRole"}}`, nil, true},
		{"variable from request key", `{"StringEquals":{"s3:x":"${s3:prefix}"}}`, kv{"s3:x": {"p/"}, "s3:prefix": {"p/"}}, true},
		{"unresolved variable never matches", `{"StringEquals":{"s3:x":"${aws:nothing}"}}`, kv{"s3:x": {""}}, false},
		{"unresolved variable fails negation", `{"StringNotEquals":{"s3:x":"${aws:nothing}"}}`, kv{"s3:x": {"a"}}, false},
		{"variable default value", `{"StringEquals":{"s3:x":"${aws:nothing, 'dflt'}"}}`, kv{"s3:x": {"dflt"}}, true},
		{"variable default not used when set", `{"StringEquals":{"s3:x":"${aws:username, 'dflt'}"}}`, kv{"s3:x": {"alice"}}, true},
		{"multi-valued key is not a variable", `{"StringEquals":{"s3:x":"${aws:TagKeys}"}}`, kv{"s3:x": {"a"}, "aws:TagKeys": {"a", "b"}}, false},
		{"escaped star is literal", `{"StringLike":{"s3:x":"a${*}"}}`, kv{"s3:x": {"ab"}}, false},
		{"escaped star matches star", `{"StringLike":{"s3:x":"a${*}"}}`, kv{"s3:x": {"a*"}}, true},
		{"escaped question", `{"StringEquals":{"s3:x":"why${?}"}}`, kv{"s3:x": {"why?"}}, true},
		{"escaped dollar", `{"StringEquals":{"s3:x":"${$}{x}"}}`, kv{"s3:x": {"${x}"}}, true},
		{"variable value is literal in StringLike", `{"StringLike":{"s3:x":"${s3:prefix}"}}`, kv{"s3:x": {"abc"}, "s3:prefix": {"a*"}}, false},
		// Keys derived from the identity.
		{"s3:authType from identity", `{"StringEquals":{"s3:authType":"REST-QUERY-STRING"}}`, nil, true},
		{"s3:signatureAge from identity", `{"NumericLessThan":{"s3:signatureAge":"600000"}}`, nil, true},
		{"s3:signatureAge too old", `{"NumericLessThan":{"s3:signatureAge":"1000"}}`, nil, false},
		{"jwt claim variable", `{"StringEquals":{"s3:x":"${jwt:sub}"}}`, kv{"s3:x": {"subject-1"}}, true},
		{"jwt claim list", `{"ForAnyValue:StringEquals":{"jwt:groups":"ops"}}`, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := engineWith(t, map[string]string{"p": doc(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":` + tc.cond + `}`)})
			id := &domain.Identity{
				AccessKey: "alice", PolicyNames: []string{"p"}, AuthType: domain.AuthTypeQuery, SignatureAge: 5000 * 1e6,
				Claims: map[string]any{"sub": "subject-1", "groups": []any{"dev", "ops"}, "admin": false, "level": float64(3)},
			}
			args := policy.Args{Identity: id, Action: policy.ActionGetObject, Bucket: "b", Object: "k", Conditions: tc.keys, Now: testNow}
			d, err := svc.Evaluate(ctx, args)
			require.NoError(t, err)
			assert.Equal(t, tc.want, d.Allowed, d.Reason)
			sim, err := svc.Simulate(ctx, args)
			require.NoError(t, err)
			assert.Equal(t, d.Allowed, sim.Allowed)
			require.Len(t, sim.Evaluated, 1)
			if !tc.want {
				assert.Equal(t, policy.MismatchCondition, sim.Evaluated[0].Mismatch)
			}
		})
	}
}
