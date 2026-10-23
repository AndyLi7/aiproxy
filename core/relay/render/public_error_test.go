//nolint:testpackage // Tests publicStreamData private serialization and exported renderers together.
package render

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
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
	flat := publicStreamData(
		[]byte(`{"type":"error","code":"private","message":"private","param":"secret"}`),
		"",
		false,
	)
	require.Contains(t, string(flat), `"code":"upstream_unavailable"`)
	require.NotContains(t, string(flat), `"error":`)
	require.NotContains(t, string(flat), "secret")

	native := publicStreamData(
		[]byte(`{"error":{"code":402,"message":"private","details":["secret"]}}`),
		"",
		true,
	)
	require.Contains(t, string(native), `"code":503`)
	require.Contains(t, string(native), `"status":"UNAVAILABLE"`)
	require.NotContains(t, string(native), "secret")
}

func TestPublicResponseDataPreservesSuccessfulMediaAndInput(t *testing.T) {
	for _, raw := range []string{
		`{"id":"resp_ok","status":"completed","error":null,"output":[{"type":"image_generation_call","result":"aGVsbG8="}],"metadata":{"note":"error: example"}}`,
		`{"id":"resp_pending","status":"in_progress","error":null,"output":[]}`,
		`{"id":"resp_incomplete","status":"incomplete","error":null,"incomplete_details":{"reason":"max_output_tokens"},"output":[{"text":"partial"}]}`,
	} {
		data := []byte(raw)
		require.Equal(t, raw, string(PublicResponseData(data)))
		require.Equal(t, raw, string(data))
	}
}

func TestPublicResponseDataFailedWithoutError(t *testing.T) {
	raw := []byte(`{"id":"resp_failed","status":"failed","error":null,"debug":"private trace"}`)
	before := string(raw)
	safe := PublicResponseData(raw)
	require.Contains(t, string(safe), `"status":"failed"`)
	require.Contains(t, string(safe), `"id":"resp_failed"`)
	require.Contains(t, string(safe), "upstream_unavailable")
	require.NotContains(t, string(safe), "private trace")
	require.Equal(t, before, string(raw))
}

func TestStreamErrorDiagnosticMatrix(t *testing.T) {
	const diagnostic = `FakeVendor http://private.invalid:8080 https://signed.invalid/file?signature=FAKE sk-test-only-NOT-A-KEY panic: fake stack.go:42`
	for _, raw := range []string{
		`{"error":{"message":"` + diagnostic + `","type":"` + diagnostic + `","code":"` + diagnostic + `"},"debug":"` + diagnostic + `"}`,
		`{"type":"error","message":"` + diagnostic + `","param":"` + diagnostic + `","code":"` + diagnostic + `"}`,
		`{"type":"response.failed","response":{"id":"resp_safe","error":{"message":"` + diagnostic + `"},"metadata":{"debug":"` + diagnostic + `"}}}`,
	} {
		for _, protocol := range []string{"openai", "anthropic", "gemini"} {
			t.Run(protocol+"/"+raw[:10], func(t *testing.T) {
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

				for _, marker := range []string{"FakeVendor", "private.invalid", "signed.invalid", "signature", "sk-test", "stack.go", "panic"} {
					require.NotContains(t, w.Body.String(), marker)
				}

				require.Contains(t, w.Body.String(), "temporarily unavailable")
				require.Equal(t, raw, string(data))
			})
		}
	}

	w := httptest.NewRecorder()
	require.NoError(t, (&Anthropic{Event: "error", Data: []byte(diagnostic)}).Render(w))
	require.NotContains(t, w.Body.String(), "FakeVendor")
	require.Contains(t, w.Body.String(), "upstream_unavailable")
}
