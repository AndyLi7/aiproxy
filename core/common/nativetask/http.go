package nativetask

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/model"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// HTTP is embedded behind the gateway's authenticated, rate-limited middleware.
// Identity and plans are server-resolved callbacks, never values from headers.
type HTTP struct {
	Engine        *Engine
	Identity      func(*http.Request) (string, int, error)
	ResolvePlan   func(*http.Request, []byte) (Plan, Provider, error)
	ResolvePoller ResolvePoller
	Archive       ArchiveFunc
	Download      func(*http.Request, string) (*http.Response, error)
	// DownloadRange reads bytes from..to (inclusive) of a stored file.
	DownloadRange func(r *http.Request, key string, from, to int64) (*http.Response, error)
	// OnError lets the gateway log the same code and message the client receives.
	OnError func(status int, code, message string)
	// OnReserved receives the task's billing operation when a submission fails
	// after its reservation, before the error is written, unless the wallet
	// explicitly refused the hold. The gateway puts it on the request's
	// rejection row: money may be held for that operation (key usage P2).
	OnReserved func(billingOperationID string)
	// PublicModel maps a task's model (the capability ID it was frozen with)
	// to the ID customers call it by. Nil, or an unknown ID, keeps it.
	PublicModel func(string) string
	// limiter defaults to the process-wide customer read limits.
	limiter *readLimiter
}

func (h *HTTP) reads() *readLimiter {
	if h.limiter != nil {
		return h.limiter
	}
	return customerReads
}

// errorMessages tell API clients, including generated client code, what to
// change. A bare code such as task_not_found was read as a model problem.
var errorMessages = map[string]string{
	"task_not_found":               "No task with this ID exists for this API key. If your POST may not have reached us (timeout or network error), resend the exact same request body with the same X-Request-Id; it is idempotent and is not charged twice. Do not keep polling an ID that returns 404.",
	"artifact_not_found":           "No artifact at that index is available to this API key. Check the task ID, the index and the key that submitted the task.",
	"request_id_conflict":          "This request ID is already used by a different request. Reuse the original body to retry, or choose a new ID for a new task.",
	"invalid_request_id":           "X-Request-Id must be 1-128 letters, digits, '_' or '-'. Use a unique ID for each new task and reuse it only for retries.",
	"invalid_request":              "The request body must be valid JSON matching this model's input schema.",
	"invalid_input":                "The input does not match this model's input schema. Check the model's API documentation.",
	"model_not_found":              "This ID cannot be called with this API key. Do not resend it: use an ID from suggested_models or an `id` from GET /v1/models.",
	"native_model_unavailable":     "This model is served on a different endpoint. Read its openapi_url in GET /v1/models; suggested_models lists the IDs to use there.",
	"model_route_unavailable":      "This model is temporarily unavailable. Retry later with the same X-Request-Id.",
	"authentication_required":      "A valid API key is required.",
	"insufficient_balance":         "Your balance does not cover this request's hold; see pricing.prepayment in /v1/models.",
	"too_many_active_tasks":        "Too many of your tasks are generating or still being billed. Wait a moment for one to finish, then retry with the same X-Request-Id.",
	"rate_limit_exceeded":          "Too many requests for this API key. Poll each task about every 5 seconds and download results a few at a time.",
	"model_unavailable":            "This model is temporarily unavailable. Retry later with the same X-Request-Id.",
	"task_store_unavailable":       "Task storage is temporarily unavailable. Retry querying the same task before submitting another task.",
	"task_temporarily_unavailable": "This task is temporarily unavailable. Retry querying the same task later.",
	"native_execution_unavailable": "Task execution is temporarily unavailable. Retry within a few minutes with the same request ID.",
	"artifact_unavailable":         "The task result is temporarily unavailable. Retry downloading it later.",
	"artifact_integrity_failed":    "The task result could not be verified. Contact support with the task ID.",
	"range_not_satisfiable":        "The requested byte range starts beyond the end of the file. Request a range within the file's size, or omit the Range header.",
	"billing_status_unavailable":   "Billing status is temporarily unavailable. Retry querying the same task later.",
	"task_not_cancellable":         "This task is no longer waiting in the queue (being sent, sent, or ended), so it cannot be cancelled. Poll it with GET /v1/model-tasks/{id}.",
}

