package controller

import (
	"context"
	"encoding/json"
	"github.com/labring/aiproxy/core/common/ownedimage"
	"github.com/labring/aiproxy/core/model"
	"time"
)

func fillImageMetadata(ctx context.Context, outputs []model.ImageOutput, read func(context.Context, string) (ownedimage.Metadata, error)) bool {
	changed := false
	for i := range outputs {
		item := &outputs[i]
		if item.Width != nil && *item.Width > 0 && item.Height != nil && *item.Height > 0 && item.ContentType != "" {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		metadata, err := read(ctx, item.URL)
		if err != nil || metadata.Width <= 0 || metadata.Height <= 0 {
			continue
		}
		w, h := int64(metadata.Width), int64(metadata.Height)
		item.Width, item.Height, item.ContentType = &w, &h, metadata.ContentType
		changed = true
	}
	return changed
}

// Read-only enrichment of an already completed task. Failure to read optional
// metadata never hides a generated image or retries generation/settlement.
func enrichCompletedImageTask(ctx context.Context, task *model.ImageTask) {
	if task.Status != "completed" || len(task.Data) == 0 || (task.ResultExpiresAt != nil && !time.Now().Before(*task.ResultExpiresAt)) {
		return
	}
	before, err := json.Marshal(task.Data)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	if !fillImageMetadata(ctx, task.Data, ownedimage.ReadMetadata) {
		return
	}
	after, err := json.Marshal(task.Data)
	if err != nil {
		return
	}
	// Terminal data URLs are immutable. CAS avoids overwriting concurrent enrichment.
	_ = model.LogDB.Model(&model.ImageTask{}).Where("id = ? AND group_id = ? AND token_id = ? AND status = ? AND data = ?", task.ID, task.GroupID, task.TokenID, "completed", string(before)).UpdateColumn("data", string(after)).Error
}
