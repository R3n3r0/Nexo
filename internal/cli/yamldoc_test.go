package cli_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
)

func TestJSONToYAMLRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		json string
		yaml string
	}{
		{
			name: "order and types kept",
			json: `{"id":"site","enabled":true,"aliases":["*.preview.example.com","www.example.com"],"origin":{"bucket":"web","pathPrefix":"/prod"},"priority":10,"ratio":0.5,"code":"0755","none":null,"empty":{},"list":[]}`,
			yaml: "id: site\nenabled: true\naliases:\n  - '*.preview.example.com'\n  - www.example.com\norigin:\n  bucket: web\n  pathPrefix: /prod\npriority: 10\nratio: 0.5\ncode: \"0755\"\nnone: null\nempty: {}\nlist: []\n",
		},
		{
			name: "strings that look like other types",
			json: `{"a":"true","b":"10","c":"null","d":"2026-01-01T00:00:00Z","e":"^img/[0-9]{4}/.*\\.(jpe?g|png)$"}`,
			yaml: "a: \"true\"\nb: \"10\"\nc: \"null\"\nd: \"2026-01-01T00:00:00Z\"\ne: ^img/[0-9]{4}/.*\\.(jpe?g|png)$\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			y, err := cli.JSONToYAML([]byte(tc.json))
			require.NoError(t, err)
			assert.Equal(t, tc.yaml, string(y))
			docs, err := cli.YAMLToJSON(y)
			require.NoError(t, err)
			require.Len(t, docs, 1)
			assert.Equal(t, tc.json, string(docs[0]), "the conversion keeps order and types")
		})
	}
}

func TestYAMLToJSON(t *testing.T) {
	in := "# rules\nbucket: media\nrules:\n  - id: a\n    priority: 5\n    caseInsensitive: yes\n    from: 2026-01-01T00:00:00Z\n---\nid: second\n...\n"
	docs, err := cli.YAMLToJSON([]byte(in))
	require.NoError(t, err)
	require.Len(t, docs, 2)
	assert.JSONEq(t, `{"bucket":"media","rules":[{"id":"a","priority":5,"caseInsensitive":"yes","from":"2026-01-01T00:00:00Z"}]}`, string(docs[0]))
	assert.JSONEq(t, `{"id":"second"}`, string(docs[1]))
	var v any
	require.NoError(t, json.Unmarshal(docs[0], &v))

	_, err = cli.YAMLToJSON([]byte("a: [unclosed\n"))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	docs, err = cli.YAMLToJSON([]byte("   \n"))
	require.NoError(t, err)
	assert.Empty(t, docs)
}
