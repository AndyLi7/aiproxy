package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/labring/aiproxy/core/common/ownedimage"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"path/filepath"
	"testing"
)

func TestLayerArchivePreservesAssociationsAcrossRetry(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "layers.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}))
	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })
	images := make([]model.ImageOutput, 17)
	for i := range images {
		images[i] = model.ImageOutput{URL: fmt.Sprintf("https://provider.test/%d.png", i), Layer: json.RawMessage(fmt.Sprintf(`{"z_index":%d,"name":null,"bounding_box":null}`, i))}
	}
	original := model.CloneImageOutputs(images)
	job := model.ImageTask{ID: "layers", Status: "result_processing", ArchiveRequired: true, ExpectedImages: 17, Data: images}
	require.NoError(t, db.Create(&job).Error)
	calls := map[string]int{}
	interrupted := false
	archive := func(ctx context.Context, src string) (string, ownedimage.Metadata, error) {
		calls[src]++
		if src == original[8].URL && !interrupted {
			interrupted = true
			return "", ownedimage.Metadata{}, errors.New("interrupted")
		}
		return "https://owned.test/" + src[len("https://provider.test/"):], ownedimage.Metadata{ContentType: "image/png", Width: 10, Height: 20}, nil
	}
	require.Error(t, archiveImageTask(context.Background(), &job, archive))
	var resumed model.ImageTask
	require.NoError(t, db.First(&resumed, "id = ?", job.ID).Error)
	require.NoError(t, archiveImageTask(context.Background(), &resumed, archive))
	var final model.ImageTask
	require.NoError(t, db.First(&final, "id = ?", job.ID).Error)
	require.Equal(t, "completed", final.Status)
	require.Len(t, final.Data, 17)
	for i, image := range final.Data {
		require.JSONEq(t, string(original[i].Layer), string(image.Layer))
		require.Equal(t, fmt.Sprintf("https://owned.test/%d.png", i), image.URL)
		expected := 1
		if i == 8 {
			expected = 2
		}
		require.Equal(t, expected, calls[original[i].URL])
	}
	var submissions int64
	require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Count(&submissions).Error)
	require.Zero(t, submissions)
}