// cancelNotFoundMessage answers a cancel of a task that does not exist.
const cancelNotFoundMessage = "No task with this ID exists for this API key; there is nothing to cancel."

// queueFullMessage is the 429 too_many_active_tasks message of a queued
// submission refused because the account's queue is full (spec §4.1). A
// direct submission keeps the too_many_active_tasks message.
const queueFullMessage = "Too many of your tasks are waiting in the queue. Wait for some of them to start, then retry with the same X-Request-Id."

// failureMessages explain the error code of a task that ended as failed.
// Every listed failure is refunded in full by SyncBilling or recovery.
var failureMessages = map[string]string{
	"upstream_task_failed":           "The provider could not generate this result. You were not charged; submit a new task with a new X-Request-Id.",
	"upstream_result_rejected":       "The provider returned a result we could not deliver. You were not charged; submit a new task with a new X-Request-Id.",
	"upstream_rejected":              "The provider rejected this request before generating. You were not charged.",
	model.InvalidParametersCode:      "The provider rejected the listed input parameters. You were not charged; fix them and submit a new task with a new X-Request-Id.",
	model.UpstreamUnavailableCode:    model.UpstreamUnavailableMessage,
	"submission_timeout":             "The provider did not confirm this task in time. You were not charged; submit a new task with a new X-Request-Id.",
	model.AsyncGenerationTimeoutCode: model.AsyncGenerationTimeoutMessage,
	// Owner decision 2026-10-13: recovery failed a reservation that was never
	// sent to a provider (a wallet reply was lost) and refunded its hold.
	model.NotStartedCode: "This task was not started because of a temporary platform error. You were not charged; submit a new task with a new X-Request-Id.",
	// Own task queue (spec 2026-10-15): failures of a task never sent.
	model.CancelledCode:                   "You cancelled this task before it was sent to the model. You were not charged.",
	model.QueueTimeoutCode:                "This task waited in the queue too long and was not started. You were not charged; submit it again with a new X-Request-Id.",
	model.ModelUnavailableWhileQueuedCode: "This model became unavailable while the task was waiting in the queue, so it was not started. You were not charged; submit it again with a new X-Request-Id.",
	model.InsufficientBalanceCode:         "Your balance went below zero while this task was waiting in the queue, so it was not started. You were not charged; top up, then submit it again with a new X-Request-Id.",
}

// ErrorMessage is the client-facing sentence for a native error code.
func ErrorMessage(code string) string {
	if message := errorMessages[code]; message != "" {
		return message
	}
	return code
}

// errorType follows the OpenAI-style type used by the other /v1 endpoints.
func errorType(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusConflict:
		return "invalid_request_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusPaymentRequired:
		return "insufficient_quota"
	default:
		return "api_error"
	}
}

// WriteError writes the native error envelope: every error has a code, a
// message and a type.
func WriteError(w http.ResponseWriter, status int, code string) {
	writeErrorEnvelope(w, status, code, ErrorMessage(code))
}

func writeErrorEnvelope(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	writeJSONResponse(w, map[string]any{"error": map[string]string{
		"code": code, "message": message, "type": errorType(status),
	}})
}

// holdRefusalMessage names the hold a refused request asked for (D7).
func holdRefusalMessage(holdMicros int64) string {
	return "This request reserves " + balance.MicrosText(holdMicros) +
		" and your balance does not cover it; see pricing.prepayment in /v1/models."
}

