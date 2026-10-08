package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"unicode/utf8"

	"github.com/r3n3r0/nexo/internal/domain"
)

// defaultMaxPolicySize is the AWS bucket policy size limit.
const defaultMaxPolicySize = 20 << 10

// Elements of a policy document and of a statement (case-sensitive, as in
// AWS).
func isPolicyElement(k string) bool {
	switch k {
	case "Version", "Id", "Statement":
		return true
	}
	return false
}

func isStatementElement(k string) bool {
	switch k {
	case "Sid", "Effect", "Principal", "NotPrincipal", "Action", "NotAction", "Resource", "NotResource", "Condition":
		return true
	}
	return false
}

// parseDocument decodes a JSON policy document strictly: valid JSON with a
// single top-level object, no duplicate keys, only the known elements
// (case-sensitive) with the expected JSON types. Errors are
// *ValidationError with the line and column of the offending text.
func parseDocument(raw []byte, maxSize int) (*domain.Policy, []int, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil, malformed("policy document is empty")
	}
	if len(raw) > maxSize {
		return nil, nil, &ValidationError{Msg: fmt.Sprintf("policy document is %d bytes, the limit is %d", len(raw), maxSize), tooLarge: true}
	}
	if !utf8.Valid(raw) {
		return nil, nil, malformed("policy document is not valid UTF-8")
	}
	offsets, err := scanJSON(raw)
	if err != nil {
		return nil, nil, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, nil, malformed("policy document must be a JSON object")
	}
	for _, k := range sortedKeys(top) {
		if !isPolicyElement(k) {
			return nil, nil, &ValidationError{Element: k, Msg: "unknown policy element"}
		}
	}
	var p domain.Policy
	if v, ok := top["Version"]; ok {
		if err := json.Unmarshal(v, &p.Version); err != nil {
			return nil, nil, &ValidationError{Element: "Version", Msg: "must be a string"}
		}
	}
	if v, ok := top["Id"]; ok {
		if err := json.Unmarshal(v, &p.ID); err != nil {
			return nil, nil, &ValidationError{Element: "Id", Msg: "must be a string"}
		}
	}
	rawStmts, err := statementList(top["Statement"])
	if err != nil {
		return nil, nil, err
	}
	p.Statement = make(domain.Statements, 0, len(rawStmts))
	for i, rs := range rawStmts {
		st, err := parseStatement(i, rs)
		if err != nil {
			locate(err, i, offsets, raw)
			return nil, nil, err
		}
		p.Statement = append(p.Statement, st)
	}
	return &p, offsets, nil
}

// statementList splits the Statement element, a single object or an array
// of objects.
func statementList(raw json.RawMessage) ([]json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, &ValidationError{Element: "Statement", Msg: "is required"}
	}
	if raw[0] == '{' {
		return []json.RawMessage{raw}, nil
	}
	var list []json.RawMessage
	if raw[0] != '[' || json.Unmarshal(raw, &list) != nil {
		return nil, &ValidationError{Element: "Statement", Msg: "must be an object or an array of objects"}
	}
	for i, s := range list {
		if s = bytes.TrimSpace(s); len(s) == 0 || s[0] != '{' {
			return nil, &ValidationError{Statement: i + 1, Element: "Statement", Msg: "must be an object"}
		}
	}
	return list, nil
}

