package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/labring/aiproxy/core/common/imagecapabilities"
	"github.com/labring/aiproxy/core/common/ownedimage"
	"math"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/common/consume"
	"github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/labring/aiproxy/core/relay/adaptors"
	relaycontroller "github.com/labring/aiproxy/core/relay/controller"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
	"gorm.io/gorm"
)

var imageRequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func ImageTasks() []gin.HandlerFunc {
	return []gin.HandlerFunc{
		replayImageTask,
		middleware.NewDistribute(mode.ImagesGenerations),
		submitImageTask,
	}
}

func imageTaskFingerprint(body []byte) (string, error) {
	var input map[string]any
	if err := json.Unmarshal(body, &input); err != nil {
		return "", err
	}

	canonical, err := json.Marshal(input)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(canonical)

	return hex.EncodeToString(sum[:]), nil
}

func imageTaskHTTPError(c *gin.Context, status int, code string) {
	middleware.SetRequestID(c, "image-trace-"+middleware.GenRequestID(time.Now()))
	message := imageTaskErrorMessage(code)
	if code == "group_balance_not_enough" {
		c.JSON(http.StatusPaymentRequired, gin.H{"error": gin.H{
			"message": "Your account balance is insufficient.", "type": "insufficient_quota", "code": "insufficient_balance", "param": nil,
		}})
		return
	}
	kind := "api_error"
	if status == http.StatusBadRequest || status == http.StatusConflict {
		kind = "invalid_request_error"
	} else if status == http.StatusNotFound {
		kind = "not_found_error"
	}
	if code == "unsupported_image_execution" {
		message = "This model does not support asynchronous image tasks. Check its published endpoint in the model catalog."
	}
	stage := model.FailureStageRouting
	if status == http.StatusBadRequest || status == http.StatusConflict {
		stage = model.FailureStageValidation
	}
	middleware.SetOperationalFailure(c, stage, code, message)
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message, "type": kind, "param": nil}})
}

func GetImageTask(c *gin.Context) {
	middleware.SetRequestID(c, "image-poll-"+middleware.GenRequestID(time.Now()))

	task, err := model.GetImageTask(
		c.Param("id"),
		middleware.GetGroup(c).ID,
		middleware.GetToken(c).ID,
	)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		imageTaskHTTPError(c, 404, "task_not_found")
		return
	}

	if err != nil {
		imageTaskHTTPError(c, 503, "task_store_unavailable")
		return
	}

	enrichCompletedImageTask(c.Request.Context(), task)
	c.JSON(200, publicImageTask(c, task))
}

type imageTaskProvider struct {
	Adapter string `json:"adapter"`
}

func selectImageTaskAdapter(
	c *gin.Context,
	providers map[string]imageTaskProvider,
) (*model.Channel, adaptor.ImageTaskExecutor) {
	selected, err := getInitialChannel(c, middleware.GetRoutingModel(c), mode.ImagesGenerations)
	if err != nil || selected == nil || selected.channel == nil {
		common.GetLogger(c).Warnf("image channel selection failed: route=%s reason=%v", middleware.GetRoutingModel(c), err)
		imageTaskHTTPError(c, 503, "channel_unavailable")
		return nil, nil
	}

	c.Set("image_initial_channel", selected)

	a, ok := adaptors.GetAdaptor(selected.channel.Type)
	if !ok {
		imageTaskHTTPError(c, 503, "adapter_unavailable")
		return nil, nil
	}

	imageAdapter, ok := a.(adaptor.ImageTaskExecutor)
	if !ok {
		imageTaskHTTPError(c, 400, "unsupported_image_adapter")
		return nil, nil
	}

	matched := false
	for _, provider := range providers {
		if provider.Adapter == imageAdapter.ImageAdapterName() {
			matched = true
		}
	}

	if !matched {
		imageTaskHTTPError(c, 400, "provider_contract_mismatch")
		return nil, nil
	}

	return selected.channel, imageAdapter
}

