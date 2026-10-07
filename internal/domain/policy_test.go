package domain

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPolicyUnmarshalFlexibleShapes(t *testing.T) {
	const doc = `{
	  "Version": "2012-10-17",
	  "Statement": {
	    "Sid": "Public",
	    "Effect": "Allow",
	    "Principal": "*",
	    "Action": "s3:GetObject",
	    "Resource": ["arn:aws:s3:::public/*"],
	    "Condition": {
	      "Bool": {"aws:SecureTransport": true},
	      "NumericLessThan": {"s3:max-keys": 10},
	      "StringLike": {"s3:prefix": ["a/*", "b/*"]}
	    }
	  }
	}`
	var p Policy
	require.NoError(t, json.Unmarshal([]byte(doc), &p))
	require.Len(t, p.Statement, 1)
	st := p.Statement[0]
	assert.Equal(t, Allow, st.Effect)
	assert.True(t, st.Principal.IsWildcard())
	assert.Equal(t, StringSet{"s3:GetObject"}, st.Action)
	assert.Equal(t, StringSet{"arn:aws:s3:::public/*"}, st.Resource)
	assert.Equal(t, StringSet{"true"}, st.Condition["Bool"]["aws:SecureTransport"])
	assert.Equal(t, StringSet{"10"}, st.Condition["NumericLessThan"]["s3:max-keys"])
	assert.Equal(t, StringSet{"a/*", "b/*"}, st.Condition["StringLike"]["s3:prefix"])

	// Round trip keeps a valid, re-parsable document.
	out, err := json.Marshal(&p)
	require.NoError(t, err)
	var again Policy
	require.NoError(t, json.Unmarshal(out, &again))
	assert.Equal(t, p, again)
}

func TestPolicyUnmarshalArrayAndObjectPrincipal(t *testing.T) {
	const doc = `{"Version":"2012-10-17","Statement":[
	  {"Effect":"Deny","Principal":{"AWS":["arn:aws:iam::1:user/bob","arn:aws:iam::1:user/alice"]},"Action":["s3:*"],"Resource":"arn:aws:s3:::b"},
	  {"Effect":"Allow","NotAction":"s3:DeleteObject","NotResource":"arn:aws:s3:::b/secret/*"}
	]}`
	var p Policy
	require.NoError(t, json.Unmarshal([]byte(doc), &p))
	require.Len(t, p.Statement, 2)
	assert.Equal(t, Deny, p.Statement[0].Effect)
	assert.False(t, p.Statement[0].Principal.IsWildcard())
	assert.Len(t, p.Statement[0].Principal.AWS, 2)
	assert.Equal(t, StringSet{"s3:DeleteObject"}, p.Statement[1].NotAction)
	assert.False(t, p.IsEmpty())
}

func TestPolicyUnmarshalErrors(t *testing.T) {
	var p Policy
	assert.Error(t, json.Unmarshal([]byte(`{"Statement":[{"Effect":"Allow","Principal":"everyone"}]}`), &p))
	assert.Error(t, json.Unmarshal([]byte(`{"Statement":[{"Effect":"Allow","Action":[{"x":1}]}]}`), &p))
	var empty *Policy
	assert.True(t, empty.IsEmpty())
}
