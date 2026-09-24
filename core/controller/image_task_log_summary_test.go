package controller

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImageTaskLogSummaryKeepsPromptWithoutUploadedMedia(t *testing.T) {
	summary := imageTaskLogRequestSummary([]byte(`{"prompt":"A quiet harbor","negative_prompt":"fog","image":"https://private.example/secret","api_key":"secret"}`))
	require.JSONEq(t, `{"prompt":"A quiet harbor","negative_prompt":"fog"}`, summary)
	require.NotContains(t, summary, "private.example")
	require.Empty(t, imageTaskLogRequestSummary([]byte(`{"image":"https://private.example/secret"}`)))
	require.Empty(t, imageTaskLogRequestSummary([]byte(`{"prompt":"`+strings.Repeat("x", 8193)+`"}`)))
	require.JSONEq(t, `{"prompt":"A quiet harbor"}`, imageTaskLogRequestSummary([]byte(`{"prompt":"A quiet harbor","image":"`+strings.Repeat("x", 70000)+`"}`)))
}