// WriteErrorDetail writes the native error envelope with the parameter at
// fault and, for model errors, suggested_models: listed IDs to use instead
// (an empty list when there is none).
func WriteErrorDetail(
	w http.ResponseWriter,
	status int,
	code, param string,
	suggestedModels []string,
) {
	if suggestedModels == nil {
		suggestedModels = []string{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	writeJSONResponse(w, map[string]any{"error": map[string]any{
		"code": code, "message": ErrorMessage(code), "type": errorType(status),
		"param": param, "suggested_models": suggestedModels,
	}})
}

// NotNativeModelError is ErrNotNativeModel with the listed IDs of the model
// on its own endpoint.
type NotNativeModelError struct{ SuggestedModels []string }

func (e *NotNativeModelError) Error() string { return ErrNotNativeModel.Error() }
func (e *NotNativeModelError) Unwrap() error { return ErrNotNativeModel }

func (h *HTTP) writeError(w http.ResponseWriter, status int, code string) {
	if h != nil && h.OnError != nil {
		h.OnError(status, code, ErrorMessage(code))
	}

	WriteError(w, status, code)
}

// writeErrorMessage is writeError with a message specific to this request.
func (h *HTTP) writeErrorMessage(w http.ResponseWriter, status int, code, message string) {
	if h != nil && h.OnError != nil {
		h.OnError(status, code, message)
	}

	writeErrorEnvelope(w, status, code, message)
}

func (h *HTTP) writeErrorDetail(
	w http.ResponseWriter,
	status int,
	code, param string,
	suggestedModels []string,
) {
	if h != nil && h.OnError != nil {
		h.OnError(status, code, ErrorMessage(code))
	}

	WriteErrorDetail(w, status, code, param, suggestedModels)
}

func (h *HTTP) owner(w http.ResponseWriter, r *http.Request) (string, int, bool) {
	if h == nil || h.Engine == nil || h.Engine.DB == nil || h.Identity == nil {
		h.writeError(w, 503, "native_execution_unavailable")
		return "", 0, false
	}

	group, token, err := h.Identity(r)
	if err != nil || group == "" || token <= 0 {
		h.writeError(w, 401, "authentication_required")
		return "", 0, false
	}

	return group, token, true
}

// WritePublic excludes all routing, upstream identity, credential and raw output
// fields. A pending result never includes output; completed null remains null.
func WritePublic(w http.ResponseWriter, status int, task *model.NativeTask) {
	writePublic(w, status, task, task.Model, 0)
}

// writePublic reports the task under the callable ID (owner decision D2,
// 2026-10-10): the stored task keeps its capability ID. A task waiting in our
// queue also reports its estimated place.
func (h *HTTP) writePublic(w http.ResponseWriter, status int, task *model.NativeTask) {
	publicModel := task.Model
	if h != nil && h.PublicModel != nil {
		if mapped := h.PublicModel(task.Model); mapped != "" {
			publicModel = mapped
		}
	}

	position := 0
	if h != nil && h.Engine != nil {
		position = h.Engine.QueuePosition(task)
	}

	writePublic(w, status, task, publicModel, position)
}

// publicView is the customer view of a task. queuePosition >= 1 adds
// queue_position: an estimate, only while the task waits in our queue; it
// never names the provider, the scope or its limit.
func publicView(task *model.NativeTask, publicModel string, queuePosition int) map[string]any {
	response := map[string]any{
		"id":     task.ID,
		"model":  publicModel,
		"status": PublicStatus(task.Status),
	}
	if queuePosition >= 1 && task.Status == model.NativeStatusWaiting {
		response["queue_position"] = queuePosition
	}

	switch task.Status {
	case "completed":
		response["output"] = json.RawMessage(task.DeliveredOutput)
	case "failed":
		message := failureMessages[task.ErrorCode]
		if message == "" {
			message = "This task failed. Contact support with the task ID."
		}

		failure := map[string]any{"code": task.ErrorCode, "message": message}
		// Rejected fields name only a path and a rule, never provider text.
		if issues := model.NativeTaskIssues(task); len(issues) > 0 {
			failure["issues"] = issues
		}

		response["error"] = failure
	}

	return response
}

func writePublic(
	w http.ResponseWriter,
	status int,
	task *model.NativeTask,
	publicModel string,
	queuePosition int,
) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	writeJSONResponse(w, publicView(task, publicModel, queuePosition))
}

func (h *HTTP) Create(w http.ResponseWriter, r *http.Request) {
	group, token, ok := h.owner(w, r)
	if !ok {
		return
	}

	id := r.Header.Get("X-Request-ID")
	if !taskID.MatchString(id) {
		h.writeError(w, 400, "invalid_request_id")
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, nativeresult.MaxBytes+1))
	if err != nil || len(raw) > nativeresult.MaxBytes {
		h.writeError(w, 400, "invalid_request")
		return
	}
	// Accepted retries use their original frozen route. Reserved retries may resolve
	// again, but ReserveNativeTask rejects any changed contract, route or quote.
	existing, err := model.GetNativeTask(h.Engine.DB, id, group, token)
	if err == nil {
		digest := sha256.Sum256(raw)
		if existing.Fingerprint != hex.EncodeToString(digest[:]) {
			h.writeError(w, 409, "request_id_conflict")
			return
		}

		if existing.Status != "reserved" {
			// A replay only reports stored state, like Get. Recovery workers own
			// billing sync, so a retry loop cannot multiply wallet calls.
			if !h.reads().allowStatus(group, token) {
				h.writeError(w, 429, "rate_limit_exceeded")
				return
			}

			h.writePublic(w, 202, existing)

			return
		}
	}

	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		h.writeError(w, 503, "task_store_unavailable")
		return
	}

	if h.ResolvePlan == nil {
		h.writeError(w, 503, "native_execution_unavailable")
		return
	}

	plan, provider, err := h.ResolvePlan(r, raw)
	if errors.Is(err, ErrNotNativeModel) {
		var (
			suggested []string
			notNative *NotNativeModelError
		)
		if errors.As(err, &notNative) {
			suggested = notNative.SuggestedModels
		}

		h.writeErrorDetail(w, 400, "native_model_unavailable", "model", suggested)

		return
	}

	if err != nil {
		// Route, channel and quote failures are ours, not a wrong model ID.
		h.writeError(w, 503, "model_route_unavailable")
		return
	}
	// Per-request engine copy prevents credentials from leaking across concurrent calls.
	engine := *h.Engine
	engine.Provider = provider

	task, err := engine.Submit(r.Context(), id, group, token, raw, plan)
	if err != nil {
		h.writeCreateFailure(w, id, group, task, err)
		return
	}

	h.writePublic(w, 202, task)
}

