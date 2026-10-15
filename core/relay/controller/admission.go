package controller

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
	relaymodel "github.com/labring/aiproxy/core/relay/model"
)

// Logging's IsReadMode is intentionally broader: AlphaSearch is excluded from
// call metrics, but starts new work. File uploads likewise are not frozen reads.
// Unknown/new modes default to admission, never to a new unguarded lane.
func relayNeedsAdmission(m mode.Mode, method string) bool {
	switch m {
	case mode.VideoGenerationsGetJobs, mode.VideoGenerationsContent,
		mode.ResponsesGet, mode.ResponsesDelete, mode.ResponsesCancel, mode.ResponsesInputItems,
		mode.VideosGet, mode.VideosContent, mode.VideosDelete,
		mode.GeminiVideoOperations, mode.AliVideoTasks, mode.DoubaoVideoTasks, mode.DoubaoVideoTasksDelete:
		return false
	case mode.GeminiFiles:
		return method != http.MethodGet && method != http.MethodHead && method != http.MethodDelete
	default:
		return true
	}
}

// Shared DoHelper entry covers public relay, retries, admin channel tests and
// nested plugin model calls before ConvertRequest can send anything upstream.
// A use is evidence of a possible call, not permission to replay another one.
func admitRelayAttempt(c *gin.Context, m *meta.Meta) adaptor.Error {
	m.ChannelAdmissionUseID = ""
	if !relayNeedsAdmission(m.Mode, c.Request.Method) {
		return nil
	}
	use, err := model.AdmitRelayAttempt(c.Request.Context(), &model.Channel{
		ID: m.Channel.ID, Type: m.Channel.Type, Key: m.Channel.Key,
		BaseURL: m.Channel.BaseURL, ProxyURL: m.Channel.ProxyURL,
		SkipTLSVerify: m.Channel.SkipTLSVerify, Configs: m.ChannelConfigs,
	}, model.RelayAdmissionScope{GroupID: m.Group.ID, TokenID: m.Token.ID,
		BillingOperationID: m.BillingOperationID, RequestID: m.RequestID,
		Mode: int(m.Mode), Model: m.OriginModel})
	if err != nil {
		return relaymodel.WrapperOpenAIErrorWithMessage(
			"channel admission unavailable", "channel_admission_unavailable", http.StatusServiceUnavailable)
	}
	if use != nil {
		m.ChannelAdmissionUseID = use.ID
	}
	return nil
}
