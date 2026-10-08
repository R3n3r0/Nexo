package policy_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

func TestParseValid(t *testing.T) {
	ctx := context.Background()
	svc, _ := newEngine(t)
	tests := []struct {
		name  string
		doc   string
		check func(t *testing.T, p *domain.Policy)
	}{
		{"single statement object", `{"Version":"2012-10-17","Id":"doc-1","Statement":{"Sid":"one","Effect":"Allow","Action":"s3:*","Resource":["arn:aws:s3:::b","arn:aws:s3:::b/*"]}}`,
			func(t *testing.T, p *domain.Policy) {
				require.Len(t, p.Statement, 1)
				assert.Equal(t, "one", p.Statement[0].SID)
				assert.Equal(t, "doc-1", p.ID)
				assert.Equal(t, domain.StringSet{"s3:*"}, p.Statement[0].Action)
			}},
		{"statement array", doc(`{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject"],"Resource":"*"}`, `{"Effect":"Deny","NotAction":"s3:Get*","NotResource":"arn:aws:s3:::b/*"}`),
			func(t *testing.T, p *domain.Policy) { assert.Len(t, p.Statement, 2) }},
		{"principal star", doc(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}`),
			func(t *testing.T, p *domain.Policy) { assert.True(t, p.Statement[0].Principal.IsWildcard()) }},
		{"principal object", doc(`{"Effect":"Allow","Principal":{"AWS":["arn:aws:iam::1:user/a","b"],"Service":"s3.amazonaws.com"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}`),
			func(t *testing.T, p *domain.Policy) {
				assert.Equal(t, domain.StringSet{"arn:aws:iam::1:user/a", "b"}, p.Statement[0].Principal.AWS)
				assert.Equal(t, domain.StringSet{"s3.amazonaws.com"}, p.Statement[0].Principal.Service)
			}},
		{"not principal", doc(`{"Effect":"Deny","NotPrincipal":{"AWS":"root"},"Action":"s3:*","Resource":"*"}`),
			func(t *testing.T, p *domain.Policy) { assert.NotNil(t, p.Statement[0].NotPrincipal) }},
		{"every condition type", doc(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":{
			"StringLike":{"s3:prefix":["a/*","${aws:username}/*"]},"NumericLessThan":{"s3:max-keys":10},"DateGreaterThan":{"aws:CurrentTime":"2026-01-01T00:00:00Z"},
			"Bool":{"aws:SecureTransport":true},"IpAddress":{"aws:SourceIp":["10.0.0.0/8","2001:db8::/32"]},"Null":{"s3:x":"false"},
			"ArnLike":{"aws:SourceArn":"arn:aws:sns:*:*:t"},"ForAllValues:StringEquals":{"aws:TagKeys":["a"]},"StringEqualsIfExists":{"aws:Referer":"x"},
			"NumericLessThanIfExists":{"s3:signatureAge":"${s3:limit}"}}}`),
			func(t *testing.T, p *domain.Policy) {
				assert.Len(t, p.Statement[0].Condition, 10)
				assert.Equal(t, domain.StringSet{"10"}, p.Statement[0].Condition["NumericLessThan"]["s3:max-keys"])
				assert.Equal(t, domain.StringSet{"true"}, p.Statement[0].Condition["Bool"]["aws:SecureTransport"])
			}},
		{"admin statement without resource", doc(`{"Effect":"Allow","Action":["admin:ServerInfo","admin:*"]}`), nil},
		{"sts and kms actions", doc(`{"Effect":"Allow","Action":["sts:AssumeRole","kms:*"]}`), nil},
		{"star action with resource", doc(`{"Effect":"Allow","Action":"*","Resource":"*"}`), nil},
		{"wildcard s3 action matching known actions", doc(`{"Effect":"Allow","Action":["s3:Get*","s3:*Object","s3:List?ucket"],"Resource":"*"}`), nil},
		{"MinIO extension actions", doc(`{"Effect":"Allow","Action":["s3:ForceDeleteBucket","s3:ListenBucketNotification"],"Resource":"*"}`), nil},
		{"variables in resources", doc(`{"Effect":"Allow","Action":"s3:GetObject","Resource":["arn:aws:s3:::home/${aws:username}/*","arn:aws:s3:::x/${*}${?}${$}","arn:aws:s3:::d/${aws:userid, 'none'}"]}`), nil},
		{"whitespace and unicode", "\n\t{ \"Version\" : \"2012-10-17\",\n \"Statement\" : [ { \"Sid\": \"città\", \"Effect\":\"Allow\", \"Action\":\"s3:GetObject\", \"Resource\":\"arn:aws:s3:::b/città/*\" } ] }\n", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := svc.Parse(ctx, []byte(tc.doc))
			require.NoError(t, err)
			require.NoError(t, svc.Validate(ctx, p))
			if tc.check != nil {
				tc.check(t, p)
			}
			// The parsed document survives a JSON round trip.
			data, err := json.Marshal(p)
			require.NoError(t, err)
			again, err := svc.Parse(ctx, data)
			require.NoError(t, err)
			assert.Equal(t, p, again)
		})
	}
}

