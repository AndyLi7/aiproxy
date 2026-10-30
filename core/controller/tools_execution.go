package controller

// RECONSTRUCTED internal Tools lifecycle. Reuses native validators, Fal transport
// and owned-artifact storage; intentionally has no API-wallet dependency.
import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/labring/aiproxy/core/relay/adaptor/fal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type toolsProvider interface {
	SubmitNative(ctx context.Context, endpoint string, input []byte) (string, error)
	PollNative(
		ctx context.Context,
		endpoint string,
		id string,
		contract *nativeresult.CompiledTask,
	) (nativeresult.PollResult, error)
	CancelNative(ctx context.Context, endpoint, id string) (string, error)
}
type toolsDependencies struct {
	provider func(*model.Channel) toolsProvider
	archive  func(context.Context, string, int, string) (ownedartifact.Receipt, error)
	download func(context.Context, string) (*http.Response, error)
}

func defaultToolsDependencies() toolsDependencies {
	return toolsDependencies{
		provider: func(ch *model.Channel) toolsProvider { return &fal.Client{Key: ch.Key} },
		archive:  ownedartifact.Store, download: ownedartifact.Download,
	}
}

func toolsOwned(c *gin.Context) (*model.ToolsExecution, bool) {
	a := toolsActor(c)

	id := c.Param("id")
	if !toolsIDPattern.MatchString(id) {
		toolsError(c, 404, "task_not_found")
		return nil, false
	}

	var task model.ToolsExecution

	err := model.DB.Where("id = ? AND user_id = ? AND agent_id = ? AND session_id = ?", id, a.UserID, a.AgentID, a.SessionID).
		First(&task).
		Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		toolsError(c, 404, "task_not_found")
		return nil, false
	}

	if err != nil {
		toolsError(c, 503, "task_store_unavailable")
		return nil, false
	}

	return &task, true
}

func toolsTaskResponse(c *gin.Context, t *model.ToolsExecution) {
	status := t.Status
	if status == "submitting" {
		status = "unconfirmed"
	}

	if status == "result_received" {
		status = "in_progress"
	}

	var output any
	if t.Output != "" && json.Unmarshal([]byte(t.Output), &output) != nil {
		toolsError(c, 503, "tools_result_invalid")
		return
	}

	var manifest map[int]ownedartifact.Receipt
	if t.Manifest != "" && json.Unmarshal([]byte(t.Manifest), &manifest) != nil {
		toolsError(c, 503, "tools_result_invalid")
		return
	}

	body := gin.H{
		"id":            t.ID,
		"model":         t.Model,
		"status":        status,
		"executionMode": "tools-internal",
		"upstreamCost":  gin.H{"status": "unreported"},
		"usage": gin.H{
			"operationId":   t.ID,
			"model":         t.Model,
			"provider":      "fal",
			"artifactCount": len(manifest),
			"status":        status,
		},
	}
	if output != nil {
		body["output"] = output
	}

	if t.ErrorCode != "" {
		body["error"] = gin.H{"code": t.ErrorCode}
	}

	c.Header("Cache-Control", "no-store")

	code := 200
	if c.Request.Method == http.MethodPost && c.FullPath() == "/internal/tools/model-tasks" {
		code = 202
	}

	c.JSON(code, body)
}

