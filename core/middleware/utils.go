package middleware

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	relaymodel "github.com/labring/aiproxy/core/relay/model"
)

func AbortOperationallyWithMode(
	m mode.Mode,
	c *gin.Context,
	stage model.FailureStage,
	statusCode int,
	message string,
	opts ...relaymodel.WrapperErrorOptionFunc,
) {
	SetFailureStage(c, stage, message)
	AbortLogWithMessageWithMode(m, c, statusCode, message, opts...)
}

func AbortOperationallyWithCodeWithMode(
	m mode.Mode,
	c *gin.Context,
	stage model.FailureStage,
	errorCode string,
	statusCode int,
	message string,
	opts ...relaymodel.WrapperErrorOptionFunc,
) {
	SetOperationalFailure(c, stage, errorCode, message)
	AbortLogWithMessageWithMode(m, c, statusCode, message, opts...)
}

func AbortPublicVideoRequestError(
	c *gin.Context,
	stage model.FailureStage,
	statusCode int,
	code string,
	message string,
	param string,
	value any,
	allowedValues []string,
	expected string,
) {
	SetOperationalFailure(c, stage, code, message)
	common.GetLogger(c).Warn(message)
	c.JSON(statusCode, relaymodel.PublicVideoRequestError(
		statusCode,
		code,
		message,
		param,
		value,
		allowedValues,
		expected,
	))
	c.Abort()
}

func AbortOperationally(
	c *gin.Context,
	stage model.FailureStage,
	statusCode int,
	message string,
	opts ...relaymodel.WrapperErrorOptionFunc,
) {
	SetFailureStage(c, stage, message)
	AbortLogWithMessage(c, statusCode, message, opts...)
}

func IsPublicVideoRequest(path string, m mode.Mode) bool {
	if path == "/v1/videos" || strings.HasPrefix(path, "/v1/videos/") {
		return true
	}

	switch m {
	case mode.VideoGenerationsJobs,
		mode.VideoGenerationsGetJobs,
		mode.VideoGenerationsContent,
		mode.Videos,
		mode.VideosGet,
		mode.VideosContent,
		mode.VideosDelete,
		mode.VideosRemix,
		mode.VideosEdits,
		mode.VideosExtensions,
		mode.GeminiVideo,
		mode.GeminiVideoOperations,
		mode.AliVideo,
		mode.AliVideoTasks,
		mode.DoubaoVideo,
		mode.DoubaoVideoTasks,
		mode.DoubaoVideoTasksDelete:
		return true
	default:
		return false
	}
}

func AbortLogWithMessageWithMode(
	m mode.Mode,
	c *gin.Context,
	statusCode int,
	message string,
	opts ...relaymodel.WrapperErrorOptionFunc,
) {
	setDefaultFailureStage(c, message)
	common.GetLogger(c).Error(message)
	AbortWithMessageWithMode(m, c, statusCode, message, opts...)
}

func AbortWithMessageWithMode(
	m mode.Mode,
	c *gin.Context,
	statusCode int,
	message string,
	opts ...relaymodel.WrapperErrorOptionFunc,
) {
	if stage, _ := c.Get(operationalFailureStageKey); stage == model.FailureStageBalance {
		abortBalanceFailure(m, c, statusCode, opts)
		return
	}

	if IsPublicVideoRequest(c.Request.URL.Path, m) ||
		(strings.HasPrefix(c.Request.URL.Path, "/v1/") && (statusCode == 401 || statusCode >= 500)) {
		c.JSON(statusCode, relaymodel.PublicVideoError(statusCode))
		c.Abort()
		return
	}

	c.JSON(statusCode,
		relaymodel.WrapperErrorWithMessage(m, statusCode, message, opts...),
	)
	c.Abort()
}