func TestParseInvalid(t *testing.T) {
	ctx := context.Background()
	svc, _ := newEngine(t)
	stmt := func(body string) string { return doc(body) }
	tests := []struct {
		name string
		doc  string
		// fragment expected in the error message.
		want string
		// statement index (1-based) and line expected in the error, when > 0.
		statement, line, column int
	}{
		{name: "empty", doc: ``, want: "empty"},
		{name: "blank", doc: "  \n ", want: "empty"},
		{name: "not json", doc: `{`, want: "invalid JSON", line: 1},
		{name: "syntax error position", doc: "{\n  \"Version\": \"2012-10-17\",\n  \"Statement\": [}\n}", want: "invalid JSON", line: 3, column: 17},
		{name: "trailing data", doc: `{"Version":"2012-10-17","Statement":[]} {}`, want: "unexpected data", line: 1, column: 41},
		{name: "array document", doc: `[{"Version":"2012-10-17"}]`, want: "JSON object"},
		{name: "invalid utf-8", doc: "{\"Version\":\"\xff\"}", want: "UTF-8"},
		{name: "duplicate key", doc: `{"Version":"2012-10-17","Version":"2012-10-17","Statement":[]}`, want: `duplicate key "Version"`, line: 1, column: 25},
		{name: "duplicate statement key", doc: stmt(`{"Effect":"Deny","Effect":"Allow","Action":"s3:*","Resource":"*"}`), want: `duplicate key "Effect"`},
		{name: "unknown element", doc: `{"Version":"2012-10-17","Statement":[],"Foo":1}`, want: "Foo: unknown policy element"},
		{name: "element case-sensitive", doc: `{"version":"2012-10-17","Statement":[]}`, want: "version: unknown policy element"},
		{name: "version not string", doc: `{"Version":2012,"Statement":[]}`, want: "Version: must be a string"},
		{name: "id not string", doc: `{"Version":"2012-10-17","Id":1,"Statement":[]}`, want: "Id: must be a string"},
		{name: "wrong version", doc: `{"Version":"2008-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`, want: "Version: must be"},
		{name: "missing version", doc: `{"Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`, want: "Version: must be"},
		{name: "missing statement", doc: `{"Version":"2012-10-17"}`, want: "Statement: is required"},
		{name: "statement null", doc: `{"Version":"2012-10-17","Statement":null}`, want: "Statement: is required"},
		{name: "statement string", doc: `{"Version":"2012-10-17","Statement":"x"}`, want: "Statement: must be an object"},
		{name: "statement element not object", doc: `{"Version":"2012-10-17","Statement":[1]}`, want: "statement 1", statement: 1},
		{name: "no statements", doc: `{"Version":"2012-10-17","Statement":[]}`, want: "at least one statement"},
		{name: "unknown statement element", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","Filter":{}}`), want: "Filter: unknown statement element", statement: 1, line: 1, column: 38},
		{name: "statement element case-sensitive", doc: stmt(`{"effect":"Allow","Action":"s3:GetObject","Resource":"*"}`), want: "effect: unknown statement element", statement: 1},
		{name: "sid not string", doc: stmt(`{"Sid":1,"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}`), want: "Sid: must be a string", statement: 1},
		{name: "effect not string", doc: stmt(`{"Effect":true,"Action":"s3:GetObject","Resource":"*"}`), want: "Effect: must be a string", statement: 1},
		{name: "bad effect", doc: stmt(`{"Effect":"Maybe","Action":"s3:GetObject","Resource":"*"}`), want: "Effect: must be", statement: 1},
		{name: "lowercase effect", doc: stmt(`{"Effect":"allow","Action":"s3:GetObject","Resource":"*"}`), want: "Effect: must be", statement: 1},
		{name: "no action", doc: stmt(`{"Effect":"Allow","Resource":"*"}`), want: "exactly one of Action and NotAction", statement: 1},
		{name: "action and notaction", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","NotAction":"s3:PutObject","Resource":"*"}`), want: "exactly one of Action and NotAction"},
		{name: "action number", doc: stmt(`{"Effect":"Allow","Action":1,"Resource":"*"}`), want: "Action: must be a string or an array", statement: 1},
		{name: "action empty array", doc: stmt(`{"Effect":"Allow","Action":[],"Resource":"*"}`), want: "Action: must not be empty"},
		{name: "action array of numbers", doc: stmt(`{"Effect":"Allow","Action":[1],"Resource":"*"}`), want: "Action: must be a string or an array"},
		{name: "unknown service", doc: stmt(`{"Effect":"Allow","Action":"ec2:RunInstances","Resource":"*"}`), want: `unknown service "ec2"`},
		{name: "action without service", doc: stmt(`{"Effect":"Allow","Action":"GetObject","Resource":"*"}`), want: "expected <service>:<action>"},
		{name: "action empty name", doc: stmt(`{"Effect":"Allow","Action":"s3:","Resource":"*"}`), want: "expected <service>:<action>"},
		{name: "action invalid characters", doc: stmt(`{"Effect":"Allow","Action":"s3:Get Object","Resource":"*"}`), want: "invalid action"},
		{name: "unknown s3 action", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObjekt","Resource":"*"}`), want: `unknown S3 action "s3:GetObjekt"`},
		{name: "s3 wildcard matching nothing", doc: stmt(`{"Effect":"Allow","Action":"s3:Gte*","Resource":"*"}`), want: "unknown S3 action"},
		{name: "missing resource", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject"}`), want: "Resource or NotResource is required"},
		{name: "missing resource with NotAction", doc: stmt(`{"Effect":"Allow","NotAction":"admin:*"}`), want: "Resource or NotResource is required"},
		{name: "missing resource with star", doc: stmt(`{"Effect":"Allow","Action":"*"}`), want: "Resource or NotResource is required"},
		{name: "bad resource", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:iam::123:user/x"}`), want: "invalid resource"},
		{name: "empty resource ARN", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::"}`), want: "invalid resource"},
		{name: "resource object", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":{"a":1}}`), want: "Resource: must be a string"},
		{name: "unterminated variable", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/${aws:username"}`), want: "invalid policy variable"},
		{name: "empty variable", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/${}"}`), want: "invalid policy variable"},
		{name: "unquoted default", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/${aws:username, x}"}`), want: "invalid policy variable"},
		{name: "both resources", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","NotResource":"*"}`), want: "mutually exclusive"},
		{name: "both principals", doc: stmt(`{"Effect":"Allow","Principal":"*","NotPrincipal":"*","Action":"s3:GetObject","Resource":"*"}`), want: "mutually exclusive"},
		{name: "principal string not star", doc: stmt(`{"Effect":"Allow","Principal":"alice","Action":"s3:GetObject","Resource":"*"}`), want: `Principal: must be "*"`},
		{name: "principal number", doc: stmt(`{"Effect":"Allow","Principal":1,"Action":"s3:GetObject","Resource":"*"}`), want: `Principal: must be "*"`},
		{name: "principal empty object", doc: stmt(`{"Effect":"Allow","Principal":{},"Action":"s3:GetObject","Resource":"*"}`), want: "at least one principal"},
		{name: "principal canonical user", doc: stmt(`{"Effect":"Allow","Principal":{"CanonicalUser":"x"},"Action":"s3:GetObject","Resource":"*"}`), want: "unsupported principal type"},
		{name: "principal empty name", doc: stmt(`{"Effect":"Allow","Principal":{"AWS":""},"Action":"s3:GetObject","Resource":"*"}`), want: "must not be empty"},
		{name: "principal AWS number", doc: stmt(`{"Effect":"Allow","Principal":{"AWS":1},"Action":"s3:GetObject","Resource":"*"}`), want: "AWS must be"},
		{name: "duplicate sid", doc: doc(`{"Sid":"a","Effect":"Allow","Action":"s3:GetObject","Resource":"*"}`, `{"Sid":"a","Effect":"Allow","Action":"s3:GetObject","Resource":"*"}`), want: `duplicate Sid "a"`, statement: 2, line: 1, column: 106},
		{name: "condition not object", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":[]}`), want: "Condition: must be an object"},
		{name: "unknown operator", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":{"StringMatches":{"a":"b"}}}`), want: `unknown condition operator "StringMatches"`},
		{name: "unknown qualifier", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":{"ForSomeValues:StringEquals":{"a":"b"}}}`), want: "unknown condition operator"},
		{name: "NullIfExists", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":{"NullIfExists":{"a":"true"}}}`), want: "unknown condition operator"},
		{name: "qualified Null", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":{"ForAllValues:Null":{"a":"true"}}}`), want: "unknown condition operator"},
		{name: "operator without keys", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":{"StringEquals":{}}}`), want: "has no condition keys"},
		{name: "operator not object", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":{"StringEquals":"x"}}`), want: "must be an object of condition keys"},
		{name: "condition value object", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":{"StringEquals":{"a":{"b":1}}}}`), want: "Condition.StringEquals.a: values must be"},
		{name: "condition empty values", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":{"StringEquals":{"a":[]}}}`), want: "at least one value"},
		{name: "condition key with spaces", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":{"StringEquals":{"a b":"c"}}}`), want: "invalid condition key"},
		{name: "numeric value", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":{"NumericLessThan":{"s3:max-keys":"ten"}}}`), want: `invalid value "ten" for NumericLessThan`},
		{name: "date value", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":{"DateLessThan":{"aws:CurrentTime":"tomorrow"}}}`), want: "invalid value"},
		{name: "bool value", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":{"Bool":{"aws:SecureTransport":"yes"}}}`), want: "invalid value"},
		{name: "null value", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":{"Null":{"s3:x":"maybe"}}}`), want: "invalid value"},
		{name: "ip value", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":{"IpAddress":{"aws:SourceIp":"10.0.0.0/33"}}}`), want: "invalid value"},
		{name: "ip value name", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":{"NotIpAddress":{"aws:SourceIp":"localhost"}}}`), want: "invalid value"},
		{name: "variable in condition", doc: stmt(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":{"StringEquals":{"s3:x":"${oops"}}}`), want: "invalid policy variable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Parse(ctx, []byte(tc.doc))
			require.Error(t, err)
			assert.ErrorIs(t, err, domain.ErrMalformedPolicy)
			assert.ErrorIs(t, err, domain.ErrInvalidArgument)
			assert.NotErrorIs(t, err, domain.ErrPolicyTooLarge)
			assert.Contains(t, err.Error(), tc.want)
			var s3err *domain.S3Error
			require.ErrorAs(t, err, &s3err)
			assert.Equal(t, "MalformedPolicy", s3err.Code)
			assert.Equal(t, err.Error(), s3err.Message)
			var ve *policy.ValidationError
			require.ErrorAs(t, err, &ve)
			if tc.statement > 0 {
				assert.Equal(t, tc.statement, ve.Statement)
			}
			if tc.line > 0 {
				assert.Equal(t, tc.line, ve.Line, err.Error())
			}
			if tc.column > 0 {
				assert.Equal(t, tc.column, ve.Column, err.Error())
			}
		})
	}
}

func TestParseErrorPositions(t *testing.T) {
	ctx := context.Background()
	svc, _ := newEngine(t)
	raw := `{
  "Version": "2012-10-17",
  "Statement": [
    {"Sid": "ok", "Effect": "Allow", "Action": "s3:GetObject", "Resource": "*"},
    {
      "Sid": "bad",
      "Effect": "Allow",
      "Action": "s3:Nope",
      "Resource": "*"
    }
  ]
}`
	_, err := svc.Parse(ctx, []byte(raw))
	var ve *policy.ValidationError
	require.ErrorAs(t, err, &ve)
	assert.Equal(t, 2, ve.Statement)
	assert.Equal(t, "bad", ve.Sid)
	assert.Equal(t, "Action", ve.Element)
	assert.Equal(t, 5, ve.Line)
	assert.Equal(t, 5, ve.Column)
	assert.Equal(t, `statement 2 (Sid "bad"), line 5, column 5: Action: unknown S3 action "s3:Nope"`, err.Error())

	// Validate on a decoded document reports the statement without a position.
	var p domain.Policy
	require.NoError(t, json.Unmarshal([]byte(raw), &p))
	err = svc.Validate(ctx, &p)
	require.ErrorAs(t, err, &ve)
	assert.Equal(t, 2, ve.Statement)
	assert.Zero(t, ve.Line)

	// A multi-byte character counts as one column.
	_, err = svc.Parse(ctx, []byte(`{"Version":"2012-10-17","Statement":{"Sid":"è","Effect":"Allow","Action":"s3:GetObject","Resource":"*"}`+"\n"+`,}`))
	require.ErrorAs(t, err, &ve)
	assert.Equal(t, 2, ve.Line)
}

func TestParseSizeLimit(t *testing.T) {
	ctx := context.Background()
	svc, _ := newEngine(t)
	big := doc(`{"Sid":"` + strings.Repeat("x", 21<<10) + `","Effect":"Allow","Action":"s3:GetObject","Resource":"*"}`)
	_, err := svc.Parse(ctx, []byte(big))
	assert.ErrorIs(t, err, domain.ErrPolicyTooLarge)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	assert.NotErrorIs(t, err, domain.ErrMalformedPolicy)
	var s3err *domain.S3Error
	require.ErrorAs(t, err, &s3err)
	assert.Equal(t, "PolicyTooLarge", s3err.Code)
	assert.Contains(t, err.Error(), "limit is 20480")

	// Validate measures the compact JSON form.
	p := mustPolicy(t, big)
	assert.ErrorIs(t, svc.Validate(ctx, p), domain.ErrPolicyTooLarge)

	small := policy.New(policy.Deps{}, policy.WithMaxPolicySize(90), policy.WithMaxPolicySize(-1))
	_, err = small.Parse(ctx, []byte(doc(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}`)))
	assert.ErrorIs(t, err, domain.ErrPolicyTooLarge)
	_, err = small.Parse(ctx, []byte(doc(`{"Effect":"Allow","Action":"s3:*","Resource":"*"}`)))
	require.NoError(t, err)
}

func TestValidateDecoded(t *testing.T) {
	ctx := context.Background()
	svc, _ := newEngine(t)
	assert.ErrorIs(t, svc.Validate(ctx, nil), domain.ErrMalformedPolicy)
	err := svc.Validate(ctx, &domain.Policy{})
	var s3err *domain.S3Error
	require.ErrorAs(t, err, &s3err)
	assert.Equal(t, "MalformedPolicy", s3err.Code)
	// Decoded documents may carry what the strict parser rejects.
	p := &domain.Policy{Version: domain.PolicyVersion, Statement: domain.Statements{{
		Effect: domain.Allow, Action: domain.StringSet{"s3:GetObject"}, Resource: domain.StringSet{"*"}, Principal: &domain.Principal{},
	}}}
	assert.ErrorIs(t, svc.Validate(ctx, p), domain.ErrMalformedPolicy)
	p.Statement[0].Principal = &domain.Principal{AWS: domain.StringSet{" "}}
	assert.ErrorIs(t, svc.Validate(ctx, p), domain.ErrMalformedPolicy)
	p.Statement[0].Principal = nil
	p.Statement[0].Condition = domain.Condition{"StringEquals": {"k": {}}}
	assert.ErrorContains(t, svc.Validate(ctx, p), "at least one value")
	p.Statement[0].Condition = domain.Condition{"StringEquals": {"": {"v"}}}
	assert.ErrorContains(t, svc.Validate(ctx, p), "invalid condition key")
	p.Statement[0].Condition = nil
	require.NoError(t, svc.Validate(ctx, p))
}

func TestParseBucketPolicy(t *testing.T) {
	ctx := context.Background()
	svc, _ := newEngine(t)
	valid := doc(
		`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::photos/*"}`,
		`{"Effect":"Allow","NotPrincipal":{"AWS":"bob"},"Action":"*","Resource":["arn:aws:s3:::photos","arn:aws:s3:::photos/a/${aws:username}/*"]}`,
	)
	p, err := svc.ParseBucketPolicy(ctx, "photos", []byte(valid))
	require.NoError(t, err)
	require.Len(t, p.Statement, 2)
	require.NoError(t, svc.ValidateBucketPolicy(ctx, "photos", p))

	tests := []struct {
		name, bucket, doc, want string
	}{
		{"no principal", "photos", doc(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::photos/*"}`), "must name a Principal"},
		{"admin action", "photos", doc(`{"Effect":"Allow","Principal":"*","Action":"admin:ServerInfo","Resource":"arn:aws:s3:::photos"}`), "not an S3 action"},
		{"admin NotAction", "photos", doc(`{"Effect":"Deny","Principal":"*","NotAction":"admin:*","Resource":"arn:aws:s3:::photos"}`), "not an S3 action"},
		{"other bucket", "photos", doc(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::other/*"}`), "outside bucket photos"},
		{"bucket prefix is not the bucket", "photos", doc(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::photos2/*"}`), "outside bucket"},
		{"star resource", "photos", doc(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"*"}`), "outside bucket"},
		{"all buckets", "photos", doc(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","NotResource":"arn:aws:s3:::*"}`), "outside bucket"},
		{"syntax", "photos", `{"Version":`, "invalid JSON"},
		{"generic validation", "photos", doc(`{"Effect":"Allow","Principal":"*","Action":"s3:Nope","Resource":"arn:aws:s3:::photos/*"}`), "unknown S3 action"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.ParseBucketPolicy(ctx, tc.bucket, []byte(tc.doc))
			assert.ErrorIs(t, err, domain.ErrMalformedPolicy)
			assert.ErrorContains(t, err, tc.want)
		})
	}
	_, err = svc.ParseBucketPolicy(ctx, "", []byte(valid))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = svc.ParseBucketPolicy(ctx, "pho*", []byte(valid))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	assert.ErrorIs(t, svc.ValidateBucketPolicy(ctx, "photos", nil), domain.ErrMalformedPolicy)
	assert.ErrorIs(t, svc.ValidateBucketPolicy(ctx, "other", p), domain.ErrMalformedPolicy)

	// The position of the offending statement is reported.
	_, err = svc.ParseBucketPolicy(ctx, "photos", []byte("{\"Version\":\"2012-10-17\",\"Statement\":[\n{\"Effect\":\"Allow\",\"Action\":\"s3:GetObject\",\"Resource\":\"arn:aws:s3:::photos/*\"}]}"))
	var ve *policy.ValidationError
	require.ErrorAs(t, err, &ve)
	assert.Equal(t, 2, ve.Line)
	assert.Equal(t, 1, ve.Column)
}

func TestValidationErrorFormat(t *testing.T) {
	tests := []struct {
		err  *policy.ValidationError
		want string
	}{
		{&policy.ValidationError{Msg: "boom"}, "boom"},
		{&policy.ValidationError{Element: "Version", Msg: "bad"}, "Version: bad"},
		{&policy.ValidationError{Line: 2, Column: 3, Msg: "bad"}, "line 2, column 3: bad"},
		{&policy.ValidationError{Statement: 1, Element: "Action", Msg: "bad"}, "statement 1: Action: bad"},
		{&policy.ValidationError{Statement: 4, Sid: "s", Line: 9, Column: 1, Element: "Resource", Msg: "bad"}, `statement 4 (Sid "s"), line 9, column 1: Resource: bad`},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, tc.err.Error())
		assert.True(t, errors.Is(tc.err, domain.ErrMalformedPolicy))
	}
}
