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
	return temporaryImageAsset(taskID, index, "", output)
}
func temporaryImageAsset(taskID string, index int, name string, output model.ImageOutput) bool {
	if name != "" && !model.ValidAuxiliaryImageName(name) {
		return false
	}
	suffix := ""
	if name != "" {
		suffix = "-" + name
	}
	extension := ""
	switch output.ContentType {
	case "image/png":
		extension = "png"
	case "image/jpeg":
		extension = "jpg"
	case "image/webp":
		extension = "webp"
	case "image/gif":
		extension = "gif"
	default:
		return false
	}
	parsed, err := url.Parse(output.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || !output.Stored {
		return false
	}
	stem := strings.TrimSuffix(parsed.Path, "."+extension)
	if stem == parsed.Path {
		return false
	}
	// Results stored since 2026-10-04 end with an unguessable signature segment.
	if signed := len(stem) - 33; signed > 0 && stem[signed] == '-' && lowerHex(stem[signed+1:]) {
		stem = stem[:signed]
	}
	return strings.HasSuffix(stem, fmt.Sprintf("/generated-results/images/%s/%d%s", taskID, index, suffix))
}

func lowerHex(value string) bool {
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return value != ""
}

func cleanupExpiredImageTask(ctx context.Context, task *model.ImageTask, deleteImage func(context.Context, string, int, string) error) (bool, error) {
	if task.ResultExpiresAt == nil || time.Now().Before(*task.ResultExpiresAt) {
		return false, nil
	}
	changed := false
	for index := range task.Data {
		if !model.ValidAuxiliaryImages(task.Data[index]) {
			return false, fmt.Errorf("invalid auxiliary image")
		}
		remove := func(name string) error {
			next := model.CloneImageOutputs(task.Data)
			asset := &next[index]
			if name != "" {
				asset = next[index].AuxiliaryImages[name]
			}
			if asset == nil || !temporaryImageAsset(task.ID, index, name, *asset) {
				return nil
			}
			assetCtx := ctx
			if name != "" {
				assetCtx = ownedimage.WithAuxiliaryName(ctx, name)
			}
			if err := deleteImage(assetCtx, task.ID, index, asset.ContentType); err != nil {
				return err
			}
			asset.URL = ""
			asset.Stored = false
			asset.URLExpiresAt = nil
			if err := model.MarkExpiredImageMediaDeleted(task, next, time.Now().UTC()); err != nil {
				return err
			}
			changed = true
			return nil
		}
		if err := remove(""); err != nil {
			return changed, err
		}
		for name := range task.Data[index].AuxiliaryImages {
			if err := remove(name); err != nil {
				return changed, err
			}
		}
	}
	return changed, nil
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
