package nativetask

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/model"
)

const ImageDeliveryLimit = 20 << 20

// SingleOwnedImage fails closed on a multi-image result or mismatched manifest.
// A native output is never truncated into an apparently successful n=1 response.
func SingleOwnedImage(task *model.NativeTask) (ownedartifact.Receipt, bool) {
	if task == nil || task.Status != "completed" {
		return ownedartifact.Receipt{}, false
	}

	var (
		output struct {
			Images []json.RawMessage `json:"images"`
		}
		entries map[int]ownedartifact.Receipt
	)
	if json.Unmarshal([]byte(task.DeliveredOutput), &output) != nil || len(output.Images) != 1 ||
		json.Unmarshal([]byte(task.ArtifactManifest), &entries) != nil || len(entries) != 1 {
		return ownedartifact.Receipt{}, false
	}

	receipt := entries[0]
	if !ownedartifact.ValidReceipt(task.ID, 0, receipt) || receipt.Size > ImageDeliveryLimit ||
		(receipt.Type != "image/png" && receipt.Type != "image/jpeg" && receipt.Type != "image/webp") {
		return ownedartifact.Receipt{}, false
	}

	return receipt, true
}

// ReadOwnedImage reuses the native download limiter and archive transport, then
// verifies all bytes before returning base64. The caller must prove ownership.
func (h *HTTP) ReadOwnedImage(r *http.Request, task *model.NativeTask) (string, error) {
	receipt, ok := SingleOwnedImage(task)
	if !ok {
		return "", errors.New("image result unavailable")
	}

	release, ok := h.reads().acquireDownload(task.GroupID, task.TokenID)
	if !ok {
		return "", errors.New("image download busy")
	}
	defer release()

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	request := r.WithContext(ctx)

	var (
		source *http.Response
		err    error
	)
	if h.Download != nil {
		source, err = h.Download(request, receipt.Key)
	} else {
		source, err = ownedartifact.Download(ctx, receipt.Key)
	}

	if source != nil && source.Body != nil {
		defer source.Body.Close()
	}

	if err != nil || source == nil || source.Body == nil || source.StatusCode != http.StatusOK ||
		source.ContentLength != receipt.Size {
		return "", errors.New("image archive unavailable")
	}

	raw, err := io.ReadAll(io.LimitReader(source.Body, receipt.Size+1))

	sum := sha256.Sum256(raw)
	if err != nil || int64(len(raw)) != receipt.Size ||
		hex.EncodeToString(sum[:]) != receipt.SHA256 {
		return "", errors.New("image archive integrity failed")
	}

	return base64.StdEncoding.EncodeToString(raw), nil
}

// WaitImage reads only persisted task state. The existing recovery workers own
// polling, archiving and settlement. Expiry or client cancellation returns the
// latest state and never changes billing or submits upstream again.
func (h *HTTP) WaitImage(
	ctx context.Context,
	task *model.NativeTask,
	until time.Time,
) (*model.NativeTask, error) {
	timer := time.NewTimer(time.Until(until))
	defer timer.Stop()

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	for {
		if task.Status == "completed" || task.Status == "failed" {
			return task, nil
		}

		select {
		case <-ctx.Done():
			return task, nil
		case <-timer.C:
			return task, nil
		case <-ticker.C:
			next, err := model.GetNativeTask(h.Engine.DB, task.ID, task.GroupID, task.TokenID)
			if err != nil {
				return task, err
			}

			task = next
		}
	}
}
