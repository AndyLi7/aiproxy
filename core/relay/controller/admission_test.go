package controller

import (
	"net/http"
	"testing"

	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

func TestRelayAdmissionClassifiesWorkInsteadOfLogMetrics(t *testing.T) {
	for _, m := range []mode.Mode{mode.Unknown, mode.ChatCompletions, mode.Responses, mode.ImagesGenerations, mode.Videos, mode.VideosRemix, mode.AlphaSearch, mode.NativeTasks, mode.Mode(99999)} {
		require.True(t, relayNeedsAdmission(m, http.MethodGet), "new work is guarded even with a forged method: %v", m)
	}
	for _, m := range []mode.Mode{mode.VideoGenerationsGetJobs, mode.VideoGenerationsContent, mode.ResponsesGet, mode.ResponsesDelete, mode.ResponsesCancel, mode.ResponsesInputItems, mode.VideosGet, mode.VideosContent, mode.VideosDelete, mode.GeminiVideoOperations, mode.AliVideoTasks, mode.DoubaoVideoTasks, mode.DoubaoVideoTasksDelete} {
		require.False(t, relayNeedsAdmission(m, http.MethodPost), "frozen operations: %v", m)
	}
	require.True(t, relayNeedsAdmission(mode.GeminiFiles, http.MethodPost))
	require.True(t, relayNeedsAdmission(mode.GeminiFiles, http.MethodPut))
	require.False(t, relayNeedsAdmission(mode.GeminiFiles, http.MethodGet))
	require.False(t, relayNeedsAdmission(mode.GeminiFiles, http.MethodDelete))
}