// Get only reads stored state. Polling the provider, archiving results and
// billing sync belong to the recovery workers, so a polling loop can never
// trigger provider calls or large transfers.
func (h *HTTP) Get(w http.ResponseWriter, r *http.Request, id string) {
	group, token, ok := h.owner(w, r)
	if !ok {
		return
	}

	if !h.reads().allowStatus(group, token) {
		h.writeError(w, 429, "rate_limit_exceeded")
		return
	}

	task, err := model.GetNativeTask(h.Engine.DB, id, group, token)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		h.writeError(w, 404, "task_not_found")
		return
	}

	if err != nil {
		h.writeError(w, 503, "task_temporarily_unavailable")
		return
	}

	h.writePublic(w, 200, task)
}

// Cancel ends a task still waiting in our queue (spec §6.2): waiting ->
// failed (cancelled), a compare-and-set that a dispatch claim wins or loses,
// then one billing sync (settle not_dispatched). A refund that fails here is
// retried by recovery. It never reads the wallet first and never cancels work
// already sent: that answers 409 task_not_cancellable with the task's status.
func (h *HTTP) Cancel(w http.ResponseWriter, r *http.Request, id string) {
	group, token, ok := h.owner(w, r)
	if !ok {
		return
	}

	if !h.reads().allowStatus(group, token) {
		h.writeError(w, 429, "rate_limit_exceeded")
		return
	}

	task, err := model.GetNativeTask(h.Engine.DB, id, group, token)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// Never the submit-time task_not_found hint: resending the POST would
		// create, and charge, the task the customer meant to cancel.
		h.writeErrorMessage(w, 404, "task_not_found", cancelNotFoundMessage)
		return
	}

	if err != nil {
		h.writeError(w, 503, "task_temporarily_unavailable")
		return
	}

	if task.Status == model.NativeStatusWaiting {
		cancelled, cancelErr := model.CancelNativeTask(h.Engine.DB, id, group, token)
		if cancelErr != nil {
			h.writeError(w, 503, "task_temporarily_unavailable")
			return
		}

		if cancelled {
			wakeDispatcher()

			if failed, readErr := model.GetNativeTask(
				h.Engine.DB,
				id,
				group,
				token,
			); readErr == nil {
				ctx, cancel := context.WithTimeout(
					context.WithoutCancel(r.Context()),
					submitWalletBudget,
				)
				if syncErr := h.Engine.SyncBilling(ctx, failed); syncErr != nil {
					log.WithFields(log.Fields{"lane": "native", "task_id": id, "group": group}).
						WithError(syncErr).
						Warn("cancelled native task refund deferred to recovery")
				}

				cancel()
			}
		}

		if task, err = model.GetNativeTask(h.Engine.DB, id, group, token); err != nil {
			h.writeError(w, 503, "task_temporarily_unavailable")
			return
		}
	}

	if task.Status == "failed" && task.ErrorCode == model.CancelledCode {
		h.writePublic(w, 200, task)
		return
	}
	// Reserved for under a second, already claimed, or sent: not cancellable.
	if h.OnError != nil {
		h.OnError(409, "task_not_cancellable", ErrorMessage("task_not_cancellable"))
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusConflict)
	writeJSONResponse(w, map[string]any{"error": map[string]string{
		"code":    "task_not_cancellable",
		"message": ErrorMessage("task_not_cancellable"),
		"type":    errorType(409),
	}, "status": PublicStatus(task.Status)})
}