// abortBalanceFailure writes a balance failure without the internal message:
// a group refusal becomes 402 insufficient_quota / insufficient_balance. On
// /v1/ it is the public error body. A balance below zero (already loaded by
// the distributor) names the amount owed; any other balance failure keeps
// the generic sentence. Status, type and code never depend on the balance.
func abortBalanceFailure(
	m mode.Mode,
	c *gin.Context,
	statusCode int,
	opts []relaymodel.WrapperErrorOptionFunc,
) {
	options := &relaymodel.WrapperErrorOption{}
	for _, apply := range opts {
		apply(options)
	}

	if statusCode == http.StatusForbidden && options.Type == GroupBalanceNotEnough {
		statusCode = http.StatusPaymentRequired
	}

	negative, belowZero := negativeBalanceMessage(c, statusCode)
	if strings.HasPrefix(c.Request.URL.Path, "/v1/") {
		if belowZero {
			c.JSON(statusCode, relaymodel.NewOpenAIError(statusCode, relaymodel.OpenAIError{
				Message: negative,
				Type:    "insufficient_quota",
				Code:    "insufficient_balance",
			}))
		} else {
			c.JSON(statusCode, relaymodel.PublicVideoError(statusCode))
		}

		c.Abort()

		return
	}

	message := "Your account balance is insufficient."
	if belowZero {
		message = negative
	}

	opts = append(
		opts,
		relaymodel.WithType("insufficient_quota"),
		relaymodel.WithCode("insufficient_balance"),
	)
	c.JSON(statusCode, relaymodel.WrapperErrorWithMessage(m, statusCode, message, opts...))
	c.Abort()
}

// negativeBalanceMessage is the 402 text for a group whose balance is below
// zero, or false when the refusal is not a 402 or the balance is not negative.
func negativeBalanceMessage(c *gin.Context, statusCode int) (string, bool) {
	if statusCode != http.StatusPaymentRequired {
		return "", false
	}

	value, ok := c.Get(GroupBalance)
	if !ok {
		return "", false
	}

	gbc, ok := value.(*GroupBalanceConsumer)
	if !ok || gbc == nil || !(gbc.balance < 0) {
		return "", false
	}

	return balance.NegativeBalanceMessage(gbc.balance), true
}

func AbortLogWithMessage(
	c *gin.Context,
	statusCode int,
	message string,
	opts ...relaymodel.WrapperErrorOptionFunc,
) {
	setDefaultFailureStage(c, message)
	common.GetLogger(c).Error(message)
	AbortWithMessage(c, statusCode, message, opts...)
}

func setDefaultFailureStage(c *gin.Context, safeError string) {
	if _, exists := c.Get(operationalFailureStageKey); exists {
		return
	}

	stage := model.FailureStageValidation
	if _, authenticated := c.Get(Token); !authenticated {
		stage = model.FailureStageAuth
	}

	SetFailureStage(c, stage, safeError)
}

func AbortWithMessage(
	c *gin.Context,
	statusCode int,
	message string,
	opts ...relaymodel.WrapperErrorOptionFunc,
) {
	if stage, _ := c.Get(operationalFailureStageKey); stage == model.FailureStageBalance {
		abortBalanceFailure(GetMode(c), c, statusCode, opts)
		return
	}

	if IsPublicVideoRequest(c.Request.URL.Path, GetMode(c)) ||
		(strings.HasPrefix(c.Request.URL.Path, "/v1/") && (statusCode == http.StatusUnauthorized || statusCode >= 500)) {
		c.JSON(statusCode, relaymodel.PublicVideoError(statusCode))
		c.Abort()
		return
	}

	c.JSON(statusCode,
		relaymodel.WrapperErrorWithMessage(GetMode(c), statusCode, message, opts...),
	)
	c.Abort()
}

func GetMode(c *gin.Context) mode.Mode {
	m, exists := c.Get(Mode)
	if !exists {
		return mode.Unknown
	}

	v, ok := m.(mode.Mode)
	if !ok {
		panic(fmt.Sprintf("mode type error: %T, %v", v, v))
	}

	return v
}
