package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor/fal"
	"github.com/labring/aiproxy/core/relay/mode"
	"gorm.io/gorm"
	"io"
	"net/http"
	"strings"
	"time"
)

const NativeResultConfigKey model.ModelConfigKey = "x_token_platform_native_result_v1"

type nativeRouteBinding struct {
	Version         int             `json:"version"`
	Contract        json.RawMessage `json:"contract"`
	ChannelID       int             `json:"channelId"`
	Endpoint        string          `json:"endpoint"`
	CredentialScope string          `json:"credentialScope"`
	KeyFingerprint  string          `json:"keyFingerprint"`
	DeliveryBase    string          `json:"deliveryBase"`
}

// Native storage is deliberately not part of automatic startup migrations.
// Routes remain unavailable until reviewed additive storage and wallet exist.
func NativeTaskEngine() (*nativetask.Engine, bool) {
	wallet, ok := balance.Default.(nativetask.Wallet)
	if !ok || !model.NativeTaskStorageReady(model.LogDB) {
		return nil, false
	}
	return &nativetask.Engine{DB: model.LogDB, Wallet: wallet}, true
}

// nativeFailureStage mirrors image tasks so native rejections are filtered and
// labelled the same way in request logs.
func nativeFailureStage(status int) model.FailureStage {
	switch status {
	case http.StatusBadRequest, http.StatusConflict:
		return model.FailureStageValidation
	case http.StatusUnauthorized:
		return model.FailureStageAuth
	case http.StatusPaymentRequired:
		return model.FailureStageBalance
	default:
		return model.FailureStageRouting
	}
}
func recordNativeFailure(c *gin.Context, status int, code, message string) {
	middleware.SetOperationalFailure(c, nativeFailureStage(status), code, message)
}