// Artifacts up to this size are verified in full before the first byte is sent.
// Larger ones stream, holding back the tail until the digest matches. Both
// apply only to files archived before chunk digests were recorded.
const (
	bufferedArtifactLimit = 8 << 20
	artifactTailHoldback  = 64 << 10
)

// chunkBuffers holds one verified chunk per download in flight.
var chunkBuffers = sync.Pool{
	New: func() any { buffer := make([]byte, ownedartifact.ChunkSize); return &buffer },
}

func (h *HTTP) GetArtifact(w http.ResponseWriter, r *http.Request, id, indexText string) {
	group, token, ok := h.owner(w, r)
	if !ok {
		return
	}

	index, err := strconv.Atoi(indexText)
	if err != nil || index < 0 || index >= 1024 {
		h.writeError(w, 404, "artifact_not_found")
		return
	}

	task, err := model.GetNativeTask(h.Engine.DB, id, group, token)
	if err != nil || task.Status != "completed" {
		h.writeError(w, 404, "artifact_not_found")
		return
	}

	entries := map[int]ownedartifact.Receipt{}
	if json.Unmarshal([]byte(task.ArtifactManifest), &entries) != nil ||
		!ownedartifact.ValidReceipt(id, index, entries[index]) {
		h.writeError(w, 404, "artifact_not_found")
		return
	}

	h.ServeOwnedArtifact(w, r, group, token, id, index, entries[index])
}

