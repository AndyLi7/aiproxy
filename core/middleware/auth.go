package middleware

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/network"
	"github.com/labring/aiproxy/core/common/oncall"
	"github.com/labring/aiproxy/core/common/requesttrace"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
	relaymodel "github.com/labring/aiproxy/core/relay/model"
	"github.com/sirupsen/logrus"
)

type APIResponse struct {
	Data    any    `json:"data,omitempty"`
	Message string `json:"message,omitempty"`
	Success bool   `json:"success"`
}

func SuccessResponse(c *gin.Context, data any) {
	c.JSON(http.StatusOK, &APIResponse{
		Success: true,
		Data:    data,
	})
}

func ErrorResponse(c *gin.Context, code int, message string) {
	c.JSON(code, &APIResponse{
		Success: false,
		Message: message,
	})
}

func AdminAuth(c *gin.Context) {
	if config.AdminKey == "" {
		ErrorResponse(c, http.StatusUnauthorized, "unauthorized, admin key is not set")
		c.Abort()
		return
	}

	accessToken := c.Request.Header.Get("Authorization")
	if accessToken == "" {
		accessToken = c.Query("key")
	}

	accessToken = strings.TrimPrefix(accessToken, "Bearer ")
	accessToken = strings.TrimPrefix(accessToken, "sk-")

	if accessToken != config.AdminKey {
		ErrorResponse(c, http.StatusUnauthorized, "unauthorized, no access token provided")
		c.Abort()
		return
	}

	c.Set(Token, &model.TokenCache{
		Key: config.AdminKey,
	})

	group := c.Param("group")
	if group != "" {
		log := common.GetLogger(c)
		log.Data["gid"] = group
	}

	c.Next()
}

func TokenAuth(c *gin.Context) {
	authTrace := BeginRequestTraceStage(
		c,
		requesttrace.StageAuthentication,
		requesttrace.Attributes{},
	)

	authTraceFinished := false
	defer func() {
		if !authTraceFinished {
			authTrace.Finish(requesttrace.StatusError)
		}
	}()

	startedAt := time.Now()

	stageLogged := false
	defer func() {
		if stageLogged {
			return
		}

		common.LogLatencyEvent(c, common.LatencyEvent{
			Event:      "aiproxy_stage_finished",
			RequestID:  GetRequestID(c),
			Stage:      "token_group_auth",
			DurationMS: float64(time.Since(startedAt).Microseconds()) / 1000,
			Outcome:    "error",
			Status:     c.Writer.Status(),
			Method:     c.Request.Method,
			Path:       c.Request.URL.Path,
			ErrorType:  "authentication_rejected",
		})
	}()

	log := common.GetLogger(c)

	key := c.Request.Header.Get("Authorization")
	if key == "" {
		key = c.Request.Header.Get("X-Api-Key")
	}

	if key == "" {
		key = c.Request.Header.Get("X-Goog-Api-Key")
	}

	key = strings.TrimPrefix(
		strings.TrimPrefix(key, "Bearer "),
		"sk-",
	)

	var (
		token            model.TokenCache
		useInternalToken bool
	)

	if config.AdminKey != "" && config.AdminKey == key ||
		config.InternalToken != "" && config.InternalToken == key {
		token = model.TokenCache{
			Key: key,
		}
		useInternalToken = true
	} else {
		tokenCache, err := model.GetAndValidateToken(key)
		if err != nil {
			oncall.AlertDBError("TokenAuth", err)
			if key != "" {
				// Operators identify the key by its masked form; clients never see it.
				log.Data["key"] = maskTokenKey(key)
			}
			abortTokenRejection(c, tokenRejectionFor(err), err)

			return
		}

		// Clear DB error state on successful token validation
		oncall.ClearDBError("TokenAuth")

		token = *tokenCache
	}

	SetLogTokenFields(log.Data, token, useInternalToken)

	if len(token.Subnets) > 0 {
		if ok, err := network.IsIPInSubnets(c.ClientIP(), token.Subnets); err != nil {
			abortTokenRejection(c, tokenRejectionFor(model.ErrTokenUnavailable),
				fmt.Errorf("token %d subnet check: %w", token.ID, err))

			return
		} else if !ok {
			abortTokenRejection(c, tokenRejection{
				status:  http.StatusForbidden,
				code:    "api_key_ip_not_allowed",
				kind:    "permission_error",
				message: "This API key cannot be used from this network address. Check the key's allowed IP ranges.",
			}, fmt.Errorf("token %d used outside its subnets", token.ID))

			return
		}
	}

	modelCaches := model.LoadModelCaches()

	var group model.GroupCache
	if useInternalToken {
		group = model.GroupCache{
			Status:        model.GroupStatusInternal,
			AvailableSets: slices.Collect(maps.Keys(modelCaches.EnabledModelsBySet)),
		}
	} else {
		groupCache, err := model.CacheGetGroup(token.Group)
		if err != nil {
			abortTokenRejection(c, tokenRejectionFor(model.ErrTokenUnavailable),
				fmt.Errorf("failed to get group: %w", err))

			return
		}

		group = *groupCache
	}

	BindRequestTraceGroup(c, group.ID)

	c.Header("Group", group.ID)

	SetLogGroupFields(log.Data, group)

	if group.Status != model.GroupStatusEnabled && group.Status != model.GroupStatusInternal {
		AbortOperationally(
			c,
			model.FailureStageEntitlement,
			http.StatusForbidden,
			"group is disabled",
		)

		return
	}

	token.SetAvailableSets(group.GetAvailableSets())
	token.SetModelsBySet(modelCaches.EnabledModelsBySet)

	c.Set(Group, group)
	c.Set(Token, token)
	c.Set(ModelCaches, modelCaches)
	common.LogLatencyEvent(c, common.LatencyEvent{
		Event:      "aiproxy_stage_finished",
		RequestID:  GetRequestID(c),
		Stage:      "token_group_auth",
		DurationMS: float64(time.Since(startedAt).Microseconds()) / 1000,
		Outcome:    "success",
		Status:     http.StatusOK,
		Method:     c.Request.Method,
		Path:       c.Request.URL.Path,
	})

	stageLogged = true

	authTrace.Finish(requesttrace.StatusSuccess)

	authTraceFinished = true

	c.Next()
}