func (d toolsDependencies) create(c *gin.Context) {
	a := toolsActor(c)
	if !toolsIDPattern.MatchString(a.RequestID) {
		toolsError(c, 400, "invalid_request_id")
		return
	}

	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		toolsError(c, 400, "invalid_request")
		return
	}
	// Replay is bound to the exact owner/body/frozen revision, before any current
	// configuration lookup. A crash or lost acceptance is never a second submit.
	var old model.ToolsExecution

	err = model.DB.Where("id = ?", a.RequestID).First(&old).Error
	if err == nil {
		if old.UserID != a.UserID || old.AgentID != a.AgentID || old.SessionID != a.SessionID ||
			old.RequestHash != toolsDigest(raw) ||
			old.Revision != a.Revision {
			toolsError(c, 409, "request_id_conflict")
			return
		}

		toolsTaskResponse(c, &old)

		return
	}

	if !errors.Is(err, gorm.ErrRecordNotFound) {
		toolsError(c, 503, "task_store_unavailable")
		return
	}

	var request struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(raw, &request) != nil || !toolsText(request.Model, 191) {
		toolsError(c, 400, "invalid_request")
		return
	}

	mc, exists, err := resolveToolsModel(request.Model)
	if err != nil {
		toolsError(c, 503, "model_route_unavailable")
		return
	}

	if !exists {
		toolsError(c, 404, "model_not_found")
		return
	}

	mapping, binding, ch, ok := toolsDescriptor(mc)
	if !ok {
		toolsError(c, 503, "model_route_unavailable")
		return
	}

	revision, err := toolsRevision(mapping, binding)
	if err != nil {
		toolsError(c, 503, "model_route_unavailable")
		return
	}

	if a.Revision != revision {
		toolsError(c, 409, "tools_revision_changed")
		return
	}

	compiled, err := nativeresult.CompileTaskContract(binding.Contract)
	if err != nil {
		toolsError(c, 503, "model_route_unavailable")
		return
	}

	input, err := compiled.ValidateRequest(raw)
	if err != nil {
		toolsError(c, 422, "invalid_input")
		return
	}

	input, err = compiled.PrepareUpstreamInput(input)
	if err != nil {
		toolsError(c, 422, "invalid_input")
		return
	}

	frozen, err := json.Marshal(binding)
	if err != nil {
		toolsError(c, 503, "model_route_unavailable")
		return
	}

	now := time.Now().Unix()
	task := model.ToolsExecution{
		ID:          a.RequestID,
		UserID:      a.UserID,
		AgentID:     a.AgentID,
		SessionID:   a.SessionID,
		Model:       request.Model,
		RequestHash: toolsDigest(raw),
		Revision:    revision,
		BindingJSON: string(frozen),
		Status:      "submitting",
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	result := model.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&task)
	if result.Error != nil {
		toolsError(c, 503, "task_store_unavailable")
		return
	}

	if result.RowsAffected != 1 {
		// The winner owns the only submission; read its immutable identity again.
		d.create(cWithBody(c, raw))
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 45*time.Second)
	defer cancel()

	id, submitErr := d.provider(ch).SubmitNative(ctx, binding.Endpoint, input)

	values := map[string]any{"updated_at": time.Now().Unix()}
	switch {
	case submitErr != nil:
		values["status"] = "unconfirmed"

		values["error_code"] = "tools_submission_unconfirmed"
		if errors.Is(submitErr, adaptor.ErrImageSubmissionRejected) {
			values["status"] = "failed"
			values["error_code"] = "upstream_rejected"
		}
	case id == "":
		values["status"] = "unconfirmed"
		values["error_code"] = "tools_submission_unconfirmed"
	default:
		values["status"] = "pending"
		values["upstream_id"] = id
	}

	result = model.DB.Model(&model.ToolsExecution{}).
		Where("id = ? AND status = ?", task.ID, "submitting").
		Updates(values)
	if result.Error != nil || result.RowsAffected != 1 {
		toolsError(c, 503, "task_store_unavailable")
		return
	}

	if model.DB.First(&task, "id = ?", task.ID).Error != nil {
		toolsError(c, 503, "task_store_unavailable")
		return
	}

	toolsTaskResponse(c, &task)
}

func cWithBody(c *gin.Context, raw []byte) *gin.Context {
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	return c
}

// Keep one body reader helper; the complete body is already bounded by ToolsAuth.
func toolsBindingForTask(task *model.ToolsExecution) (nativeRouteBinding, *model.Channel, bool) {
	var binding nativeRouteBinding
	if json.Unmarshal([]byte(task.BindingJSON), &binding) != nil {
		return binding, nil, false
	}

	ch, err := model.GetChannelByID(binding.ChannelID)
	// Reads use the frozen account after disable; never a rotated key or host.
	if err != nil || ch == nil || ch.Type != model.ChannelTypeFal || ch.Key == "" ||
		model.ImageChannelKeyFingerprint(ch.Key) != binding.KeyFingerprint ||
		(ch.BaseURL != "" && ch.BaseURL != "https://queue.fal.run") {
		return binding, nil, false
	}

	return binding, ch, true
}

func toolsLease(task *model.ToolsExecution) (string, bool, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", false, err
	}

	owner := hex.EncodeToString(nonce[:])
	now := time.Now().Unix()
	result := model.DB.Model(&model.ToolsExecution{}).
		Where("id = ? AND (lease_until < ? OR lease_owner = '')", task.ID, now).
		Updates(map[string]any{"lease_owner": owner, "lease_until": now + 660})

	return owner, result.RowsAffected == 1, result.Error
}

func toolsSave(task *model.ToolsExecution, owner string, values map[string]any) error {
	values["updated_at"] = time.Now().Unix()

	r := model.DB.Model(&model.ToolsExecution{}).
		Where("id = ? AND lease_owner = ?", task.ID, owner).
		Updates(values)
	if r.Error != nil {
		return r.Error
	}

	if r.RowsAffected != 1 {
		return errors.New("tools lease lost")
	}

	return model.DB.First(task, "id = ?", task.ID).Error
}

