package nativetask

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/model"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
	"io"
	"net/http"
	"strconv"
	"time"
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
	// OnError lets the gateway log the same code and message the client receives.
	OnError func(status int, code, message string)
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
	"native_model_unavailable":     "This model is not available for tasks on this endpoint. Check the model ID in the model catalog.",
	"model_route_unavailable":      "This model is temporarily unavailable. Retry later with the same X-Request-Id.",
	"authentication_required":      "A valid API key is required.",
	"insufficient_balance":         "Your account balance is insufficient.",
	"too_many_active_tasks":        "Too many of your tasks are generating at once. Wait for one to finish, then retry with the same X-Request-Id.",
	"rate_limit_exceeded":          "Too many requests for this API key. Poll each task about every 5 seconds and download results a few at a time.",
	"model_unavailable":            "This model is temporarily unavailable. Retry later with the same X-Request-Id.",
	"task_store_unavailable":       "Task storage is temporarily unavailable. Retry querying the same task before submitting another task.",
	"task_temporarily_unavailable": "This task is temporarily unavailable. Retry querying the same task later.",
	"native_execution_unavailable": "Task execution is temporarily unavailable. Retry later with the same request ID.",
	"artifact_unavailable":         "The task result is temporarily unavailable. Retry downloading it later.",
	"artifact_integrity_failed":    "The task result could not be verified. Contact support with the task ID.",
	"billing_status_unavailable":   "Billing status is temporarily unavailable. Retry querying the same task later.",
}

// failureMessages explain the error code of a task that ended as failed.
// Every listed failure is refunded in full by SyncBilling or recovery.
var failureMessages = map[string]string{
	"upstream_task_failed":           "The provider could not generate this result. You were not charged; submit a new task with a new X-Request-Id.",
	"upstream_result_rejected":       "The provider returned a result we could not deliver. You were not charged; submit a new task with a new X-Request-Id.",
	"upstream_rejected":              "The provider rejected this request before generating. You were not charged.",
	model.UpstreamUnavailableCode:    model.UpstreamUnavailableMessage,
	"submission_timeout":             "The provider did not confirm this task in time. You were not charged; submit a new task with a new X-Request-Id.",
	model.AsyncGenerationTimeoutCode: model.AsyncGenerationTimeoutMessage,
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
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
		"code": code, "message": ErrorMessage(code), "type": errorType(status),
	}})
}
func (h *HTTP) writeError(w http.ResponseWriter, status int, code string) {
	if h != nil && h.OnError != nil {
		h.OnError(status, code, ErrorMessage(code))
	}
	WriteError(w, status, code)
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
	response := map[string]any{"id": task.ID, "model": task.Model, "status": task.Status}
	switch task.Status {
	case "reserved", "submitting", "submission_unknown":
		response["status"] = "queued"
	case "result_received", "delivery_ready":
		response["status"] = "running"
	case "completed":
		response["output"] = json.RawMessage(task.DeliveredOutput)
	case "failed":
		message := failureMessages[task.ErrorCode]
		if message == "" {
			message = "This task failed. Contact support with the task ID."
		}
		response["error"] = map[string]string{"code": task.ErrorCode, "message": message}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}
func (h *HTTP) Create(w http.ResponseWriter, r *http.Request) {
	group, token, ok := h.owner(w, r)
	if !ok {
		return
	}
	id := r.Header.Get("X-Request-Id")
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
			WritePublic(w, 202, existing)
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
		h.writeError(w, 400, "native_model_unavailable")
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
		switch {
		case errors.Is(err, model.ErrNativeTaskConflict):
			h.writeError(w, 409, "request_id_conflict")
		case errors.Is(err, nativeresult.ErrRequest), errors.Is(err, nativeresult.ErrInvalid):
			h.writeError(w, 400, "invalid_input")
		case errors.Is(err, balance.ErrPrepaymentInsufficientBalance):
			h.writeError(w, 402, "insufficient_balance")
		case errors.Is(err, balance.ErrPrepaymentTooManyActiveTasks):
			h.writeError(w, 429, "too_many_active_tasks")
		case errors.Is(err, balance.ErrPrepaymentModelPaused):
			h.writeError(w, 503, "model_unavailable")
		default:
			// Never swallow this error: a pending or wallet-blocked task looks
			// queued to the customer, so the log is the only trace of why.
			fields := log.Fields{"lane": "native", "task_id": id, "group": group, "error": err.Error()}
			if task != nil {
				fields["task_status"] = task.Status
				fields["model"] = task.Model
			}
			if task != nil && task.Status != "reserved" {
				log.WithFields(fields).Warn("native task create returned stored state after an error")
				WritePublic(w, 202, task)
			} else {
				log.WithFields(fields).Error("native task create unavailable")
				h.writeError(w, 503, "native_execution_unavailable")
			}
		}
		return
	}
	WritePublic(w, 202, task)
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
	WritePublic(w, 200, task)
}

// Artifacts up to this size are verified in full before the first byte is sent.
// Larger ones stream, holding back the tail until the digest matches.
const (
	bufferedArtifactLimit = 8 << 20
	artifactTailHoldback  = 64 << 10
)

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
	if json.Unmarshal([]byte(task.ArtifactManifest), &entries) != nil || !ownedartifact.ValidReceipt(id, index, entries[index]) {
		h.writeError(w, 404, "artifact_not_found")
		return
	}
	release, ok := h.reads().acquireDownload(group, token)
	if !ok {
		h.writeError(w, 429, "rate_limit_exceeded")
		return
	}
	defer release()
	receipt := entries[index]
	// A slow reader must not hold a download slot indefinitely: the whole
	// transfer, including the storage read, ends at this deadline. The server
	// has no write timeout, so the deadline is cleared again for keep-alive reuse.
	deadline := time.Now().Add(artifactTransferTime(receipt.Size))
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(deadline)
	defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	r = r.WithContext(ctx)
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
	if source.StatusCode != 200 || (source.ContentLength > 0 && source.ContentLength != receipt.Size) {
		h.writeError(w, 503, "artifact_integrity_failed")
		return
	}
	if receipt.Size > bufferedArtifactLimit {
		streamArtifact(w, source.Body, receipt)
		return
	}
	// Reverify immutable receipt before emitting even one byte to the customer.
	raw, err := io.ReadAll(io.LimitReader(source.Body, receipt.Size+1))
	sum := sha256.Sum256(raw)
	if err != nil || int64(len(raw)) != receipt.Size || hex.EncodeToString(sum[:]) != receipt.SHA256 {
		h.writeError(w, 503, "artifact_integrity_failed")
		return
	}
	writeArtifactHeaders(w, receipt.Size)
	_, _ = w.Write(raw)
}

// artifactTransferTime allows 30 seconds plus one second per 128 KiB, so a
// client reading at least that fast always finishes.
var artifactTransferTime = func(size int64) time.Duration {
	return 30*time.Second + time.Duration(size/(128<<10))*time.Second
}

func writeArtifactHeaders(w http.ResponseWriter, size int64) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="artifact.bin"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(200)
}

// streamArtifact keeps memory bounded for large artifacts. The last
// artifactTailHoldback bytes are written only after the size and digest match,
// so a corrupted object is never delivered complete: the response falls short
// of its Content-Length and the server drops the connection.
func streamArtifact(w http.ResponseWriter, body io.Reader, receipt ownedartifact.Receipt) {
	writeArtifactHeaders(w, receipt.Size)
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
