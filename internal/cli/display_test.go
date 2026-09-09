package cli

import (
	"encoding/json"
	"testing"

	"github.com/action-state-group/capsule-emit-go/artifact"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDisplayBytes(t *testing.T) {
	for _, tt := range []struct {
		name  string
		input []byte
		want  string
	}{
		{"nil", nil, `null`},
		{"empty", []byte{}, `""`},
		{"object", []byte(`{"a":1}`), `{"a":1}`},
		{"array", []byte(`[true,2]`), `[true,2]`},
		{"large_integer", []byte(`9007199254740993`), `9007199254740993`},
		{"boolean", []byte(`true`), `true`},
		{"null", []byte(`null`), `null`},
		{"json_string", []byte(`"value"`), `"value"`},
		{"text", []byte("hello\nworld"), `"hello\nworld"`},
		{"unicode", []byte{0xc3, 0xa9}, `"\u00e9"`},
		{"binary", []byte{0xff, 0xfe}, `{"encoding":"base64","data":"//4="}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(displayBytes(tt.input))
			require.NoError(t, err)
			assert.JSONEq(t, tt.want, string(b))
			if tt.name == "large_integer" {
				assert.Equal(t, tt.want, string(b))
			}
		})
	}
}

func TestGetRecordOutput(t *testing.T) {
	r := artifact.Record{
		CapsuleID:        "example",
		Capsule:          []byte("{\n  \"n\": 9007199254740993\n}"),
		ProducerEnvelope: []byte{0xff, 0xfe},
		Artifacts: []artifact.Artifact{
			{Name: "payload", Binding: "effect.request_digest", Content: []byte(`{"ok":true}`), State: "present", ContentSHA256: "digest"},
			{Name: "purged", State: "purged", ContentSHA256: "retained-digest"},
			{Name: "empty", Content: []byte{}, State: "present"},
		},
	}
	original, err := json.Marshal(r)
	require.NoError(t, err)

	readable, err := json.Marshal(getRecordOutput(r, false))
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"capsule_id":"example",
		"capsule":{"n":9007199254740993},
		"producer_envelope":{"encoding":"base64","data":"//4="},
		"artifacts":[
			{"name":"payload","binding":"effect.request_digest","content":{"ok":true},"state":"present","content_sha256":"digest"},
			{"name":"purged","state":"purged","content_sha256":"retained-digest"},
			{"name":"empty","state":"present","content":""}
		]
	}`, string(readable))
	assert.Contains(t, string(readable), "9007199254740993")

	raw, err := json.Marshal(getRecordOutput(r, true))
	require.NoError(t, err)
	assert.Equal(t, original, raw)
	var restored artifact.Record
	require.NoError(t, json.Unmarshal(raw, &restored))
	assert.Equal(t, r.Capsule, restored.Capsule)
	assert.Equal(t, r.ProducerEnvelope, restored.ProducerEnvelope)
	assert.Equal(t, r.Artifacts[0], restored.Artifacts[0])
	after, err := json.Marshal(r)
	require.NoError(t, err)
	assert.Equal(t, original, after, "display must not mutate stored bytes")
}

func TestGetRecordOutputEmptyInventory(t *testing.T) {
	for _, artifacts := range [][]artifact.Artifact{nil, {}} {
		r := artifact.Record{Artifacts: artifacts}
		b, err := json.Marshal(getRecordOutput(r, false))
		require.NoError(t, err)
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(b, &fields))
		if artifacts == nil {
			assert.Equal(t, "null", string(fields["artifacts"]))
		} else {
			assert.Equal(t, "[]", string(fields["artifacts"]))
		}
	}
}