func (d toolsDependencies) read(c *gin.Context, cancelRequest bool) {
	task, ok := toolsOwned(c)
	if !ok {
		return
	}

	if task.Status == "completed" || task.Status == "failed" || task.Status == "cancelled" ||
		task.Status == "unconfirmed" ||
		task.Status == "submitting" ||
		task.Status == "timed_out" {
		toolsTaskResponse(c, task)
		return
	}

	owner, claimed, err := toolsLease(task)
	if err != nil {
		toolsError(c, 503, "task_store_unavailable")
		return
	}

	if !claimed {
		toolsTaskResponse(c, task)
		return
	}

	defer model.DB.Model(&model.ToolsExecution{}).
		Where("id = ? AND lease_owner = ?", task.ID, owner).
		Updates(map[string]any{"lease_owner": "", "lease_until": 0})

	if model.DB.First(task, "id = ?", task.ID).Error != nil {
		toolsError(c, 503, "task_store_unavailable")
		return
	}
	// Recheck after acquiring the lease: another reader may have finished already.
	if task.Status == "completed" || task.Status == "failed" || task.Status == "cancelled" ||
		task.Status == "timed_out" {
		toolsTaskResponse(c, task)
		return
	}

	binding, ch, ok := toolsBindingForTask(task)
	if !ok {
		toolsError(c, 503, "tools_frozen_account_unavailable")
		return
	}

	ctx, finish := context.WithTimeout(c.Request.Context(), 10*time.Minute)
	defer finish()

	provider := d.provider(ch)
	if cancelRequest {
		cancelToolsTask(c, ctx, task, owner, binding, provider)
		return
	}

	if task.Status != "result_received" && time.Now().Unix()-task.CreatedAt > 600 {
		if toolsSave(
			task,
			owner,
			map[string]any{"status": "timed_out", "error_code": "generation_timeout"},
		) != nil {
			toolsError(c, 503, "task_store_unavailable")
			return
		}

		toolsTaskResponse(c, task)

		return
	}

	if task.Status != "result_received" && !pollToolsTask(c, ctx, task, owner, binding, provider) {
		return
	}

	if task.Status == "result_received" && !d.archiveResult(c, ctx, task, owner, binding) {
		return
	}

	toolsTaskResponse(c, task)
}

func (d toolsDependencies) artifact(c *gin.Context) {
	task, ok := toolsOwned(c)
	if !ok {
		return
	}

	index, err := strconv.Atoi(c.Param("index"))

	var manifest map[int]ownedartifact.Receipt
	if err != nil || index < 0 || index > 63 || task.Status != "completed" ||
		json.Unmarshal([]byte(task.Manifest), &manifest) != nil {
		toolsError(c, 404, "artifact_not_found")
		return
	}

	receipt, ok := manifest[index]
	if !ok {
		toolsError(c, 404, "artifact_not_found")
		return
	}

	identity, err := json.Marshal([]string{task.UserID, task.AgentID, task.SessionID})
	if err != nil {
		toolsError(c, 503, "tools_result_invalid")
		return
	}

	h := nativetask.HTTP{
		Download: func(r *http.Request, key string) (*http.Response, error) { return d.download(r.Context(), key) },
	}
	h.ServeOwnedArtifact(
		c.Writer,
		c.Request,
		"tools:"+toolsDigest(identity),
		0,
		"tools-"+toolsDigest([]byte(task.ID)),
		index,
		receipt,
	)
}
func CreateToolsNativeTask(c *gin.Context)  { defaultToolsDependencies().create(c) }
func GetToolsNativeTask(c *gin.Context)     { defaultToolsDependencies().read(c, false) }
func CancelToolsNativeTask(c *gin.Context)  { defaultToolsDependencies().read(c, true) }
func GetToolsNativeArtifact(c *gin.Context) { defaultToolsDependencies().artifact(c) }

func cancelToolsTask(
	c *gin.Context,
	ctx context.Context,
	task *model.ToolsExecution,
	owner string,
	binding nativeRouteBinding,
	provider toolsProvider,
) {
	var err error

	if task.CancelAttempted {
		toolsTaskResponse(c, task)
		return
	}

	if task.Status == "result_received" {
		toolsError(c, 409, "task_not_cancellable")
		return
	}
	// Persist intent before the best-effort call; retries do not send it twice.
	if toolsSave(
		task,
		owner,
		map[string]any{
			"status":           "cancellation_unconfirmed",
			"error_code":       "cancellation_unconfirmed",
			"cancel_attempted": true,
		},
	) != nil {
		toolsError(c, 503, "task_store_unavailable")
		return
	}

	callCtx, done := context.WithTimeout(ctx, 45*time.Second)
	outcome, e := provider.CancelNative(callCtx, binding.Endpoint, task.UpstreamID)

	done()

	if e == nil && outcome == fal.NativeCancelRequested {
		err = toolsSave(
			task,
			owner,
			map[string]any{"status": "cancellation_requested", "error_code": ""},
		)
	}

	if e == nil && outcome == fal.NativeCancelAlreadyCompleted {
		err = toolsSave(task, owner, map[string]any{"status": "pending", "error_code": ""})
	}

	if err != nil {
		toolsError(c, 503, "task_store_unavailable")
		return
	}

	toolsTaskResponse(c, task)
}

