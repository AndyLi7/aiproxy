package fal

import (
	"encoding/json"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestActualParameterError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			w.Write([]byte(`{"status":"COMPLETED"}`))
			return
		}
		w.WriteHeader(422)
		w.Write([]byte(`{"detail":[{"type":"string_type","loc":["body","loras",0,"path"],"msg":"SECRET https://fal.example","input":"SECRET"},{"type":"float_parsing","loc":["body","loras",0,"scale"]}]}`))
	}))
	defer s.Close()
	c := Client{HTTP: s.Client(), BaseURL: s.URL}
	r, e := c.Poll(t.Context(), "fal-ai/test", "id")
	require.NoError(t, e)
	require.Equal(t, "invalid_parameters", r.Error.Code)
	require.Len(t, r.Error.Issues, 2)
	require.Equal(t, "loras[0].path", r.Error.Issues[0].Field)
	require.Equal(t, "number", r.Error.Issues[1].Rule)
	require.NotContains(t, r.Error.Message, "SECRET")
}
func TestUnsafeParameterErrors(t *testing.T) {
	for _, loc := range [][]any{{"body", "api_key"}, {"body", "https://example.com"}, {"body", "prompt", -1.0}, {"body", "prompt", 1.5}, {"header", "authorization"}, {"body", "__proto__"}} {
		_, ok := parameterIssue("missing", loc)
		require.False(t, ok)
	}
	// Only error types shaped like fal/pydantic codes fall back to rule invalid.
	for _, kind := range []string{"", "Unknown", "has-dash", "has space", "1st", strings.Repeat("a", 65)} {
		_, ok := parameterIssue(kind, []any{"body", "prompt"})
		require.False(t, ok, kind)
	}
	i, ok := parameterIssue("missing", []any{"body", "prompt"})
	require.True(t, ok)
	require.Equal(t, "required", i.Rule)
}

// Owner decision 2026-10-09: fal model error types map to categorized rules,
// and any other well-formed type still names its field with rule invalid.
func TestFalErrorTypesMapToPublicRules(t *testing.T) {
	for kind, rule := range map[string]string{
		"feature_not_supported": "unsupported_value", "one_of": "allowed_value",
		"sequence_too_short": "length", "sequence_too_long": "length",
		"image_too_small": "file_size", "image_too_large": "file_size", "file_too_large": "file_size",
		"unsupported_image_format": "file_format", "unsupported_audio_format": "file_format",
		"unsupported_video_format": "file_format", "unsupported_format": "file_format",
		"image_load_error": "file_unreadable", "file_download_error": "file_unreadable",
		"audio_duration_too_long": "duration", "audio_duration_too_short": "duration",
		"video_duration_too_long": "duration", "video_duration_too_short": "duration",
		"content_policy_violation": "content_policy", "input_value_error": "invalid",
		"string_pattern_mismatch": "invalid", "brand_new_check_2": "invalid",
	} {
		issue, ok := parameterIssue(kind, []any{"body", "voice_setting", "voice_id"})
		require.True(t, ok, kind)
		require.Equal(t, model.ImageParameterIssue{Field: "voice_setting.voice_id", Rule: rule}, issue, kind)
	}
	// The rule enum is shared with the application, which has a message per rule.
	enum := map[string]bool{}
	for _, rule := range []string{"required", "string", "integer", "number", "boolean", "array", "object", "allowed_value", "range", "length", "multiple", "invalid", "unsupported_value", "file_size", "file_format", "file_unreadable", "duration", "content_policy"} {
		enum[rule] = true
	}
	for kind, rule := range parameterRules {
		require.True(t, enum[rule], kind)
	}
}

// The image lane shares the mapping: a fal model error on the result names
// its field too.
func TestImageLaneNamesFalModelErrorFields(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			w.Write([]byte(`{"status":"COMPLETED"}`))
			return
		}
		w.WriteHeader(422)
		w.Write([]byte(`{"detail":[{"type":"image_too_small","loc":["body","image_url"],"msg":"Image too small: 12x12","url":"https://docs.fal.ai/errors","input":"https://customer.example/x.png"}]}`))
	}))
	defer s.Close()
	c := Client{HTTP: s.Client(), BaseURL: s.URL}
	r, e := c.Poll(t.Context(), "fal-ai/test", "id")
	require.NoError(t, e)
	require.Equal(t, "invalid_parameters", r.Error.Code)
	require.Equal(t, []model.ImageParameterIssue{{Field: "image_url", Rule: "file_size"}}, r.Error.Issues)
	encoded, err := json.Marshal(r)
	require.NoError(t, err)
	for _, leak := range []string{"12x12", "docs.fal.ai", "customer.example"} {
		require.NotContains(t, string(encoded), leak)
	}
}

func TestImmediateParameterRejectionRemainsNotAccepted(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		w.Write([]byte(`{"detail":[{"type":"missing","loc":["body","prompt"]}]}`))
	}))
	defer s.Close()
	c := Client{HTTP: s.Client(), BaseURL: s.URL}
	_, err := c.Submit(t.Context(), "fal-ai/test", []byte(`{}`))
	require.ErrorIs(t, err, adaptor.ErrImageSubmissionRejected)
	var failure *adaptor.ImageSubmissionFailure
	require.ErrorAs(t, err, &failure)
	require.Equal(t, "required", failure.PublicError.Issues[0].Rule)
}

func TestUnsignedSeedDoesNotDiscardSuccessfulImage(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			w.Write([]byte(`{"status":"COMPLETED"}`))
			return
		}
		w.Write([]byte(`{"images":[{"url":"https://example.com/a.png"}],"seed":18446744073709551615}`))
	}))
	defer s.Close()
	c := Client{HTTP: s.Client(), BaseURL: s.URL}
	r, e := c.Poll(t.Context(), "fal-ai/test", "id")
	require.NoError(t, e)
	require.Equal(t, "completed", r.Status)
	require.Equal(t, "18446744073709551615", r.Metadata.SeedExact)
	require.Nil(t, r.Metadata.Seed)
}