//nolint:gocyclo // Admission combines authentication, contract, pricing and idempotency gates.
func submitImageTask(c *gin.Context) {
	id := c.GetHeader("X-Request-Id")
	if !imageRequestID.MatchString(id) {
		imageTaskHTTPError(c, 400, "invalid_request_id")
		return
	}

	mc := middleware.GetModelConfig(c)
	// Async endpoints require a published Registry contract, including private demo projections.
	var wrapper struct {
		ID       string `json:"entry_id"`
		Contract struct {
			Providers map[string]imageTaskProvider `json:"providers"`
			Execution struct {
				Mode   string `json:"mode"`
				Output string `json:"output"`
			} `json:"execution"`
		} `json:"contract"`
	}

	encoded, err := json.Marshal(mc.Config["x_token_platform_capability_contract"])
	if err != nil {
		imageTaskHTTPError(c, 400, "unsupported_image_execution")
		return
	}

	if json.Unmarshal(encoded, &wrapper) != nil || wrapper.Contract.Execution.Mode != "async" ||
		wrapper.Contract.Execution.Output != "image" {
		imageTaskHTTPError(c, 400, "unsupported_image_execution")
		return
	}

	group, token := middleware.GetGroup(c), middleware.GetToken(c)
	adminDemo := middleware.OperationalFieldsFromContext(c).RequestSource == model.RequestSourceAdminDemo
	if c.GetHeader(middleware.OperationalLogSourceHeader) == model.RequestSourceAdminDemo &&
		group.Status != model.GroupStatusInternal {
		imageTaskHTTPError(c, 403, "internal_admin_token_required")
		return
	}
	if token.ID == 0 || group.ID == "" {
		imageTaskHTTPError(c, 403, "customer_token_required")
		return
	}

	body, err := common.GetRequestBodyReusable(c.Request)
	if err != nil {
		imageTaskHTTPError(c, 400, "invalid_request")
		return
	}

	fingerprint, err := imageTaskFingerprint(body)
	if err != nil {
		imageTaskHTTPError(c, 400, "invalid_request")
		return
	}

	existing, err := model.GetImageTask(id, group.ID, token.ID)
	if err == nil {
		if existing.Fingerprint != fingerprint {
			imageTaskHTTPError(c, 409, "request_id_conflict")
			return
		}

		c.JSON(http.StatusAccepted, publicImageTask(c, existing))

		return
	}

	if !errors.Is(err, gorm.ErrRecordNotFound) {
		imageTaskHTTPError(c, 503, "task_store_unavailable")
		return
	}

	channel, imageAdapter := selectImageTaskAdapter(c, wrapper.Contract.Providers)
	if channel == nil {
		return
	}

	contract := imageProviderContract(c)

	var (
		mappedBody      []byte
		mappingErr      error
		selectedBinding registryvalidation.ProviderBinding
	)
	if registryvalidation.HasProviderContracts(contract) {
		mappedBody, selectedBinding, mappingErr = mapChannelProviderInput(
			contract,
			channel,
			middleware.GetRoutingModel(c),
			body,
		)
	} else {
		if _, sync := imageAdapter.(adaptor.SyncImageTaskAdapter); sync {
			imageTaskHTTPError(c, 503, "provider_binding_required")
			return
		}

		mappedBody, mappingErr = adaptor.MapImageProviderInput(
			mc.Config,
			imageAdapter.ImageAdapterName(),
			body,
		)
	}

	if mappingErr != nil {
		imageTaskHTTPError(c, 503, "provider_mapping_unavailable")
		return
	}

	mt := NewMetaByContext(c, channel, mode.ImagesGenerations)

	price, err := relaycontroller.GetImagesRequestPrice(c, mc)
	if err != nil {
		imageTaskHTTPError(c, 400, "invalid_image_price")
		return
	}

	if price.HasImageBilling() &&
		(!imagecapabilities.SupportsMeasuredBilling(imageAdapter.ImageAdapterName()) || price.ImageBilling == nil || len(price.ConditionalPrices) != 0) {
		imageTaskHTTPError(c, 400, "measured_image_billing_not_supported_by_queue_adapter")
		return
	}

	currency, version, pricingOK := mc.RetailPricingMetadata()
	if !pricingOK {
		imageTaskHTTPError(c, 503, "pricing_metadata_unavailable")
		return
	}

	var input struct {
		N int `json:"n"`
	}
	if json.Unmarshal(body, &input) != nil || input.N < 1 {
		imageTaskHTTPError(c, 400, "invalid_image_count")
		return
	}

	requestUsage, err := relaycontroller.GetImagesRequestUsage(c, mc)
	if err != nil {
		imageTaskHTTPError(c, 400, "invalid_image_usage")
		return
	}

	metering, err := registryvalidation.ResolveImageMetering(
		contract,
		selectedBinding,
		mappedBody,
		input.N,
		model.ImageBillingMaxOutputs,
		price.HasImageBilling(),
	)
	if err != nil {
		imageTaskHTTPError(c, 400, "invalid_image_metering")
		return
	}

	if metering.Explicit {
		requestUsage.Context.ImageUsage = &model.ImageUsage{
			Version:    1,
			State:      "incomplete",
			Scenario:   "generation",
			InputCount: &metering.InputCount,
			Outputs:    []model.ImageUsageOutput{},
		}
	}

	// Procurement costs are recorded by the application. Internal example tasks
	// must never create a customer charge, even when a retail price is configured.
	if adminDemo {
		price = model.Price{}
	}

	requestAt := time.Now()
	// Queue image contracts expose a validated image count, so the known output
	// charge can be checked before any paid work is reserved or submitted.
	requiredBalance := math.Max(consume.CalculateAmountWithOptions(
		http.StatusOK,
		model.Usage{ImageOutputTokens: model.ZeroNullInt64(metering.MaximumOutputs)},
		requestUsage.Context,
		price,
		model.PriceSelectionOptions{
			DisableResolutionFuzzyMatch: mc.DisableResolutionFuzzyMatch,
			RequestAt:                   requestAt,
		},
	), middleware.GetGroupMinimumBalance())

	if price.HasImageBilling() {
		maximum, err := model.QueueImageMaximumAmount(
			price,
			metering.MaximumOutputs,
			metering.InputCount,
			metering.MaxOutputPixels,
		)
		if err != nil {
			imageTaskHTTPError(c, 400, "invalid_image_price")
			return
		}

		requiredBalance = math.Max(maximum, middleware.GetGroupMinimumBalance())
	}

	if !adminDemo {
		balanceConsumer := middleware.GetGroupBalanceConsumerFromContext(c)
		if balanceConsumer == nil || balanceConsumer.CheckBalance == nil {
			imageTaskHTTPError(c, 503, "balance_unavailable")
			return
		}

		if !balanceConsumer.CheckBalance(requiredBalance) {
			imageTaskHTTPError(c, 403, "group_balance_not_enough")
			return
		}
	}

	info := &model.AsyncUsageInfo{
		InternalImageTask:           adminDemo,
		RequestID:                   id,
		RequestAt:                   requestAt,
		UsageContext:                requestUsage.Context,
		Mode:                        int(mode.ImagesGenerations),
		Model:                       mt.OriginModel,
		Capability:                  middleware.GetResolvedCapability(c),
		ChannelID:                   mt.Channel.ID,
		BaseURL:                     mt.Channel.BaseURL,
		GroupID:                     group.ID,
		TokenID:                     token.ID,
		TokenName:                   token.Name,
		PricingCurrency:             currency,
		PricingVersion:              version,
		Price:                       price,
		DisableResolutionFuzzyMatch: mc.DisableResolutionFuzzyMatch,
	}

	var rawWrapper struct {
		Contract json.RawMessage `json:"contract"`
	}
	if json.Unmarshal(encoded, &rawWrapper) != nil {
		imageTaskHTTPError(c, 503, "invalid_contract")
		return
	}

	if registryvalidation.HasProviderContracts(rawWrapper.Contract) {
		frozen, freezeErr := registryvalidation.FreezeProviderBinding(
			rawWrapper.Contract,
			selectedBinding,
		)
		if freezeErr != nil {
			imageTaskHTTPError(c, 503, "invalid_provider_binding")
			return
		}

		rawWrapper.Contract = frozen
	}

	task, created, err := model.ReserveImageTask(
		&model.ImageTask{
			ArchiveRequired:    ownedimage.Configured(),
			ID:                 id,
			Model:              wrapper.ID,
			RequestModel:       middleware.GetRequestedModel(c),
			ValidationContract: string(rawWrapper.Contract),
			GroupID:            group.ID,
			TokenID:            token.ID,
			Fingerprint:        fingerprint,
			UpstreamModel:      mt.ActualModel,
			ChannelType:        mt.Channel.Type,
			KeyFingerprint:     model.ImageChannelKeyFingerprint(mt.Channel.Key),
			ExpectedImages:     metering.MaximumOutputs,
			RequestSummary:     imageTaskLogRequestSummary(body),
		},
		info,
		middleware.OperationalFieldsFromContext(c),
	)
	if errors.Is(err, model.ErrImageTaskConflict) {
		imageTaskHTTPError(c, 409, "request_id_conflict")
		return
	}

	if err != nil {
		imageTaskHTTPError(c, 503, "task_store_unavailable")
		return
	}

	if !created {
		c.JSON(202, publicImageTask(c, task))
		return
	}

	dispatchImageTaskWithFailover(c, task, imageAdapter, mt, mappedBody)
}