// ServeOwnedArtifact shares native bounded/range-verified downloads with an
// already-authorized internal execution. The caller must prove task ownership;
// this method independently validates the storage receipt and enforces limits.
func (h *HTTP) ServeOwnedArtifact(
	w http.ResponseWriter,
	r *http.Request,
	group string,
	token int,
	id string,
	index int,
	receipt ownedartifact.Receipt,
) {
	if !ownedartifact.ValidReceipt(id, index, receipt) {
		h.writeError(w, 404, "artifact_not_found")
		return
	}

	var err error

	file := artifactFile{name: ownedartifact.FileName(id, index, receipt.Type), receipt: receipt}

	from, to, ranged, satisfiable := artifactRange(r, receipt)
	if !satisfiable {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(receipt.Size, 10))
		w.Header().Set("Cache-Control", "private, no-store")
		h.writeError(w, 416, "range_not_satisfiable")
		return
	}

	release, ok := h.reads().acquireDownload(group, token)
	if !ok {
		h.writeError(w, 429, "rate_limit_exceeded")
		return
	}
	defer release()
	// A slow reader must not hold a download slot indefinitely: the whole
	// transfer, including the storage read, ends at this deadline. The server
	// has no write timeout, so the deadline is cleared again for keep-alive reuse.
	deadline := time.Now().Add(artifactTransferTime(to - from + 1))
	controller := http.NewResponseController(w)

	_ = controller.SetWriteDeadline(deadline)
	defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()

	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()

	r = r.WithContext(ctx)

	w = &artifactWriter{ResponseWriter: w, controller: controller, deadline: deadline}
	if receipt.Chunks != "" {
		h.serveChunked(w, r, file, from, to, ranged)
		return
	}

	var source *http.Response
	if h.Download != nil {
		source, err = h.Download(r, receipt.Key)
	} else {
		source, err = ownedartifact.Download(r.Context(), receipt.Key)
	}

	if err != nil || source == nil {
		h.writeError(w, 503, "artifact_unavailable")
		return
	}

	if source.Body == nil {
		h.writeError(w, 503, "artifact_unavailable")
		return
	}
	defer source.Body.Close()
	// A declared length that disagrees with the receipt fails before any byte is sent.
	if source.StatusCode != http.StatusOK || source.ContentLength != receipt.Size {
		h.writeError(w, 503, "artifact_integrity_failed")
		return
	}

	if receipt.Size > bufferedArtifactLimit {
		streamArtifact(w, source.Body, file)
		return
	}
	// Reverify immutable receipt before emitting even one byte to the customer.
	raw, err := io.ReadAll(io.LimitReader(source.Body, receipt.Size+1))

	sum := sha256.Sum256(raw)
	if err != nil || int64(len(raw)) != receipt.Size ||
		hex.EncodeToString(sum[:]) != receipt.SHA256 {
		h.writeError(w, 503, "artifact_integrity_failed")
		return
	}

	writeArtifactHeaders(w, file, 200, 0, receipt.Size-1)
	_, _ = w.Write(raw)
}

// Both idle writers and the total size-based budget release download slots.
type artifactWriter struct {
	http.ResponseWriter
	controller *http.ResponseController
	deadline   time.Time
}

func (w *artifactWriter) Write(p []byte) (int, error) {
	deadline := time.Now().Add(60 * time.Second)
	if w.deadline.Before(deadline) {
		deadline = w.deadline
	}

	_ = w.controller.SetWriteDeadline(deadline)

	return w.ResponseWriter.Write(p)
}

type artifactFile struct {
	name    string
	receipt ownedartifact.Receipt
}

// artifactRange reads a single "bytes=" range. Only files with chunk digests
// serve ranges; anything else (no header, several ranges, If-Range, a
// malformed value) is the whole file, as HTTP allows. satisfiable is false
// only for a well-formed range that starts beyond the end of the file.
func artifactRange(
	r *http.Request,
	receipt ownedartifact.Receipt,
) (from, to int64, ranged, satisfiable bool) {
	size := receipt.Size
	whole := func() (int64, int64, bool, bool) { return 0, size - 1, false, true }

	header := strings.TrimSpace(r.Header.Get("Range"))
	if receipt.Chunks == "" || size <= 0 || header == "" || r.Header.Get("If-Range") != "" {
		return whole()
	}

	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return whole()
	}

	first, last, ok := strings.Cut(strings.TrimSpace(spec), "-")
	if !ok {
		return whole()
	}

	number := func(text string) (int64, bool) {
		if text == "" || len(text) > 18 || strings.Trim(text, "0123456789") != "" {
			return 0, false
		}

		value, err := strconv.ParseInt(text, 10, 64)

		return value, err == nil
	}
	if first == "" {
		suffix, ok := number(last)
		if !ok {
			return whole()
		}

		if suffix == 0 {
			return 0, 0, true, false
		}

		return max(size-suffix, 0), size - 1, true, true
	}

	from, ok = number(first)
	if !ok {
		return whole()
	}

	to = size - 1
	if last != "" {
		if to, ok = number(last); !ok || to < from {
			return whole()
		}

		to = min(to, size-1)
	}

	if from >= size {
		return 0, 0, true, false
	}

	return from, to, true, true
}