func pollToolsTask(
	c *gin.Context,
	ctx context.Context,
	task *model.ToolsExecution,
	owner string,
	binding nativeRouteBinding,
	provider toolsProvider,
) bool {
	compiled, e := nativeresult.CompileTaskContract(binding.Contract)
	if e != nil {
		toolsError(c, 503, "tools_frozen_contract_unavailable")
		return false
	}

	callCtx, done := context.WithTimeout(ctx, 45*time.Second)
	result, e := provider.PollNative(callCtx, binding.Endpoint, task.UpstreamID, compiled)

	done()

	if e != nil {
		if errors.Is(e, nativeresult.ErrInvalid) || errors.Is(e, nativeresult.ErrTooLarge) {
			if toolsSave(
				task,
				owner,
				map[string]any{"status": "failed", "error_code": "upstream_result_rejected"},
			) != nil {
				toolsError(c, 503, "task_store_unavailable")
				return false
			}

			toolsTaskResponse(c, task)

			return false
		}

		toolsError(c, 503, "tools_poll_unavailable")

		return false
	}

	values := map[string]any{}
	switch result.Status {
	case "failed":
		values["status"] = "failed"
		values["error_code"] = "upstream_task_failed"
	case "result_received":
		if _, e = compiled.ValidateOutput(result.Output); e != nil {
			toolsError(c, 503, "tools_result_invalid")
			return false
		}

		values["status"] = "result_received"
		values["native_output"] = string(result.Output)
		values["error_code"] = ""
	default:
		if time.Now().Unix()-task.CreatedAt > 600 {
			values["status"] = "timed_out"
			values["error_code"] = "generation_timeout"
		} else if task.Status != "cancellation_requested" && task.Status != "cancellation_unconfirmed" {
			values["status"] = "in_progress"
		}
	}

	if len(values) > 0 {
		if toolsSave(task, owner, values) != nil {
			toolsError(c, 503, "task_store_unavailable")
			return false
		}
	}

	return true
}

func (d toolsDependencies) archiveResult(
	c *gin.Context,
	ctx context.Context,
	task *model.ToolsExecution,
	owner string,
	binding nativeRouteBinding,
) bool {
	var contract nativeresult.TaskContract
	if json.Unmarshal(binding.Contract, &contract) != nil {
		toolsError(c, 503, "tools_result_invalid")
		return false
	}

	artifacts, e := nativeresult.PlanArtifacts([]byte(task.NativeOutput), contract.Artifacts)
	if e != nil || len(artifacts) > 64 {
		toolsError(c, 503, "tools_result_invalid")
		return false
	}

	manifest := map[int]ownedartifact.Receipt{}
	if task.Manifest != "" && json.Unmarshal([]byte(task.Manifest), &manifest) != nil {
		toolsError(c, 503, "tools_result_invalid")
		return false
	}

	owned := map[string]nativeresult.OwnedFile{}
	for index, a := range artifacts {
		receipt, exists := manifest[index]
		if !exists {
			receipt, e = d.archive(ctx, "tools-"+toolsDigest([]byte(task.ID)), index, a.Source)
			if e != nil {
				toolsError(c, 503, "artifact_unavailable")
				return false
			}

			if receipt.Size < 1 || receipt.Size > ownedartifact.MaxBytes ||
				!toolsHashPattern.MatchString(receipt.SHA256) {
				toolsError(c, 503, "artifact_integrity_failed")
				return false
			}

			manifest[index] = receipt

			raw, encodeErr := json.Marshal(manifest)
			if encodeErr != nil {
				toolsError(c, 503, "tools_result_invalid")
				return false
			}

			if toolsSave(task, owner, map[string]any{"manifest": string(raw)}) != nil {
				toolsError(c, 503, "task_store_unavailable")
				return false
			}
		}

		owned[a.Pointer] = nativeresult.OwnedFile{
			URL: fmt.Sprintf(
				"%s/internal/tools/model-tasks/%s/artifacts/%d",
				binding.DeliveryBase,
				task.ID,
				index,
			),
			ContentType: receipt.Type,
		}
	}

	output, _, e := nativeresult.DeliverArtifacts(
		[]byte(task.NativeOutput),
		contract.Artifacts,
		owned,
		nativeresult.RemoveUnboundURLs,
	)
	if e != nil {
		toolsError(c, 503, "tools_result_invalid")
		return false
	}

	if toolsSave(
		task,
		owner,
		map[string]any{"output": string(output), "status": "completed", "error_code": ""},
	) != nil {
		toolsError(c, 503, "task_store_unavailable")
		return false
	}

	return true
}
