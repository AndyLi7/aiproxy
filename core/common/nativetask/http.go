package nativetask

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/model"
	"gorm.io/gorm"
	"io"
	"net/http"
	"strconv"
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
}

func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": code}})
}
func (h *HTTP) owner(w http.ResponseWriter, r *http.Request) (string, int, bool) {
	if h == nil || h.Engine == nil || h.Engine.DB == nil || h.Identity == nil {
		writeError(w, 503, "native_execution_unavailable")
		return "", 0, false
	}
	group, token, err := h.Identity(r)
	if err != nil || group == "" || token <= 0 {
		writeError(w, 401, "authentication_required")
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
		response["error"] = map[string]string{"code": task.ErrorCode}
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
		writeError(w, 400, "invalid_request_id")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, nativeresult.MaxBytes+1))
	if err != nil || len(raw) > nativeresult.MaxBytes {
		writeError(w, 400, "invalid_request")
		return
	}
	// Accepted retries use their original frozen route. Reserved retries may resolve
	// again, but ReserveNativeTask rejects any changed contract, route or quote.
	existing, err := model.GetNativeTask(h.Engine.DB, id, group, token)
	if err == nil {
		digest := sha256.Sum256(raw)
		if existing.Fingerprint != hex.EncodeToString(digest[:]) {
			writeError(w, 409, "request_id_conflict")
			return
		}
		if existing.Status != "reserved" {
			if err = h.Engine.SyncBilling(r.Context(), existing); err != nil {
				writeError(w, 503, "billing_status_unavailable")
				return
			}
			WritePublic(w, 202, existing)
			return
		}
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		writeError(w, 503, "task_store_unavailable")
		return
	}
	if h.ResolvePlan == nil {
		writeError(w, 503, "native_execution_unavailable")
		return
	}
	plan, provider, err := h.ResolvePlan(r, raw)
	if err != nil {
		writeError(w, 400, "native_model_unavailable")
		return
	}
	// Per-request engine copy prevents credentials from leaking across concurrent calls.
	engine := *h.Engine
	engine.Provider = provider
	task, err := engine.Submit(r.Context(), id, group, token, raw, plan)
	if err != nil {
		switch {
		case errors.Is(err, model.ErrNativeTaskConflict):
			writeError(w, 409, "request_id_conflict")
		case errors.Is(err, nativeresult.ErrRequest), errors.Is(err, nativeresult.ErrInvalid):
			writeError(w, 400, "invalid_input")
		case errors.Is(err, balance.ErrPrepaymentInsufficientBalance):
			writeError(w, 402, "insufficient_balance")
		default:
			if task != nil && task.Status != "reserved" {
				WritePublic(w, 202, task)
			} else {
				writeError(w, 503, "native_execution_unavailable")
			}
		}
		return
	}
	WritePublic(w, 202, task)
}
func (h *HTTP) Get(w http.ResponseWriter, r *http.Request, id string) {
	group, token, ok := h.owner(w, r)
	if !ok {
		return
	}
	task, err := h.Engine.Poll(r.Context(), id, group, token, h.ResolvePoller, h.Archive)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		writeError(w, 404, "task_not_found")
		return
	}
	if err != nil {
		writeError(w, 503, "task_temporarily_unavailable")
		return
	}
	WritePublic(w, 200, task)
}
func (h *HTTP) GetArtifact(w http.ResponseWriter, r *http.Request, id, indexText string) {
	group, token, ok := h.owner(w, r)
	if !ok {
		return
	}
	index, err := strconv.Atoi(indexText)
	if err != nil || index < 0 || index >= 1024 {
		writeError(w, 404, "artifact_not_found")
		return
	}
	task, err := model.GetNativeTask(h.Engine.DB, id, group, token)
	if err != nil || task.Status != "completed" {
		writeError(w, 404, "artifact_not_found")
		return
	}
	entries := map[int]ownedartifact.Receipt{}
	if json.Unmarshal([]byte(task.ArtifactManifest), &entries) != nil || !ownedartifact.ValidReceipt(id, index, entries[index]) {
		writeError(w, 404, "artifact_not_found")
		return
	}
	receipt := entries[index]
	var source *http.Response
	if h.Download != nil {
		source, err = h.Download(r, receipt.Key)
	} else {
		source, err = ownedartifact.Download(r.Context(), receipt.Key)
	}
	if err != nil || source == nil {
		writeError(w, 503, "artifact_unavailable")
		return
	}
	if source.Body == nil {
		writeError(w, 503, "artifact_unavailable")
		return
	}
	defer source.Body.Close()
	// Reverify immutable receipt before emitting even one byte to the customer.
	raw, err := io.ReadAll(io.LimitReader(source.Body, receipt.Size+1))
	sum := sha256.Sum256(raw)
	if source.StatusCode != 200 || err != nil || int64(len(raw)) != receipt.Size || hex.EncodeToString(sum[:]) != receipt.SHA256 {
		writeError(w, 503, "artifact_integrity_failed")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="artifact.bin"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(200)
	_, _ = w.Write(raw)
}