// parseStatement decodes one statement element by element.
func parseStatement(i int, raw json.RawMessage) (domain.Statement, error) {
	var fields map[string]json.RawMessage
	var st domain.Statement
	if err := json.Unmarshal(raw, &fields); err != nil {
		return st, stmtError(i, "", "", "statement must be an object")
	}
	if v, ok := fields["Sid"]; ok {
		if err := json.Unmarshal(v, &st.SID); err != nil {
			return st, stmtError(i, "", "Sid", "must be a string")
		}
	}
	for _, k := range sortedKeys(fields) {
		if !isStatementElement(k) {
			return st, stmtError(i, st.SID, k, "unknown statement element")
		}
	}
	if v, ok := fields["Effect"]; ok {
		var e string
		if err := json.Unmarshal(v, &e); err != nil {
			return st, stmtError(i, st.SID, "Effect", "must be a string")
		}
		st.Effect = domain.Effect(e)
	}
	for _, el := range []struct {
		name string
		dst  **domain.Principal
	}{{"Principal", &st.Principal}, {"NotPrincipal", &st.NotPrincipal}} {
		v, ok := fields[el.name]
		if !ok {
			continue
		}
		p, err := parsePrincipal(v)
		if err != nil {
			return st, stmtError(i, st.SID, el.name, "%s", err.Error())
		}
		*el.dst = p
	}
	for _, el := range []struct {
		name string
		dst  *domain.StringSet
	}{{"Action", &st.Action}, {"NotAction", &st.NotAction}, {"Resource", &st.Resource}, {"NotResource", &st.NotResource}} {
		v, ok := fields[el.name]
		if !ok {
			continue
		}
		set, err := parseStrings(v)
		if err != nil {
			return st, stmtError(i, st.SID, el.name, "%s", err.Error())
		}
		*el.dst = set
	}
	if v, ok := fields["Condition"]; ok {
		cond, element, err := parseCondition(v)
		if err != nil {
			return st, stmtError(i, st.SID, element, "%s", err.Error())
		}
		st.Condition = cond
	}
	return st, nil
}

// parseStrings decodes a string or a non-empty array of strings.
func parseStrings(raw json.RawMessage) (domain.StringSet, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, errors.New("must be a string or an array of strings")
		}
		return domain.StringSet{s}, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil || list == nil {
		return nil, errors.New("must be a string or an array of strings")
	}
	if len(list) == 0 {
		return nil, errors.New("must not be empty")
	}
	return list, nil
}

// parsePrincipal decodes "*" or an object of AWS/Service/Federated
// principals.
func parsePrincipal(raw json.RawMessage) (*domain.Principal, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || s != "*" {
			return nil, errors.New(`must be "*" or an object such as {"AWS": [...]}`)
		}
		return &domain.Principal{AWS: domain.StringSet{"*"}}, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, errors.New(`must be "*" or an object such as {"AWS": [...]}`)
	}
	if len(fields) == 0 {
		return nil, errors.New("must name at least one principal")
	}
	var p domain.Principal
	for _, k := range sortedKeys(fields) {
		var dst *domain.StringSet
		switch k {
		case "AWS":
			dst = &p.AWS
		case "Service":
			dst = &p.Service
		case "Federated":
			dst = &p.Federated
		default:
			return nil, fmt.Errorf("unsupported principal type %q (AWS, Service, Federated)", k)
		}
		set, err := parseStrings(fields[k])
		if err != nil {
			return nil, fmt.Errorf("%s %w", k, err)
		}
		*dst = set
	}
	return &p, nil
}