// serveChunked streams bytes from..to, reading whole chunks from storage and
// writing only bytes of a chunk whose digest matches the receipt. The first
// chunk is checked before the headers, so a corrupted file answers 503; a
// later mismatch ends the response short of its Content-Length.
func (h *HTTP) serveChunked(
	w http.ResponseWriter,
	r *http.Request,
	file artifactFile,
	from, to int64,
	ranged bool,
) {
	receipt := file.receipt
	firstChunk, lastChunk := from/ownedartifact.ChunkSize, to/ownedartifact.ChunkSize
	start, end := firstChunk*ownedartifact.ChunkSize, min(
		(lastChunk+1)*ownedartifact.ChunkSize,
		receipt.Size,
	)-1

	var (
		source *http.Response
		err    error
	)
	switch {
	case start == 0 && end == receipt.Size-1 && h.Download != nil:
		source, err = h.Download(r, receipt.Key)
	case start == 0 && end == receipt.Size-1:
		source, err = ownedartifact.Download(r.Context(), receipt.Key)
	case h.DownloadRange != nil:
		source, err = h.DownloadRange(r, receipt.Key, start, end)
	default:
		source, err = ownedartifact.DownloadRange(r.Context(), receipt.Key, start, end)
	}

	if err != nil || source == nil || source.Body == nil {
		h.writeError(w, 503, "artifact_unavailable")
		return
	}

	defer source.Body.Close()

	validHeaders := source.ContentLength == end-start+1
	if start == 0 && end == receipt.Size-1 {
		validHeaders = validHeaders && source.StatusCode == http.StatusOK
	} else {
		validHeaders = validHeaders && source.StatusCode == http.StatusPartialContent &&
			source.Header.Get(
				"Content-Range",
			) == fmt.Sprintf(
				"bytes %d-%d/%d",
				start,
				end,
				receipt.Size,
			)
	}

	if !validHeaders {
		h.writeError(w, 503, "artifact_integrity_failed")
		return
	}

	pooled, ok := chunkBuffers.Get().(*[]byte)
	if !ok || pooled == nil {
		buffer := make([]byte, ownedartifact.ChunkSize)
		pooled = &buffer
	}
	defer chunkBuffers.Put(pooled)

	buffer := *pooled

	status := 200
	if ranged {
		status = 206
	}

	for chunk := firstChunk; chunk <= lastChunk; chunk++ {
		chunkStart := chunk * ownedartifact.ChunkSize
		chunkEnd := min(chunkStart+ownedartifact.ChunkSize, receipt.Size)
		piece := buffer[:chunkEnd-chunkStart]
		_, readErr := io.ReadFull(source.Body, piece)
		sum := sha256.Sum256(piece)

		verified := readErr == nil &&
			hex.EncodeToString(sum[:]) == receipt.Chunks[chunk*64:(chunk+1)*64]
		if chunk == lastChunk && verified {
			var extra [1]byte

			n, err := source.Body.Read(extra[:])
			verified = n == 0 && err == io.EOF
		}

		if chunk == firstChunk {
			if !verified {
				h.writeError(w, 503, "artifact_integrity_failed")
				return
			}

			writeArtifactHeaders(w, file, status, from, to)
		} else if !verified {
			return
		}

		if _, err := w.Write(
			piece[max(from, chunkStart)-chunkStart : min(to+1, chunkEnd)-chunkStart],
		); err != nil {
			return
		}
	}
}

// artifactTransferTime allows 30 seconds plus one second per 128 KiB, so a
// client reading at least that fast always finishes.
var artifactTransferTime = func(size int64) time.Duration {
	return 30*time.Second + time.Duration(size/(128<<10))*time.Second
}

