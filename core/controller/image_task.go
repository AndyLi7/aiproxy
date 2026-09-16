package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": code}})
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

	c.JSON(200, task)
}

type imageTaskProvider struct {
	Adapter string `json:"adapter"`
}

func selectImageTaskAdapter(
	c *gin.Context,
	providers map[string]imageTaskProvider,
) (*model.Channel, adaptor.ImageTaskAdapter) {
	selected, err := getInitialChannel(c, middleware.GetRoutingModel(c), mode.ImagesGenerations)
	if err != nil || selected == nil || selected.channel == nil {
		imageTaskHTTPError(c, 503, "channel_unavailable")
		return nil, nil
	}

	a, ok := adaptors.GetAdaptor(selected.channel.Type)
	if !ok {
		imageTaskHTTPError(c, 503, "adapter_unavailable")
		return nil, nil
	}

	imageAdapter, ok := a.(adaptor.ImageTaskAdapter)
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

func submitImageTask(c *gin.Context) {
	id := c.GetHeader("X-Request-Id")
	if !imageRequestID.MatchString(id) {
		imageTaskHTTPError(c, 400, "invalid_request_id")
		return
	}

	mc := middleware.GetModelConfig(c)
	// Async endpoints require a published Registry contract and reject demo transport IDs.
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

	if middleware.OperationalFieldsFromContext(c).RequestSource == model.RequestSourceAdminDemo {
		imageTaskHTTPError(c, 400, "unsupported_admin_image_execution")
		return
	}

	group, token := middleware.GetGroup(c), middleware.GetToken(c)
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

		c.JSON(http.StatusAccepted, existing)

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

	mappedBody, mappingErr := adaptor.MapImageProviderInput(
		mc.Config,
		imageAdapter.ImageAdapterName(),
		body,
	)
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

	requestAt := time.Now()
	// Queue image contracts expose a validated image count, so the known output
	// charge can be checked before any paid work is reserved or submitted.
	requiredBalance := math.Max(consume.CalculateAmountWithOptions(
		http.StatusOK,
		model.Usage{ImageOutputTokens: model.ZeroNullInt64(input.N)},
		requestUsage.Context,
		price,
		model.PriceSelectionOptions{
			DisableResolutionFuzzyMatch: mc.DisableResolutionFuzzyMatch,
			RequestAt:                   requestAt,
		},
	), middleware.GetGroupMinimumBalance())

	balanceConsumer := middleware.GetGroupBalanceConsumerFromContext(c)
	if balanceConsumer == nil || balanceConsumer.CheckBalance == nil {
		imageTaskHTTPError(c, 503, "balance_unavailable")
		return
	}

	if !balanceConsumer.CheckBalance(requiredBalance) {
		imageTaskHTTPError(c, 403, "group_balance_not_enough")
		return
	}

	info := &model.AsyncUsageInfo{
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

	task, created, err := model.ReserveImageTask(
		&model.ImageTask{
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
			ExpectedImages:     input.N,
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
		c.JSON(202, task)
		return
	}

	dispatchReservedImageTask(c, task, imageAdapter, mt, mappedBody)
}

func dispatchReservedImageTask(
	c *gin.Context,
	task *model.ImageTask,
	imageAdapter adaptor.ImageTaskAdapter,
	mt *meta.Meta,
	mappedBody []byte,
) {
	// One bounded submission; disconnects cannot cause a second paid invocation.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 50*time.Second)
	defer cancel()

	upstream, err := imageAdapter.SubmitImage(ctx, mt, mappedBody)
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
		c.JSON(202, task)

		return
	}

	if err = model.AcceptImageTask(task.ID, upstream); err != nil {
		imageTaskHTTPError(c, 503, "task_store_unavailable")
		return
	}

	task.Status = "queued"
	c.JSON(202, task)
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

	c.JSON(202, task)
	c.Abort()
}
