package nexoadmin

import (
	"encoding/json"
	"reflect"
	"strings"
)

// The documents of the distributions keep the members this version of the
// client does not know (Extra), so that a read-modify-write (GET, change,
// PUT) never drops what a newer server stores: the cache settings of
// docs/11, for instance.

// marshalWithExtra encodes v (a struct without custom marshaling) and adds
// the members of extra it does not set.
func marshalWithExtra(v any, extra map[string]json.RawMessage) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil || len(extra) == 0 {
		return data, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	for k, raw := range extra {
		if _, set := m[k]; !set {
			m[k] = raw
		}
	}
	return json.Marshal(m)
}

// unmarshalWithExtra decodes data into v (a pointer to a struct without
// custom unmarshaling) and returns the members v does not know.
func unmarshalWithExtra(data []byte, v any) (map[string]json.RawMessage, error) {
	if err := json.Unmarshal(data, v); err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	for _, name := range jsonNames(reflect.TypeOf(v).Elem()) {
		delete(m, name)
	}
	if len(m) == 0 {
		return nil, nil
	}
	return m, nil
}

// jsonNames returns the JSON member names of the fields of struct type t.
func jsonNames(t reflect.Type) []string {
	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch name {
		case "-":
			continue
		case "":
			name = f.Name
		}
		out = append(out, name)
	}
	return out
}
