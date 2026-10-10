package render

import (
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"testing"
)

func TestStreamErrorsDoNotExposeUpstreamDetails(t *testing.T) {
	for _, raw := range []string{
		`{"error":{"message":"private-key fal balance","type":"private-provider","code":"private-code"},"debug":"private-key"}`,
		`{"type":"error","message":"private-key","code":"private-code"}`,
		`{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"message":"private-key"},"metadata":{"key":"private-key"}}}`,
	} {
		for _, protocol := range []string{"openai", "anthropic", "gemini"} {
			w := httptest.NewRecorder()
			data := []byte(raw)
			switch protocol {
			case "openai":
				require.NoError(t, (&OpenaiSSE{Data: data}).Render(w))
			case "anthropic":
				require.NoError(t, (&Anthropic{Event: "error", Data: data}).Render(w))
			case "gemini":
				require.NoError(t, (&GeminiSSE{Data: data}).Render(w))
			}
			require.NotContains(t, w.Body.String(), "private-")
			require.NotContains(t, w.Body.String(), "balance")
			require.Contains(t, w.Body.String(), "temporarily unavailable")
			require.Equal(t, raw, string(data))
		}
	}
}
func TestStreamContentIsNotRedactedAsAnError(t *testing.T) {
	for _, raw := range []string{`{"choices":[{"delta":{"content":"error: balance example"}}]}`, `{"type":"response.output_text.delta","delta":"private-key example"}`, `[DONE]`} {
		w := httptest.NewRecorder()
		require.NoError(t, (&OpenaiSSE{Data: []byte(raw)}).Render(w))
		require.Contains(t, w.Body.String(), raw)
	}
}

func TestFlatAndNativeStreamErrorEnvelopes(t *testing.T) {
	flat := publicStreamData([]byte(`{"type":"error","code":"private","message":"private","param":"secret"}`), "", false)
	require.Contains(t, string(flat), `"code":"upstream_unavailable"`)
	require.NotContains(t, string(flat), `"error":`)
	require.NotContains(t, string(flat), "secret")
	native := publicStreamData([]byte(`{"error":{"code":402,"message":"private","details":["secret"]}}`), "", true)
	require.Contains(t, string(native), `"code":503`)
	require.Contains(t, string(native), `"status":"UNAVAILABLE"`)
	require.NotContains(t, string(native), "secret")
}
