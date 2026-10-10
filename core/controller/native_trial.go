package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"gorm.io/gorm"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor/fal"
	"github.com/labring/aiproxy/core/relay/mode"
)

// Private trials use the same durable execution/wallet engine without inserting a
// public model configuration. These handlers MUST remain behind AdminAuth. Only
// an internal group and its enabled token may own a trial; customer groups cannot
// be charged through this administrative path.
type nativeTrialDependencies struct {
	engine   func() (*nativetask.Engine, bool)
	token    func(int) (*model.Token, error)
	group    func(string, bool) (*model.Group, error)
	channel  func(int) (*model.Channel, error)
	provider func(*model.Channel) nativetask.Provider
}

func nativeTrialDefaults() nativeTrialDependencies {
	return nativeTrialDependencies{NativeTaskEngine, model.GetTokenByID, model.GetGroupByID, model.GetChannelByID,
		func(c *model.Channel) nativetask.Provider { return &fal.Client{Key: c.Key} }}
}
func (d nativeTrialDependencies) owner(c *gin.Context) (*nativetask.Engine, string, int, bool) {
	tokenID, err := strconv.Atoi(c.Param("token"))
	groupID := c.Param("group")
	if err != nil || tokenID <= 0 || groupID == "" {
		middleware.ErrorResponse(c, 400, "invalid trial identity")
		return nil, "", 0, false
	}
	token, err := d.token(tokenID)
	if err != nil || token == nil || token.GroupID != groupID || token.Status != model.TokenStatusEnabled {
		middleware.ErrorResponse(c, 403, "trial identity unavailable")
		return nil, "", 0, false
	}
	group, err := d.group(groupID, false)
	if err != nil || group == nil || group.Status != model.GroupStatusInternal {
		middleware.ErrorResponse(c, 403, "trial requires an internal group")
		return nil, "", 0, false
	}
	engine, ok := d.engine()
	if !ok {
		middleware.ErrorResponse(c, 503, "native execution unavailable")
		return nil, "", 0, false
	}
	return engine, groupID, tokenID, true
}

type nativeTrialRequest struct {
	Contract        json.RawMessage `json:"contract"`
	Input           json.RawMessage `json:"input"`
	Quote           json.RawMessage `json:"quote"`
	ChannelID       int             `json:"channelId"`
	Endpoint        string          `json:"endpoint"`
	CredentialScope string          `json:"credentialScope"`
	KeyFingerprint  string          `json:"keyFingerprint"`
	DeliveryBase    string          `json:"deliveryBase"`
	// InputMeter has the shape of model.InputMeterConfigKey. Send it only
	// while the gateway advertises native_input_meter_v1: older gateways
	// refuse unknown trial fields.
	InputMeter json.RawMessage `json:"inputMeter,omitempty"`
}

func (d nativeTrialDependencies) create(c *gin.Context) {
	engine, group, token, ok := d.owner(c)
	if !ok {
		return
	}
	id := c.GetHeader("X-Request-Id")
	if !strings.HasPrefix(id, "trial_") {
		middleware.ErrorResponse(c, 400, "invalid trial request id")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, nativeresult.MaxBytes+1))
	// Preserve exact JSON numbers and reject duplicate keys, depth and size abuse.
	validator, compileErr := nativeresult.Compile([]byte(`{}`))
	if err != nil || compileErr != nil {
		middleware.ErrorResponse(c, 400, "invalid native trial")
		return
	}
	if _, err = validator.Validate(raw); err != nil {
		middleware.ErrorResponse(c, 400, "invalid native trial")
		return
	}
	var input nativeTrialRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil {
		middleware.ErrorResponse(c, 400, "invalid native trial")
		return
	}
	contract, err := nativeresult.CompileTaskContract(input.Contract)
	if err != nil {
		middleware.ErrorResponse(c, 400, "invalid native contract")
		return
	}
	var frozen nativeresult.TaskContract
	if json.Unmarshal(input.Contract, &frozen) != nil {
		middleware.ErrorResponse(c, 400, "invalid native contract")
		return
	}
	body, err := json.Marshal(struct {
		Model string          `json:"model"`
		Input json.RawMessage `json:"input"`
	}{frozen.Model, input.Input})
	if err != nil {
		middleware.ErrorResponse(c, 400, "invalid trial input")
		return
	}
	if _, err = contract.ValidateRequest(body); err != nil {
		middleware.ErrorResponse(c, 400, "invalid trial input")
		return
	}
	origin, err := url.Parse(input.DeliveryBase)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") {
		middleware.ErrorResponse(c, 400, "invalid trial delivery origin")
		return
	}
	channel, err := d.channel(input.ChannelID)
	if err != nil || channel == nil || channel.Type != model.ChannelTypeFal || channel.Key == "" ||
		(channel.Status != model.ChannelStatusEnabled && channel.Status != model.ChannelStatusDisabled) ||
		(channel.BaseURL != "" && strings.TrimRight(channel.BaseURL, "/") != "https://queue.fal.run") ||
		model.ImageChannelKeyFingerprint(channel.Key) != input.KeyFingerprint {
		middleware.ErrorResponse(c, 400, "trial channel changed or unavailable")
		return
	}
	// Disabled shadow channels are deliberately eligible here, never public routing.
	plan := nativetask.Plan{Contract: input.Contract, ChannelID: channel.ID, Endpoint: input.Endpoint,
		CredentialScope: input.CredentialScope, KeyFingerprint: input.KeyFingerprint, DeliveryBase: input.DeliveryBase, QuoteJSON: string(input.Quote), InputMeter: input.InputMeter,
		Log: &model.NativeTaskLog{RequestAt: time.Now(), Endpoint: "POST /api/native-trials", RequestSource: model.RequestSourceAdminDemo,
			IP: c.ClientIP(), Mode: int(mode.NativeTasks)}}
	// Trials are paid examples run by operators; log them as admin demos.
	if owner, tokenErr := d.token(token); tokenErr == nil && owner != nil {
		plan.Log.TokenName = string(owner.Name)
	}
	requestEngine := *engine
	requestEngine.Provider = d.provider(channel)
	task, err := requestEngine.Submit(c.Request.Context(), id, group, token, body, plan)
	if err != nil {
		// Do not expose internal provider credentials/errors. Durable accepted/unknown
		// states are polled using their frozen plan; never retry a different endpoint.
		if task == nil || task.Status == "reserved" {
			middleware.ErrorResponse(c, 409, "native trial could not be admitted")
			return
		}
	}
	writeNativeTrial(c, http.StatusAccepted, task)
}
func (d nativeTrialDependencies) read(c *gin.Context, artifact bool) {
	engine, group, token, ok := d.owner(c)
	if !ok {
		return
	}
	if !strings.HasPrefix(c.Param("id"), "trial_") {
		middleware.ErrorResponse(c, 404, "trial not found")
		return
	}
	handler := &nativetask.HTTP{Engine: engine, Identity: func(*http.Request) (string, int, error) { return group, token, nil }, ResolvePoller: nativetask.ResolveFalPoller(d.channel)}
	if artifact {
		handler.GetArtifact(c.Writer, c.Request, c.Param("id"), c.Param("index"))
	} else {
		// Reads never poll the provider; recovery workers advance trials too.
		task, err := model.GetNativeTask(engine.DB, c.Param("id"), group, token)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			middleware.ErrorResponse(c, 404, "trial not found")
			return
		}
		if err != nil {
			middleware.ErrorResponse(c, 503, "trial temporarily unavailable")
			return
		}
		writeNativeTrial(c, http.StatusOK, task)
	}
}