// tokenRejection is what an API client can act on after a failed key check.
// Messages never include the key's name, ID, subnets or the caller's address.
type tokenRejection struct {
	status  int
	code    string
	kind    string
	message string
}

func tokenRejectionFor(err error) tokenRejection {
	switch {
	case errors.Is(err, model.ErrTokenDisabled):
		return tokenRejection{
			status:  http.StatusForbidden,
			code:    "api_key_disabled",
			kind:    "permission_error",
			message: "This API key is disabled. Enable it in the console or use another key.",
		}
	case errors.Is(err, model.ErrTokenQuotaExhausted):
		// 429 rather than 402: 402 means the account balance is insufficient,
		// which a top-up fixes; this is a limit set on the key itself.
		return tokenRejection{
			status:  http.StatusTooManyRequests,
			code:    "api_key_quota_exhausted",
			kind:    "insufficient_quota",
			message: "This API key has reached its spending limit. Raise the key's limit, wait for its limit period to reset, or use another key.",
		}
	case errors.Is(err, model.ErrTokenUnavailable):
		return tokenRejection{
			status:  http.StatusServiceUnavailable,
			code:    "auth_unavailable",
			kind:    "api_error",
			message: "API key verification is temporarily unavailable. Retry later with the same request.",
		}
	default:
		return tokenRejection{
			status:  http.StatusUnauthorized,
			code:    "invalid_api_key",
			kind:    "authentication_error",
			message: "The API key is missing or invalid.",
		}
	}
}

