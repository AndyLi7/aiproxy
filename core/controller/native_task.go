package controller

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor/fal"
	"github.com/labring/aiproxy/core/relay/mode"
	"gorm.io/gorm"
)

const NativeResultConfigKey = model.NativeResultConfigKey

type nativeRouteBinding = model.NativeRouteBinding

// Native storage is deliberately not part of automatic startup migrations.
// Routes remain unavailable until reviewed additive storage and wallet exist.
func NativeTaskEngine() (*nativetask.Engine, bool) {
	wallet, ok := balance.Default.(nativetask.Wallet)
	if !ok || !model.NativeTaskStorageReady(model.LogDB) {
		return nil, false
	}

	return &nativetask.Engine{
		DB:                  model.LogDB,
		Wallet:              wallet,
		InputMeter:          !config.DisableNativeInputMeter,
		AbandonReservations: !config.DisableNativeReservationStopLoss,
		QueueProviders: map[string]nativetask.QueueProvider{
			"fal": nativetask.FalQueueProvider(model.GetChannelByID),
		},
	}, true
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
	return []gin.HandlerFunc{
		nativeTaskRuntime,
		replayNativeTask,
		middleware.NewDistribute(mode.NativeTasks),
		submitNativeTask,
	}
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
	}, OnReserved: func(billingOperationID string) {
		// A rejection row written after the reservation carries the op: the
		// wallet may hold money for it, so the call failed rather than being
		// refused at a gate (key usage P2).
		middleware.SetCallBillingOperationID(c, billingOperationID)
	}, PublicModel: nativePublicModel(c)}, true
}

// nativePublicModel reports tasks under the ID customers call the model by
// (owner decision D2, 2026-10-10). GET /v1/model-tasks/:id bypasses the
// distributor, so the mapping reads the model configs, not the route.
func nativePublicModel(c *gin.Context) func(string) string {
	caches := model.LoadModelCaches()
	if value, ok := c.Get(middleware.ModelCaches); ok {
		if typed, ok := value.(*model.ModelCaches); ok && typed != nil {
			caches = typed
		}
	}

	return func(capabilityModel string) string {
		if caches == nil {
			return capabilityModel
		}
		return model.NativeCallableModelID(capabilityModel, caches.EnabledModelConfigsMap)
	}
}

// nativeAcceptedModel is the request's model when the distributor resolved
// it to this config by public_api_id or an alias, so the contract accepts it
// in place of its own capability ID; else "".
func nativeAcceptedModel(c *gin.Context, mc model.ModelConfig) string {
	requested := middleware.GetRequestedModel(c)

	identity, ok := model.PublicCapabilityIdentityFromConfig(mc)
	if !ok || requested == "" || requested == identity.CapabilityModel ||
		!identity.Accepts(requested) {
		return ""
	}

	return requested
}

func submitNativeTask(c *gin.Context) {
	handler, ok := nativeHTTP(c)
	if !ok {
		return
	}

	handler.ResolvePlan = func(_ *http.Request, _ []byte) (nativetask.Plan, nativetask.Provider, error) {
		return resolveNativePlan(c)
	}
	handler.Create(c.Writer, c.Request)
}

// resolveNativePlan routes a native submission to its bound channel. Every
// failure but a non-native model is logged with its reason (native_route.go).
// The identity, fingerprint and quote checks stay as a second line behind the
// binding filter in getInitialChannel.
func resolveNativePlan(c *gin.Context) (nativetask.Plan, nativetask.Provider, error) {
	mc := middleware.GetModelConfig(c)
	if model.IsAgentCredentialName(middleware.GetToken(c).Name) &&
		(mc.Config[AgentExecutionConfigKey] == nil || c.GetHeader("X-Agent-Execution-Revision") != agentExecutionRevision(mc)) {
		return nativetask.Plan{}, nil, nativetask.ErrUnavailable
	}

	if mc.Config[NativeResultConfigKey] == nil {
		// The model is not published for native tasks: the caller chose the
		// wrong model or endpoint. Every later failure is a server-side route.
		// Suggest only the model's own IDs on its other endpoint.
		var suggested []string
		if suggestion := middleware.NativeModelSuggestions(
			c,
			middleware.GetRequestModel(c),
		); suggestion.OtherEndpoint {
			suggested = suggestion.Models
		}

		return nativetask.Plan{}, nil, &nativetask.NotNativeModelError{SuggestedModels: suggested}
	}

	binding, ok := parseNativeRouteBinding(mc)
	if !ok {
		return rejectNativeRoute(c, nativeRouteBindingInvalid, binding, nil, nil)
	}

	selected, err := getInitialChannel(c, middleware.GetRoutingModel(c), mode.NativeTasks)
	if err != nil || selected == nil || selected.channel == nil {
		return rejectNativeRoute(c, nativeRouteBoundChannelUnavailable, binding, nil, err)
	}

	channel := selected.channel
	if channel.ID != binding.ChannelID || channel.Type != model.ChannelTypeFal ||
		(channel.BaseURL != "" && strings.TrimRight(channel.BaseURL, "/") != "https://queue.fal.run") {
		return rejectNativeRoute(c, nativeRouteChannelIdentityMismatch, binding, channel, nil)
	}

	if channel.Key == "" ||
		model.ImageChannelKeyFingerprint(channel.Key) != binding.KeyFingerprint {
		return rejectNativeRoute(c, nativeRouteKeyFingerprintMismatch, binding, channel, nil)
	}

	quote, rawQuote, err := mc.ImagePrepaymentQuote()
	if err != nil || quote == nil {
		return rejectNativeRoute(c, nativeRouteQuoteInvalid, binding, channel, err)
	}

	identity, err := model.ChannelIdentityOf(channel)
	if err != nil {
		return rejectNativeRoute(c, nativeRouteChannelIdentityMismatch, binding, channel, err)
	}

	return nativetask.Plan{
		AdmissionIdentityHash: identity.IdentityHash,
		Contract:              binding.Contract,
		ChannelID:             channel.ID,
		Endpoint:              binding.Endpoint,
		CredentialScope:       binding.CredentialScope,
		KeyFingerprint:        binding.KeyFingerprint,
		DeliveryBase:          binding.DeliveryBase,
		QuoteJSON:             rawQuote,
		InputMeter:            mc.InputMeterJSON(),
		AcceptedModel:         nativeAcceptedModel(c, mc),
		Log:                   nativeTaskLogInfo(c),
	}, &fal.Client{Key: channel.Key}, nil
}

