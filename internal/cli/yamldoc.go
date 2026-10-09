package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/r3n3r0/nexo/internal/domain"
)

// The YAML documents of the GitOps commands (access export|import, dist
// export|import) are the JSON documents of the Admin API written as YAML:
// the conversion keeps the order of the members and every member, so that
// an exported document imports back unchanged.

// JSONToYAML converts a JSON document to YAML, keeping the order of the
// members of the objects.
func JSONToYAML(data []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	node, err := jsonToNode(dec)
	if err != nil {
		return nil, fmt.Errorf("%w: JSON to YAML: %v", domain.ErrInvalidArgument, err)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(node); err != nil {
		return nil, fmt.Errorf("encode YAML: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode YAML: %w", err)
	}
	return buf.Bytes(), nil
}

// jsonToNode reads one JSON value as a YAML node.
func jsonToNode(dec *json.Decoder) (*yaml.Node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := tok.(type) {
	case json.Delim:
		switch v {
		case '{':
			n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyTok.(string)
				if !ok {
					return nil, fmt.Errorf("object key %v is not a string", keyTok)
				}
				val, err := jsonToNode(dec)
				if err != nil {
					return nil, err
				}
				n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, val)
			}
			_, err := dec.Token()
			return n, err
		case '[':
			n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			for dec.More() {
				val, err := jsonToNode(dec)
				if err != nil {
					return nil, err
				}
				n.Content = append(n.Content, val)
			}
			_, err := dec.Token()
			return n, err
		}
		return nil, fmt.Errorf("unexpected %v", v)
	case string:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}, nil
	case json.Number:
		if _, err := v.Int64(); err == nil {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: v.String()}, nil
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: v.String()}, nil
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(v)}, nil
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	}
	return nil, fmt.Errorf("unexpected token %v", tok)
}

// YAMLToJSON converts every document of a YAML stream (or a JSON
// document, which is YAML) to JSON, keeping the order of the members. A
// syntax error is domain.ErrInvalidArgument with its line.
func YAMLToJSON(data []byte) ([][]byte, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out [][]byte
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: invalid YAML: %v", domain.ErrInvalidArgument, err)
		}
		if len(doc.Content) == 0 {
			continue
		}
		var buf bytes.Buffer
		if err := nodeToJSON(&buf, doc.Content[0]); err != nil {
			return nil, fmt.Errorf("%w: YAML to JSON: %v", domain.ErrInvalidArgument, err)
		}
		out = append(out, buf.Bytes())
	}
	return out, nil
}

// nodeToJSON writes a YAML node as JSON.
func nodeToJSON(buf *bytes.Buffer, n *yaml.Node) error {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			buf.WriteString("null")
			return nil
		}
		return nodeToJSON(buf, n.Content[0])
	case yaml.AliasNode:
		return nodeToJSON(buf, n.Alias)
	case yaml.MappingNode:
		buf.WriteByte('{')
		for i := 0; i+1 < len(n.Content); i += 2 {
			if i > 0 {
				buf.WriteByte(',')
			}
			key, err := json.Marshal(n.Content[i].Value)
			if err != nil {
				return err
			}
			buf.Write(key)
			buf.WriteByte(':')
			if err := nodeToJSON(buf, n.Content[i+1]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		return nil
	case yaml.SequenceNode:
		buf.WriteByte('[')
		for i, c := range n.Content {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := nodeToJSON(buf, c); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	case yaml.ScalarNode:
		return scalarToJSON(buf, n)
	}
	return fmt.Errorf("line %d: unsupported YAML node", n.Line)
}

// scalarToJSON writes a YAML scalar with the JSON type of its resolved tag
// (timestamps stay strings).
func scalarToJSON(buf *bytes.Buffer, n *yaml.Node) error {
	switch n.ShortTag() {
	case "!!null":
		buf.WriteString("null")
		return nil
	case "!!bool":
		var b bool
		if err := n.Decode(&b); err != nil {
			return fmt.Errorf("line %d: %v", n.Line, err)
		}
		buf.WriteString(strconv.FormatBool(b))
		return nil
	case "!!int":
		var i int64
		if err := n.Decode(&i); err != nil {
			return fmt.Errorf("line %d: %v", n.Line, err)
		}
		buf.WriteString(strconv.FormatInt(i, 10))
		return nil
	case "!!float":
		var f float64
		if err := n.Decode(&f); err != nil {
			return fmt.Errorf("line %d: %v", n.Line, err)
		}
		data, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("line %d: %v", n.Line, err)
		}
		buf.Write(data)
		return nil
	}
	data, err := json.Marshal(n.Value)
	if err != nil {
		return err
	}
	buf.Write(data)
	return nil
}
