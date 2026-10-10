package task

import (
	"context"
	"github.com/labring/aiproxy/core/common/ownedimage"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"path/filepath"
	"testing"
)

func TestMaterialMapIdentitySurvivesArchiveAndDurableReload(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "maps.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}))
	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })
	record := model.ImageTask{ID: "maps", Status: "result_processing", ArchiveRequired: true, ExpectedImages: 2, Data: []model.ImageOutput{{URL: "https://provider.test/color.png", MapType: "basecolor"}, {URL: "https://provider.test/normal.png", MapType: "normal"}}}
	require.NoError(t, db.Create(&record).Error)
	require.NoError(t, archiveImageTask(context.Background(), &record, func(ctx context.Context, source string) (string, ownedimage.Metadata, error) {
		return source + "?archived", ownedimage.Metadata{ContentType: "image/png"}, nil
	}))
	var got model.ImageTask
	require.NoError(t, db.First(&got, "id = ?", record.ID).Error)
	require.Equal(t, "completed", got.Status)
	require.Equal(t, "basecolor", got.Data[0].MapType)
	require.Equal(t, "normal", got.Data[1].MapType)
	require.True(t, got.Data[0].Stored)
	require.True(t, got.Data[1].Stored)
}