// parseCondition decodes the Condition element: operator → key → value or
// array of values (strings, numbers or booleans).
func parseCondition(raw json.RawMessage) (domain.Condition, string, error) {
	var ops map[string]json.RawMessage
	if err := json.Unmarshal(raw, &ops); err != nil || ops == nil {
		return nil, "Condition", errors.New("must be an object of operators")
	}
	cond := make(domain.Condition, len(ops))
	for _, op := range sortedKeys(ops) {
		element := "Condition." + op
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(ops[op], &keys); err != nil || keys == nil {
			return nil, element, errors.New("must be an object of condition keys")
		}
		values := make(map[string]domain.StringSet, len(keys))
		for _, k := range sortedKeys(keys) {
			var set domain.StringSet
			if err := json.Unmarshal(keys[k], &set); err != nil {
				return nil, element + "." + k, errors.New("values must be strings, numbers or booleans")
			}
			if len(set) == 0 {
				return nil, element + "." + k, errors.New("must have at least one value")
			}
			values[k] = set
		}
		cond[op] = values
	}
	if len(cond) == 0 {
		return nil, "", nil // "Condition": {} is no condition
	}
	return cond, "", nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// locate adds the position of statement i to a validation error.
func locate(err error, i int, offsets []int, raw []byte) {
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Line > 0 || i < 0 || i >= len(offsets) {
		return
	}
	ve.Line, ve.Column = lineColumn(raw, offsets[i])
}

// lineColumn converts a byte offset into a 1-based line and column (in
// runes).
func lineColumn(raw []byte, offset int) (int, int) {
	if offset > len(raw) {
		offset = len(raw)
	}
	before := raw[:offset]
	line := bytes.Count(before, []byte("\n")) + 1
	start := bytes.LastIndexByte(before, '\n') + 1
	return line, utf8.RuneCount(before[start:]) + 1
}

// jsonFrame is an open object or array during scanJSON.
type jsonFrame struct {
	object    bool
	keys      map[string]bool
	expectKey bool
	// statements marks the frame holding the statements: the top-level
	// object while the "Statement" value is pending, or its array.
	statements bool
}

// scanJSON checks the syntax of a document (a single JSON value, no
// duplicate keys) and returns the byte offsets of the statements. Errors
// carry the line and column.
func scanJSON(raw []byte) ([]int, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var (
		stack     []*jsonFrame
		offsets   []int
		values    int
		pendingSt bool // the next value is the Statement element
	)
	fail := func(offset int64, format string, args ...any) error {
		line, col := lineColumn(raw, tokenStart(raw, int(offset)))
		return &ValidationError{Line: line, Column: col, Msg: fmt.Sprintf(format, args...)}
	}
	// valueDone marks the end of a value in the enclosing frame.
	valueDone := func() {
		if len(stack) > 0 && stack[len(stack)-1].object {
			stack[len(stack)-1].expectKey = true
		}
	}
	for {
		start := dec.InputOffset()
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var syn *json.SyntaxError
			if errors.As(err, &syn) {
				return nil, fail(syn.Offset, "invalid JSON: %s", syn.Error())
			}
			return nil, fail(dec.InputOffset(), "invalid JSON: %s", err.Error())
		}
		if len(stack) == 0 {
			values++
			if values > 1 {
				return nil, fail(start, "invalid JSON: unexpected data after the policy document")
			}
		}
		var top *jsonFrame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if key, isKey := tok.(string); isKey && top != nil && top.object && top.expectKey {
			if top.keys[key] {
				return nil, fail(start, "duplicate key %q", key)
			}
			top.keys[key] = true
			top.expectKey = false
			pendingSt = len(stack) == 1 && key == "Statement"
			continue
		}
		statementValue := pendingSt
		pendingSt = false
		switch tok {
		case json.Delim('{'):
			if statementValue || (top != nil && top.statements && !top.object) {
				offsets = append(offsets, statementStart(raw, int(start)))
			}
			stack = append(stack, &jsonFrame{object: true, keys: map[string]bool{}, expectKey: true})
		case json.Delim('['):
			stack = append(stack, &jsonFrame{statements: statementValue})
		case json.Delim('}'), json.Delim(']'):
			stack = stack[:len(stack)-1]
			valueDone()
		default:
			valueDone()
		}
	}
	if len(stack) > 0 {
		return nil, fail(int64(len(raw)), "invalid JSON: unexpected end of the document")
	}
	return offsets, nil
}

// tokenStart skips the whitespace and separators before a token.
func tokenStart(raw []byte, offset int) int {
	for offset < len(raw) && bytes.IndexByte([]byte(" \t\r\n,:"), raw[offset]) >= 0 {
		offset++
	}
	return offset
}

// statementStart skips the separators before a statement's opening brace.
func statementStart(raw []byte, offset int) int {
	for offset < len(raw) && raw[offset] != '{' {
		offset++
	}
	return offset
}