// nativeTaskError writes the shared native envelope and logs the same code.
func nativeTaskError(c *gin.Context, status int, code string) {
	recordNativeFailure(c, status, code, nativetask.ErrorMessage(code))
	nativetask.WriteError(c.Writer, status, code)
	c.Abort()
}
func nativeTaskRuntime(c *gin.Context) {
	if _, ok := NativeTaskEngine(); !ok {
		nativeTaskError(c, 503, "native_execution_unavailable")
		return
	}
	c.Next()
}
func NativeTasks() []gin.HandlerFunc {
	return []gin.HandlerFunc{nativeTaskRuntime, replayNativeTask, middleware.NewDistribute(mode.NativeTasks), submitNativeTask}
}
func nativeHTTP(c *gin.Context) (*nativetask.HTTP, bool) {
	engine, ok := NativeTaskEngine()
	if !ok {
		nativeTaskError(c, 503, "native_execution_unavailable")
		return nil, false
	}
	return &nativetask.HTTP{Engine: engine, Identity: func(*http.Request) (string, int, error) {
		return middleware.GetGroup(c).ID, middleware.GetToken(c).ID, nil
	}, ResolvePoller: nativetask.ResolveFalPoller(model.GetChannelByID), OnError: func(status int, code, message string) {
		recordNativeFailure(c, status, code, message)
	}}, true
}
func submitNativeTask(c *gin.Context) {
	handler, ok := nativeHTTP(c)
	if !ok {
		return
	}
	handler.ResolvePlan = func(_ *http.Request, _ []byte) (nativetask.Plan, nativetask.Provider, error) {
		mc := middleware.GetModelConfig(c)
		if mc.Config[NativeResultConfigKey] == nil {
			// The model is not published for native tasks: the caller chose the
			// wrong model or endpoint. Every later failure is a server-side route.
			return nativetask.Plan{}, nil, nativetask.ErrNotNativeModel
		}
		encoded, err := json.Marshal(mc.Config[NativeResultConfigKey])
		if err != nil {
			return nativetask.Plan{}, nil, nativetask.ErrUnavailable
		}
		var binding nativeRouteBinding
		if json.Unmarshal(encoded, &binding) != nil || binding.Version != 1 {
			return nativetask.Plan{}, nil, nativetask.ErrUnavailable
		}
		selected, err := getInitialChannel(c, middleware.GetRoutingModel(c), mode.NativeTasks)
		if err != nil || selected == nil || selected.channel == nil {
			return nativetask.Plan{}, nil, nativetask.ErrUnavailable
		}
		channel := selected.channel
		if channel.ID != binding.ChannelID || channel.Type != model.ChannelTypeFal || channel.Key == "" || (channel.BaseURL != "" && strings.TrimRight(channel.BaseURL, "/") != "https://queue.fal.run") || model.ImageChannelKeyFingerprint(channel.Key) != binding.KeyFingerprint {
			return nativetask.Plan{}, nil, nativetask.ErrUnavailable
		}
		quote, rawQuote, err := mc.ImagePrepaymentQuote()
		if err != nil || quote == nil {
			return nativetask.Plan{}, nil, nativetask.ErrUnavailable
		}
		return nativetask.Plan{Contract: binding.Contract, ChannelID: channel.ID, Endpoint: binding.Endpoint, CredentialScope: binding.CredentialScope, KeyFingerprint: binding.KeyFingerprint, DeliveryBase: binding.DeliveryBase, QuoteJSON: rawQuote,
			Log: &model.NativeTaskLog{RequestAt: middleware.GetRequestAt(c), TokenName: middleware.GetToken(c).Name, Endpoint: "POST /v1/model-tasks",
				RequestSource: middleware.OperationalFieldsFromContext(c).RequestSource, IP: c.ClientIP(), Mode: int(mode.NativeTasks)}}, &fal.Client{Key: channel.Key}, nil
	}
	handler.Create(c.Writer, c.Request)
}
func GetNativeTask(c *gin.Context) {
	if handler, ok := nativeHTTP(c); ok {
		handler.Get(c.Writer, c.Request, c.Param("id"))
	}
}
func GetNativeTaskArtifact(c *gin.Context) {
	if handler, ok := nativeHTTP(c); ok {
		handler.GetArtifact(c.Writer, c.Request, c.Param("id"), c.Param("index"))
	}
}
func RecoverNativeTasks(ctx context.Context, owner string) error {
	engine, ok := NativeTaskEngine()
	if !ok {
		return nil
	}
	_, err := engine.RecoverOnce(ctx, owner, time.Now(), nativetask.ResolveFalPoller(model.GetChannelByID), nil)
	return err
}

// Accepted request IDs remain replayable after a registry or price change.
// New/reserved tasks still pass normal model authorization and rate limiting.
func replayNativeTask(c *gin.Context) {
	group, token := middleware.GetGroup(c), middleware.GetToken(c)
	if group.ID == "" || token.ID <= 0 {
		nativeTaskError(c, 401, "authentication_required")
		return
	}
	id := c.GetHeader("X-Request-Id")
	if !imageRequestID.MatchString(id) {
		nativeTaskError(c, 400, "invalid_request_id")
		return
	}
	task, err := model.GetNativeTask(model.LogDB, id, group.ID, token.ID)
	if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && task.Status == "reserved") {
		c.Next()
		return
	}
	if err != nil {
		nativeTaskError(c, 503, "task_store_unavailable")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, nativeresult.MaxBytes+1))
	if err != nil || len(raw) > nativeresult.MaxBytes {
		nativeTaskError(c, 400, "invalid_request")
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	if handler, ok := nativeHTTP(c); ok {
		handler.Create(c.Writer, c.Request)
	}
	c.Abort()
}

// Runtime negotiation is separate from per-model execution/billing evidence.
// Never advertise native execution on an unmigrated or non-prepayment runtime.
func nativeRuntimeFeatures() []string {
	if _, ok := NativeTaskEngine(); !ok {
		return nil
	}
	features := []string{"native_task_v1", "native_prepayment_recovery_v1", "native_private_trial_v1", "actual_cost_prepayment_v1"}
	if ownedartifact.Configured() {
		features = append(features, "native_owned_artifact_v1")
	}
	return features
}