// Replays are authorized by token ownership and the reserved schema snapshot;
// they do not depend on today's model publication, balance, or channel health.
func replayImageTask(c *gin.Context) {
	id := c.GetHeader("X-Request-Id")
	if !imageRequestID.MatchString(id) {
		imageTaskHTTPError(c, 400, "invalid_request_id")
		c.Abort()
		return
	}

	task, err := model.GetImageTask(id, middleware.GetGroup(c).ID, middleware.GetToken(c).ID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return
	}

	if err != nil {
		imageTaskHTTPError(c, 503, "task_store_unavailable")
		c.Abort()
		return
	}

	body, err := common.GetRequestBodyReusable(c.Request)

	var input map[string]any
	if err != nil || json.Unmarshal(body, &input) != nil || input["model"] != task.RequestModel {
		imageTaskHTTPError(c, 409, "request_id_conflict")
		c.Abort()
		return
	}

	var contract struct {
		ID string `json:"entry_id"`
	}
	if json.Unmarshal([]byte(task.ValidationContract), &contract) != nil || contract.ID == "" {
		imageTaskHTTPError(c, 503, "reserved_contract_unavailable")
		c.Abort()
		return
	}

	input["model"] = contract.ID

	body, err = json.Marshal(input)
	if err != nil {
		imageTaskHTTPError(c, 409, "request_id_conflict")
		c.Abort()
		return
	}

	normalized, validationErr := registryvalidation.ValidateImage(
		[]byte(task.ValidationContract),
		contract.ID,
		body,
	)
	if validationErr != nil {
		imageTaskHTTPError(c, 409, "request_id_conflict")
		c.Abort()
		return
	}

	if json.Unmarshal(normalized, &input) != nil {
		imageTaskHTTPError(c, 503, "reserved_contract_unavailable")
		c.Abort()
		return
	}

	input["model"] = task.RequestModel

	normalized, err = json.Marshal(input)
	if err != nil {
		imageTaskHTTPError(c, 503, "reserved_contract_unavailable")
		c.Abort()
		return
	}

	fp, err := imageTaskFingerprint(normalized)
	if err != nil || fp != task.Fingerprint {
		imageTaskHTTPError(c, 409, "request_id_conflict")
		c.Abort()
		return
	}

	c.JSON(202, publicImageTask(c, task))
	c.Abort()
}

