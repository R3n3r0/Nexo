package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// PolicyVersion is the only supported policy document version.
const PolicyVersion = "2012-10-17"

// Effect is the Allow/Deny effect of a statement.
type Effect string

// Statement effects.
const (
	Allow Effect = "Allow"
	Deny  Effect = "Deny"
)

// IsValid reports whether the effect is Allow or Deny.
func (e Effect) IsValid() bool { return e == Allow || e == Deny }

// StringSet is a JSON value that may be encoded as a single scalar or as an
// array of scalars, as AWS policy documents allow. Booleans and numbers are
// converted to their string form.
type StringSet []string

// UnmarshalJSON implements json.Unmarshaler.
func (s *StringSet) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		*s = nil
		return nil
	}
	if b[0] == '[' {
		var raw []json.RawMessage
		if err := json.Unmarshal(b, &raw); err != nil {
			return err
		}
		out := make(StringSet, 0, len(raw))
		for _, r := range raw {
			v, err := scalarToString(r)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		*s = out
		return nil
	}
	v, err := scalarToString(b)
	if err != nil {
		return err
	}
	*s = StringSet{v}
	return nil
}

// MarshalJSON implements json.Marshaler: a single value is encoded as a
// string, several values as an array.
func (s StringSet) MarshalJSON() ([]byte, error) {
	if len(s) == 1 {
		return json.Marshal(s[0])
	}
	return json.Marshal([]string(s))
}

// Contains reports whether v is in the set.
func (s StringSet) Contains(v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func scalarToString(b []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	switch t := v.(type) {
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case json.Number:
		return t.String(), nil
	default:
		return "", fmt.Errorf("%w: policy value must be a string, number or boolean", ErrInvalidArgument)
	}
}

// Principal is the Principal/NotPrincipal element. "*" is decoded as
// {"AWS": ["*"]}.
type Principal struct {
	AWS       StringSet `json:"AWS,omitempty"`
	Service   StringSet `json:"Service,omitempty"`
	Federated StringSet `json:"Federated,omitempty"`
}

// UnmarshalJSON implements json.Unmarshaler.
func (p *Principal) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s != "*" {
			return fmt.Errorf("%w: principal must be \"*\" or an object", ErrInvalidArgument)
		}
		*p = Principal{AWS: StringSet{"*"}}
		return nil
	}
	type alias Principal
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*p = Principal(a)
	return nil
}

// IsWildcard reports whether the principal matches everyone.
func (p *Principal) IsWildcard() bool { return p != nil && p.AWS.Contains("*") }

// Condition maps a condition operator (e.g. "StringEquals") to the condition
// keys and their values.
type Condition map[string]map[string]StringSet

// Statement is a single policy statement.
type Statement struct {
	SID          string     `json:"Sid,omitempty"`
	Effect       Effect     `json:"Effect"`
	Principal    *Principal `json:"Principal,omitempty"`
	NotPrincipal *Principal `json:"NotPrincipal,omitempty"`
	Action       StringSet  `json:"Action,omitempty"`
	NotAction    StringSet  `json:"NotAction,omitempty"`
	Resource     StringSet  `json:"Resource,omitempty"`
	NotResource  StringSet  `json:"NotResource,omitempty"`
	Condition    Condition  `json:"Condition,omitempty"`
}

// Statements is the Statement element, which AWS allows to be a single
// object or an array.
type Statements []Statement

// UnmarshalJSON implements json.Unmarshaler.
func (s *Statements) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '{' {
		var one Statement
		if err := json.Unmarshal(b, &one); err != nil {
			return err
		}
		*s = Statements{one}
		return nil
	}
	var many []Statement
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*s = many
	return nil
}

// Policy is an AWS-format policy document (IAM policy or bucket policy).
type Policy struct {
	ID        string     `json:"Id,omitempty"`
	Version   string     `json:"Version"`
	Statement Statements `json:"Statement"`
}

// IsEmpty reports whether the policy has no statements.
func (p *Policy) IsEmpty() bool { return p == nil || len(p.Statement) == 0 }
