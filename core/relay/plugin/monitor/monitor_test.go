//nolint:testpackage
package monitor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	relaymeta "github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
	relaymodel "github.com/labring/aiproxy/core/relay/model"
	"github.com/stretchr/testify/require"
)

type monitorDoResponseFunc func(
	*relaymeta.Meta,
	adaptor.Store,
	*gin.Context,
	*http.Response,
) (adaptor.DoResponseResult, adaptor.Error)

func (fn monitorDoResponseFunc) DoResponse(
	meta *relaymeta.Meta,
	store adaptor.Store,
	c *gin.Context,
	resp *http.Response,
) (adaptor.DoResponseResult, adaptor.Error) {
	return fn(meta, store, c, resp)
}

func TestGetChannelWarnErrorRateUsesChannelValueEvenWhenAutoBalanceDisabled(t *testing.T) {
	meta := &relaymeta.Meta{}
	meta.Channel.WarnErrorRate = 0.42
	meta.Channel.EnabledAutoBalanceCheck = false
	meta.Channel.MaxErrorRate = 0.95

	require.InDelta(t, 0.42, getChannelWarnErrorRate(meta), 0.0001)
	require.InDelta(t, 0.95, getChannelMaxErrorRate(meta), 0.0001)
}

func TestGetChannelWarnErrorRateFallsBackToDefault(t *testing.T) {
	previous := config.GetDefaultWarnNotifyErrorRate()
	config.SetDefaultWarnNotifyErrorRate(previous)
	t.Cleanup(func() {
		config.SetDefaultWarnNotifyErrorRate(previous)
	})

	config.SetDefaultWarnNotifyErrorRate(0.37)

	require.InDelta(t, 0.37, getChannelWarnErrorRate(&relaymeta.Meta{}), 0.0001)
}

func TestGetChannelMaxErrorRateDoesNotDependOnBalanceCheckSwitch(t *testing.T) {
	meta := &relaymeta.Meta{}
	meta.Channel.WarnErrorRate = 0.25
	meta.Channel.MaxErrorRate = 0.88

	require.InDelta(t, 0.88, getChannelMaxErrorRate(meta), 0.0001)

	meta.Channel.EnabledAutoBalanceCheck = true

	require.InDelta(t, 0.88, getChannelMaxErrorRate(meta), 0.0001)
}

func TestShouldTryBanNoPermissionRequiresChannelSwitch(t *testing.T) {
	meta := &relaymeta.Meta{}

	require.False(t, shouldTryBanNoPermission(meta, false))

	meta.Channel.EnabledNoPermissionBan = true

	require.True(t, shouldTryBanNoPermission(meta, false))
	require.False(t, shouldTryBanNoPermission(meta, true))
}

func TestChannelStatusHasPermission(t *testing.T) {
	t.Parallel()

	for _, statusCode := range []int{
		http.StatusUnauthorized,
		http.StatusPaymentRequired,
		http.StatusForbidden,
		http.StatusNotFound,
	} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			t.Parallel()

			require.False(t, ChannelStatusHasPermission(statusCode))
		})
	}

	require.True(t, ChannelStatusHasPermission(http.StatusBadRequest))
}

func TestChannelHasPermissionForForbiddenErrorCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		code string
		want bool
	}{
		{
			name: "request blocked by cyber policy",
			code: "session_blocked_by_cyber_policy",
			want: true,
		},
		{
			name: "channel permission failure",
			code: "insufficient_user_quota",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			relayErr := relaymodel.NewOpenAIError(http.StatusForbidden, relaymodel.OpenAIError{
				Code:    tt.code,
				Message: "forbidden",
				Type:    relaymodel.ErrorTypeUpstream,
			})

			require.Equal(t, tt.want, ChannelHasPermission(relayErr))
		})
	}
}

func TestChannelMonitorDoResponseRecordsResponseCost(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", nil)
	entry := common.NewLogger()
	common.SetLogger(c.Request, entry)

	requestMeta := relaymeta.NewMeta(
		&model.Channel{ID: 901, Type: model.ChannelTypeOpenAI},
		mode.ChatCompletions,
		"resp-cost-test",
		model.ModelConfig{},
	)
	requestMeta.Channel.MaxErrorRate = 0

	result, relayErr := (&ChannelMonitor{}).DoResponse(
		requestMeta,
		nil,
		c,
		&http.Response{StatusCode: http.StatusOK},
		monitorDoResponseFunc(func(
			*relaymeta.Meta,
			adaptor.Store,
			*gin.Context,
			*http.Response,
		) (adaptor.DoResponseResult, adaptor.Error) {
			time.Sleep(2 * time.Millisecond)
			return adaptor.DoResponseResult{}, nil
		}),
	)

	require.NoError(t, relayErr)
	require.Empty(t, result.UpstreamID)
	require.Contains(t, entry.Data, "resp_cost")
	cost, ok := entry.Data["resp_cost"].(string)
	require.True(t, ok)
	require.NotEmpty(t, cost)
	parsedCost, err := time.ParseDuration(cost)
	require.NoError(t, err)
	require.Greater(t, parsedCost, time.Duration(0))
}

func TestChannelMonitorDoResponseRecordsResponseCostOnError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", nil)
	entry := common.NewLogger()
	common.SetLogger(c.Request, entry)

	requestMeta := relaymeta.NewMeta(
		&model.Channel{ID: 902, Type: model.ChannelTypeOpenAI},
		mode.ChatCompletions,
		"resp-cost-error-test",
		model.ModelConfig{},
	)
	relayErrExpected := relaymodel.NewOpenAIError(http.StatusBadGateway, relaymodel.OpenAIError{
		Message: "upstream error",
	})

	_, relayErr := (&ChannelMonitor{}).DoResponse(
		requestMeta,
		nil,
		c,
		&http.Response{StatusCode: http.StatusBadGateway},
		monitorDoResponseFunc(func(
			*relaymeta.Meta,
			adaptor.Store,
			*gin.Context,
			*http.Response,
		) (adaptor.DoResponseResult, adaptor.Error) {
			return adaptor.DoResponseResult{}, relayErrExpected
		}),
	)

	require.ErrorIs(t, relayErr, relayErrExpected)
	require.Contains(t, entry.Data, "resp_cost")
	cost, ok := entry.Data["resp_cost"].(string)
	require.True(t, ok)
	require.NotEmpty(t, cost)
	parsedCost, err := time.ParseDuration(cost)
	require.NoError(t, err)
	require.Greater(t, parsedCost, time.Duration(0))
}
