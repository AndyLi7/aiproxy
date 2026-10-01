package fal

import (
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
	_, ok := parameterIssue("unknown", []any{"body", "prompt"})
	require.False(t, ok)
	i, ok := parameterIssue("missing", []any{"body", "prompt"})
	require.True(t, ok)
	require.Equal(t, "required", i.Rule)
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
