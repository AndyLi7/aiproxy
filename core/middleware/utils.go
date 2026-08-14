package middleware

import (
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
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
	if IsPublicVideoRequest(c.Request.URL.Path, m) {
		c.JSON(statusCode, relaymodel.PublicVideoError(statusCode))
		c.Abort()
		return
	}

	c.JSON(statusCode,
		relaymodel.WrapperErrorWithMessage(m, statusCode, message, opts...),
	)
	c.Abort()
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
	if IsPublicVideoRequest(c.Request.URL.Path, GetMode(c)) {
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