// nativeTaskLogInfo is the call row of a public native submission: the
// request start and the source derived from the authenticated key. The wallet
// admission may restamp both with the hold's (an admit replay).
func nativeTaskLogInfo(c *gin.Context) *model.NativeTaskLog {
	return &model.NativeTaskLog{
		RequestAt: middleware.GetRequestAt(
			c,
		),
		TokenName: middleware.GetToken(c).Name,
		Endpoint:  "POST /v1/model-tasks",
		RequestSource: middleware.OperationalFieldsFromContext(
			c,
		).RequestSource,
		IP:             c.ClientIP(),
		Mode:           int(mode.NativeTasks),
		RequestedModel: middleware.GetRequestedModel(c),
	}
}

func GetNativeTask(c *gin.Context) {
	if handler, ok := nativeHTTP(c); ok {
		handler.Get(c.Writer, c.Request, c.Param("id"))
	}
}

// CancelNativeTask cancels a task still waiting in the own task queue
// (POST /v1/model-tasks/:id/cancel, spec §6.2).
func CancelNativeTask(c *gin.Context) {
	if handler, ok := nativeHTTP(c); ok {
		handler.Cancel(c.Writer, c.Request, c.Param("id"))
	}
}

// RunNativeQueueDispatcher sends queued native tasks until ctx ends.
func RunNativeQueueDispatcher(ctx context.Context) {
	nativetask.RunDispatcher(ctx, NativeTaskEngine)
}

// GetNativeQueueStatus reports the own task queue for the admin card
// (GET /api/native-queue/status, spec §7.3).
func GetNativeQueueStatus(c *gin.Context) {
	engine, ok := NativeTaskEngine()
	if !ok {
		middleware.ErrorResponse(c, http.StatusServiceUnavailable, "native execution unavailable")
		return
	}

	status, err := engine.ReadQueueStatus()
	if err != nil {
		middleware.ErrorResponse(
			c,
			http.StatusServiceUnavailable,
			"native queue status unavailable",
		)

		return
	}

	c.Header("Cache-Control", "no-store")
	middleware.SuccessResponse(c, status)
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

	_, err := engine.RecoverOnce(
		ctx,
		owner,
		time.Now(),
		nativetask.ResolveFalPoller(model.GetChannelByID),
		nil,
	)

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
	engine, ok := NativeTaskEngine()
	if !ok {
		return nil
	}

	features := []string{
		"native_task_v1",
		"native_prepayment_recovery_v1",
		"native_private_trial_v1",
		"actual_cost_prepayment_v1",
	}
	if ownedartifact.Configured() {
		features = append(features, "native_owned_artifact_v1")
		if len(os.Getenv("EXTERNAL_BALANCE_KEY")) >= 32 &&
			model.LoadModelCaches().ImageIdentityEvidence().Ready() {
			features = append(features, "openai_images_native_v1")
		}
	}
	// The application publishes input meters only while this is advertised.
	if engine.InputMeter {
		features = append(features, model.InputMeterFeature)
	}
	// The application writes public_api_id and aliases only while this is
	// advertised. DISABLE_PUBLIC_API_IDS=true stops advertising; keys already
	// written keep resolving, so a rollback has no window of 404s.
	if !config.DisablePublicAPIIDs {
		features = append(features, model.PublicAPIIDFeature)
	}

	return features
}
