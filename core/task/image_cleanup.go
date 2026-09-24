package task

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/labring/aiproxy/core/common/ownedimage"
	"github.com/labring/aiproxy/core/model"
	log "github.com/sirupsen/logrus"
)

func temporaryImageOutput(taskID string, index int, output model.ImageOutput) bool {
	extension := ""
	switch output.ContentType {
	case "image/png":
		extension = "png"
	case "image/jpeg":
		extension = "jpg"
	case "image/webp":
		extension = "webp"
	default:
		return false
	}
	parsed, err := url.Parse(output.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return false
	}
	return output.Stored && strings.HasSuffix(parsed.Path, fmt.Sprintf("/generated-results/images/%s/%d.%s", taskID, index, extension))
}

func cleanupExpiredImageTask(ctx context.Context, task *model.ImageTask, deleteImage func(context.Context, string, int, string) error) (bool, error) {
	if task.ResultExpiresAt == nil || time.Now().Before(*task.ResultExpiresAt) {
		return false, nil
	}
	outputs := append([]model.ImageOutput(nil), task.Data...)
	changed := false
	for index, output := range outputs {
		if !temporaryImageOutput(task.ID, index, output) {
			continue
		}
		if err := deleteImage(ctx, task.ID, index, output.ContentType); err != nil {
			return false, err
		}
		outputs[index].URL = ""
		outputs[index].Stored = false
		outputs[index].URLExpiresAt = nil
		changed = true
	}
	if !changed {
		return false, nil
	}
	return true, model.MarkExpiredImageMediaDeleted(task, outputs, time.Now().UTC())
}

func ImageResultCleanupTask(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for {
		cleanExpiredImageResults(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func cleanExpiredImageResults(ctx context.Context) {
	const batchSize = 50
	afterID := ""
	for scanned := 0; scanned < 1000; scanned += batchSize {
		if ctx.Err() != nil {
			return
		}
		tasks, err := model.ListExpiredTemporaryImageTasks(time.Now(), afterID, batchSize)
		if err != nil {
			log.WithError(err).Warn("list expired temporary image results")
			return
		}
		for index := range tasks {
			deleteCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_, err := cleanupExpiredImageTask(deleteCtx, &tasks[index], ownedimage.DeleteArchivedImage)
			cancel()
			if err != nil {
				log.WithError(err).WithField("task_id", tasks[index].ID).Warn("delete expired image result")
			}
		}
		if len(tasks) < batchSize {
			return
		}
		afterID = tasks[len(tasks)-1].ID
	}
}