// writeArtifactHeaders names the file after the task and gives it the type
// read from its bytes when it was archived. Every file stays a download in a
// sandbox: only the media types ownedartifact recognises are ever named.
func writeArtifactHeaders(w http.ResponseWriter, file artifactFile, status int, from, to int64) {
	w.Header().Set("Content-Type", ownedartifact.ContentType(file.receipt.Type))
	w.Header().Set("Content-Disposition", `attachment; filename="`+file.name+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("Cache-Control", "private, no-store")

	if file.receipt.Chunks != "" {
		w.Header().Set("Accept-Ranges", "bytes")
	}

	if status == 206 {
		w.Header().
			Set("Content-Range", "bytes "+strconv.FormatInt(from, 10)+"-"+strconv.FormatInt(to, 10)+"/"+strconv.FormatInt(file.receipt.Size, 10))
	}

	w.Header().Set("Content-Length", strconv.FormatInt(to-from+1, 10))
	w.WriteHeader(status)
}

// streamArtifact keeps memory bounded for large artifacts. The last
// artifactTailHoldback bytes are written only after the size and digest match,
// so a corrupted object is never delivered complete: the response falls short
// of its Content-Length and the server drops the connection.
func streamArtifact(w http.ResponseWriter, body io.Reader, file artifactFile) {
	receipt := file.receipt
	writeArtifactHeaders(w, file, 200, 0, receipt.Size-1)

	digest := sha256.New()
	reader := io.LimitReader(body, receipt.Size+1)
	held := make([]byte, 0, artifactTailHoldback+32<<10)
	chunk := make([]byte, 32<<10)

	var read int64
	for {
		n, err := reader.Read(chunk)
		if n > 0 {
			read += int64(n)
			digest.Write(chunk[:n])

			held = append(held, chunk[:n]...)
			if over := len(held) - artifactTailHoldback; over > 0 {
				if _, werr := w.Write(held[:over]); werr != nil {
					return
				}

				held = append(held[:0], held[over:]...)
			}
		}

		if err == io.EOF {
			break
		}

		if err != nil {
			return
		}
	}

	if read != receipt.Size || hex.EncodeToString(digest.Sum(nil)) != receipt.SHA256 {
		return
	}

	_, _ = w.Write(held)
}

// writeJSONResponse records an encoding or client-write failure after headers were sent.
func writeJSONResponse(w http.ResponseWriter, value any) {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.WithError(err).Warn("native response write failed")
	}
}

func (h *HTTP) writeCreateFailure(
	w http.ResponseWriter,
	id, group string,
	task *model.NativeTask,
	err error,
) {
	if task != nil && !WalletRefusal(err) && h.OnReserved != nil {
		h.OnReserved(task.BillingOperationID)
	}

	switch {
	case errors.Is(err, model.ErrNativeTaskConflict) && (task == nil || task.Status == "reserved"):
		h.writeError(w, 409, "request_id_conflict")
	case errors.Is(err, nativeresult.ErrRequest), errors.Is(err, nativeresult.ErrInvalid):
		h.writeError(w, 400, "invalid_input")
	case errors.Is(err, balance.ErrPrepaymentInsufficientBalance):
		var refusal *HoldRefusal
		if errors.As(err, &refusal) && refusal.HoldMicros > 0 {
			h.writeErrorMessage(
				w,
				402,
				"insufficient_balance",
				holdRefusalMessage(refusal.HoldMicros),
			)
		} else {
			h.writeError(w, 402, "insufficient_balance")
		}
	case errors.Is(err, balance.ErrPrepaymentTooManyActiveTasks):
		var limit *balance.TaskLimitError
		if errors.As(err, &limit) && limit.Waiting {
			h.writeErrorMessage(w, 429, "too_many_active_tasks", queueFullMessage)
		} else {
			h.writeError(w, 429, "too_many_active_tasks")
		}
	case errors.Is(err, balance.ErrPrepaymentModelPaused):
		h.writeError(w, 503, "model_unavailable")
	default:
		// Never swallow this error: a pending or wallet-blocked task looks
		// queued to the customer, so the log is the only trace of why.
		// A conflict after the reserved->submitting transition lands here
		// too (plan C9): another writer owns the submission, which may
		// already be accepted and charged, so the request answers the
		// stored state and keeps its one 202 row.
		fields := log.Fields{
			"lane":    "native",
			"task_id": id,
			"group":   group,
			"error":   err.Error(),
		}
		if task != nil {
			fields["task_status"] = task.Status
			fields["model"] = task.Model
		}

		if task != nil && task.Status != "reserved" {
			log.WithFields(fields).
				Warn("native task create returned stored state after an error")
			h.writePublic(w, 202, task)
		} else {
			log.WithFields(fields).Error("native task create unavailable")
			h.writeError(w, 503, "native_execution_unavailable")
		}
	}
}