// Keep native JSON as text inside the admin envelope. The ordinary admin client
// parses JSON numbers into JavaScript doubles; embedding output objects there
// would silently round valid upstream integers before evidence is retained.
func writeNativeTrial(c *gin.Context, status int, task *model.NativeTask) {
	digest := sha256.Sum256([]byte(task.FrozenContract))
	data := gin.H{"id": task.ID, "model": task.Model, "status": task.Status, "billingSettled": task.BillingSettled,
		"contractDigest": "sha256:" + hex.EncodeToString(digest[:])}
	var receipt balance.PrepaymentReceipt
	actualBill := json.Unmarshal([]byte(task.BillingReceiptJSON), &receipt) == nil &&
		task.BillingSettled && receipt.ID == task.BillingOperationID && receipt.Status == "settled" &&
		receipt.ChargedMicros != nil && *receipt.ChargedMicros >= 0
	compiled, compileErr := nativeresult.CompileTaskContract([]byte(task.FrozenContract))
	delivery := false
	if compileErr == nil && task.Status == "completed" {
		_, outputErr := compiled.ValidateOutput([]byte(task.DeliveredOutput))
		delivery = outputErr == nil
	}
	artifacts, artifactsValid := nativeTrialArtifactList(task)
	delivery = delivery && artifactsValid
	data["verification"] = gin.H{"schemaCompiled": compileErr == nil, "routingVerified": task.UpstreamID != "",
		"billingVerified": actualBill, "deliveryVerified": delivery,
		"platformControlsVerified": compileErr == nil && task.UpstreamID != ""}
	if task.Status == "completed" {
		data["outputJSON"] = task.DeliveredOutput
		data["artifacts"] = artifacts
	}
	if task.Status == "failed" {
		data["errorCode"] = task.ErrorCode
		if issues := model.NativeTaskIssues(task); len(issues) > 0 {
			data["errorIssues"] = issues
		}
	}
	c.Header("Cache-Control", "private, no-store")
	c.JSON(status, middleware.APIResponse{Success: true, Data: data})
}

// Expose only owned indices and sizes, never storage keys or provider URLs.
func nativeTrialArtifactList(task *model.NativeTask) ([]gin.H, bool) {
	result := []gin.H{}
	if task.Status != "completed" {
		return result, false
	}
	var contract nativeresult.TaskContract
	if json.Unmarshal([]byte(task.FrozenContract), &contract) != nil {
		return result, false
	}
	planned, err := nativeresult.PlanArtifacts([]byte(task.DeliveredOutput), contract.Artifacts)
	if err != nil {
		return result, false
	}
	entries := map[int]ownedartifact.Receipt{}
	if task.ArtifactManifest != "" && json.Unmarshal([]byte(task.ArtifactManifest), &entries) != nil {
		return result, false
	}
	if len(entries) != len(planned) {
		return result, false
	}
	for index, artifact := range planned {
		receipt := entries[index]
		expected := strings.TrimRight(task.DeliveryBase, "/") + "/v1/model-tasks/" + url.PathEscape(task.ID) + "/artifacts/" + strconv.Itoa(index)
		if !ownedartifact.ValidReceipt(task.ID, index, receipt) || artifact.Source != expected {
			return []gin.H{}, false
		}
		result = append(result, gin.H{"index": index, "size": receipt.Size})
	}
	return result, true
}
func CreateNativeTrial(c *gin.Context)      { nativeTrialDefaults().create(c) }
func GetNativeTrial(c *gin.Context)         { nativeTrialDefaults().read(c, false) }
func GetNativeTrialArtifact(c *gin.Context) { nativeTrialDefaults().read(c, true) }