// abortTokenRejection logs the same code it returns. The detail, which may name
// the key's ID, stays in server logs only.
func abortTokenRejection(c *gin.Context, rejection tokenRejection, detail error) {
	SetOperationalFailure(c, model.FailureStageAuth, rejection.code, rejection.message)
	common.GetLogger(c).Errorf("token rejected (%s): %v", rejection.code, detail)
	c.JSON(rejection.status, relaymodel.NewOpenAIError(rejection.status, relaymodel.OpenAIError{
		Code:    rejection.code,
		Message: rejection.message,
		Type:    rejection.kind,
	}))
	c.Abort()
}

func GetGroup(c *gin.Context) model.GroupCache {
	v, ok := c.MustGet(Group).(model.GroupCache)
	if !ok {
		panic(fmt.Sprintf("group cache type error: %T, %v", v, v))
	}

	return v
}

func GetToken(c *gin.Context) model.TokenCache {
	v, ok := c.MustGet(Token).(model.TokenCache)
	if !ok {
		panic(fmt.Sprintf("token cache type error: %T, %v", v, v))
	}

	return v
}

func GetModelCaches(c *gin.Context) *model.ModelCaches {
	v, ok := c.MustGet(ModelCaches).(*model.ModelCaches)
	if !ok {
		panic(fmt.Sprintf("model caches type error: %T, %v", v, v))
	}

	return v
}

func SetLogFieldsFromMeta(m *meta.Meta, fields logrus.Fields) {
	SetLogServiceTier(fields, m.RequestServiceTier)
	SetLogPromptCacheKey(fields, m.PromptCacheKey)
	SetLogRequestUser(fields, m.User)

	SetLogRequestIDField(fields, m.RequestID)

	SetLogModeField(fields, m.Mode)
	SetLogModelFields(fields, m.OriginModel)
	SetLogCapabilityField(fields, m.VideoCapability)
	SetLogActualModelFields(fields, m.ActualModel)

	SetLogGroupFields(fields, m.Group)
	SetLogTokenFields(fields, m.Token, false)
	SetLogChannelFields(fields, m.Channel)
}

func SetLogServiceTier(fields logrus.Fields, serviceTier string) {
	if serviceTier == "" {
		return
	}

	fields["service_tier"] = serviceTier
}

func SetLogPromptCacheKey(fields logrus.Fields, promptCacheKey string) {
	if promptCacheKey == "" {
		return
	}

	fields["prompt_cache_key"] = promptCacheKey
}

func SetLogRequestUser(fields logrus.Fields, user string) {
	if user == "" {
		return
	}

	fields["user"] = user
}

func SetLogModeField(fields logrus.Fields, mode mode.Mode) {
	fields["mode"] = mode.String()
}

func SetLogActualModelFields(fields logrus.Fields, actualModel string) {
	fields["actmodel"] = actualModel
}

func SetLogModelFields(fields logrus.Fields, model string) {
	fields["model"] = model
}

func SetLogCapabilityField(fields logrus.Fields, capability string) {
	if capability == "" {
		return
	}

	fields["capability"] = capability
}

func SetLogChannelFields(fields logrus.Fields, channel meta.ChannelMeta) {
	if channel.ID > 0 {
		fields["chid"] = channel.ID
	}

	if channel.Name != "" {
		fields["chname"] = channel.Name
	}

	if channel.Type > 0 {
		fields["chtype"] = int(channel.Type)
		fields["chtype_name"] = channel.Type.String()
	}
}

func SetLogRequestIDField(fields logrus.Fields, requestID string) {
	fields["reqid"] = requestID
}

func SetLogGroupFields(fields logrus.Fields, group model.GroupCache) {
	if group.ID != "" {
		fields["gid"] = group.ID
	}
}

func SetLogTokenFields(fields logrus.Fields, token model.TokenCache, internal bool) {
	if token.ID > 0 {
		fields["kid"] = token.ID
	}

	if token.Name != "" {
		fields["kname"] = token.Name
	}

	if token.Key != "" {
		fields["key"] = maskTokenKey(token.Key)
	}

	if internal {
		fields["internal"] = "true"
	}
}

func maskTokenKey(key string) string {
	if len(key) <= 8 {
		return "*****"
	}
	return key[:4] + "*****" + key[len(key)-4:]
}