func dispatchSyncImageTask(
	c *gin.Context,
	ctx context.Context,
	task *model.ImageTask,
	a adaptor.SyncImageTaskAdapter,
	mt *meta.Meta,
	body []byte,
) {
	result, err := a.GenerateImage(ctx, mt, body, []byte(task.ValidationContract))
	finishSyncImageTask(c, task, result, err)
}

func finishSyncImageTask(c *gin.Context, task *model.ImageTask, result adaptor.ImageTaskResult, err error) {
	if err != nil {
		status := "submission_unknown"

		var taskError *model.ImageTaskError
		if errors.Is(err, adaptor.ErrImageSubmissionRejected) {
			status = "failed"
			taskError = &model.ImageTaskError{
				Code:    "submission_rejected",
				Message: "Upstream rejected image submission",
			}
		}

		if model.SetImageTaskResult(task.ID, status, nil, taskError) != nil {
			imageTaskHTTPError(c, 503, "task_store_unavailable")
			return
		}

		task.Status = status
		task.Error = taskError
		c.JSON(202, publicImageTask(c, task))

		return
	}

	if result.Status != "completed" || len(result.Data) == 0 ||
		(task.ExpectedImages > 0 && len(result.Data) > task.ExpectedImages) {
		taskError := &model.ImageTaskError{
			Code:    "invalid_result",
			Message: "Image generation failed",
		}
		if model.SetImageTaskResult(task.ID, "failed", nil, taskError) != nil {
			imageTaskHTTPError(c, 503, "task_store_unavailable")
			return
		}

		task.Status = "failed"
		task.Error = taskError
		c.JSON(202, publicImageTask(c, task))

		return
	}

	if model.CompleteSyncImageTask(task.ID, result.Data) != nil {
		imageTaskHTTPError(c, 503, "task_store_unavailable")
		return
	}

	task.Status = "completed"
	task.Data = result.Data
	c.JSON(202, publicImageTask(c, task))
}
