package registryvalidation

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func metadataFixture(t *testing.T, projection any, properties map[string]any) []byte {
	var contract map[string]any
	require.NoError(t, json.Unmarshal(providerFixture(), &contract))
	upstream := contract["providers"].(map[string]any)["small"].(map[string]any)["upstream"].(map[string]any)
	if projection != nil {
		upstream["outputMetadata"] = projection
	}
	upstream["outputJsonSchema"] = map[string]any{"type": "object", "properties": properties, "required": []string{"images"}}
	frozen, err := FreezeProviderBinding(mustJSON(t, contract), fixtureBinding())
	require.NoError(t, err)
	return frozen
}

func TestFrozenProviderMetadataOnlyDeliversDeclaredFields(t *testing.T) {
	properties := map[string]any{"images": map[string]any{"type": "array"}, "caption": map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "null"}}}, "count": map[string]any{"type": "integer"}}
	raw := metadataFixture(t, map[string]any{"version": 1, "fields": []string{"caption", "count"}}, properties)
	got, err := ExtractFrozenProviderMetadata(raw, []byte(`{"images":[],"caption":null,"count":18446744073709551615,"secret":"never exposed","unknown":"not declared"}`))
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, json.RawMessage(`null`), got["caption"])
	require.Equal(t, json.RawMessage(`18446744073709551615`), got["count"])
	got, err = ExtractFrozenProviderMetadata(raw, []byte(`{"images":[]}`))
	require.NoError(t, err)
	require.Empty(t, got)
	_, err = ExtractFrozenProviderMetadata(raw, []byte(`{"images":[],"caption":12}`))
	require.Error(t, err)
	legacy := metadataFixture(t, nil, properties)
	got, err = ExtractFrozenProviderMetadata(legacy, []byte(`{"images":[],"caption":"legacy"}`))
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestOutputMetadataProjectionRejectsUndeclaredAndSensitiveFields(t *testing.T) {
	for _, name := range []string{"unknown", "image", "seed", "request_id", "api_key", "billing_cost", "nsfw_content_detected", "image_url"} {
		t.Run(name, func(t *testing.T) {
			props := map[string]any{name: map[string]any{"type": "string"}}
			if name == "unknown" {
				delete(props, name)
			}
			spec := &ProviderSpec{Output: map[string]any{"properties": props}, OutputMetadata: &OutputMetadataProjection{Version: 1, Fields: []string{name}}}
			require.Error(t, validateOutputMetadataProjection(spec))
		})
	}
	for _, schema := range []map[string]any{{}, {"type": "object"}, {"type": "string", "format": "uri"}, {"type": "string", "contentEncoding": "base64"}, {"type": "array", "items": map[string]any{"type": "object"}}} {
		spec := &ProviderSpec{Output: map[string]any{"properties": map[string]any{"extra": schema}}, OutputMetadata: &OutputMetadataProjection{Version: 1, Fields: []string{"extra"}}}
		require.Error(t, validateOutputMetadataProjection(spec))
	}
	spec := &ProviderSpec{Output: map[string]any{"properties": map[string]any{"palette": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}}, OutputMetadata: &OutputMetadataProjection{Version: 1, Fields: []string{"palette"}}}
	require.NoError(t, validateOutputMetadataProjection(spec))
	spec.OutputMetadata.Version = 4
	require.Error(t, validateOutputMetadataProjection(spec))
	spec.OutputMetadata.Version = 1
	spec.OutputMetadata.Fields = []string{"palette", "palette"}
	require.Error(t, validateOutputMetadataProjection(spec))
}

func TestStructuredDocumentsPreserveValuesWithoutTruncation(t *testing.T) {
	properties := map[string]any{"images": map[string]any{"type": "array"}, "vgl": map[string]any{"type": "object", "additionalProperties": true}}
	raw := metadataFixture(t, map[string]any{"version": 2, "fields": []string{"vgl"}}, properties)
	document := `{"layers":[{"id":18446744073709551615,"opacity":0.1234567890123456789,"text":"hello","extra":null}],"nested":{"enabled":true}}`
	result, err := ExtractFrozenProviderMetadata(raw, []byte(`{"images":[],"vgl":`+document+`,"debug":"hidden"}`))
	require.NoError(t, err)
	require.Equal(t, json.RawMessage(document), result["vgl"])
	require.Len(t, result, 1)
	_, err = ExtractFrozenProviderMetadata(raw, []byte(`{"images":[],"vgl":null}`))
	require.Error(t, err)
	require.True(t, boundedMetadataJSON([]byte(strings.Repeat("[", 32)+"0"+strings.Repeat("]", 32))))
	require.False(t, boundedMetadataJSON([]byte(strings.Repeat("[", 33)+"0"+strings.Repeat("]", 33))))
	require.False(t, boundedMetadataJSON([]byte(`"`+strings.Repeat("a", 1048576)+`"`)))
	require.False(t, boundedMetadataJSON([]byte(`[`+strings.Repeat("0,", 100000)+`0]`)))
	_, err = ExtractFrozenProviderMetadata(raw, []byte(`{"images":[],"vgl":{"large":"`+strings.Repeat("a", 1048576)+`"}}`))
	require.Error(t, err)
}

func TestReflectionDocumentsRequireV3AndPreserveJSON(t *testing.T) {
	field := map[string]any{"anyOf": []any{map[string]any{"type": "array", "items": map[string]any{"type": "object"}}, map[string]any{"type": "null"}}}
	properties := map[string]any{"images": map[string]any{"type": "array"}, "best_info": field}
	for _, version := range []int{1, 2} {
		require.Error(t, validateOutputMetadataProjection(&ProviderSpec{Output: map[string]any{"properties": properties}, OutputMetadata: &OutputMetadataProjection{Version: version, Fields: []string{"best_info"}}}))
	}
	raw := metadataFixture(t, map[string]any{"version": 3, "fields": []string{"best_info"}}, properties)
	for _, payload := range []string{`{"images":[]}`, `{"images":[],"best_info":null}`, `{"images":[],"best_info":[]}`, `{"images":[],"best_info":[{"score":18446744073709551615,"reason":"detail","nested":{"keep":true}}]}`} {
		got, err := ExtractFrozenProviderMetadata(raw, []byte(payload))
		require.NoError(t, err)
		var input map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(payload), &input))
		require.Equal(t, input["best_info"], got["best_info"])
	}
	for _, payload := range []string{`{"images":[],"best_info":{}}`, `{"images":[],"best_info":[1]}`} {
		_, err := ExtractFrozenProviderMetadata(raw, []byte(payload))
		require.Error(t, err)
	}
	huge, _ := json.Marshal(map[string]any{"images": []any{}, "best_info": []any{map[string]any{"text": strings.Repeat("x", 1048577)}}})
	_, err := ExtractFrozenProviderMetadata(raw, huge)
	require.Error(t, err)
}
