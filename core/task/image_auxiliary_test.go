package task

import (
	"context"
	"errors"
	"github.com/labring/aiproxy/core/common/ownedimage"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAuxiliaryImageArchiveResumesWithoutExtraGeneration(t *testing.T) {
	for _, name := range []string{"mask_image", "transparent_overlay"} {
		t.Run(name, func(t *testing.T) {
			db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "aux.db"))
			require.NoError(t, err)
			require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}))
			old := model.LogDB
			model.LogDB = db
			t.Cleanup(func() { model.LogDB = old })
			task := model.ImageTask{ID: "aux-task", Status: "result_processing", ArchiveRequired: true, ExpectedImages: 1, Data: []model.ImageOutput{{URL: "https://provider.test/main.png", AuxiliaryImages: map[string]*model.ImageOutput{name: {URL: "https://provider.test/mask.png"}}}}}
			require.NoError(t, db.Create(&task).Error)
			calls := 0
			err = archiveImageTask(context.Background(), &task, func(ctx context.Context, src string) (string, ownedimage.Metadata, error) {
				calls++
				if ownedimage.AuxiliaryName(ctx) != "" {
					require.Equal(t, name, ownedimage.AuxiliaryName(ctx))
					return "", ownedimage.Metadata{}, errors.New("storage retry")
				}
				require.Equal(t, "https://provider.test/main.png", src)
				return "https://media.test/generated-results/images/aux-task/0.png", ownedimage.Metadata{Width: 10, Height: 20, ContentType: "image/png"}, nil
			})
			require.Error(t, err)
			var saved model.ImageTask
			require.NoError(t, db.First(&saved, "id = ?", task.ID).Error)
			require.Len(t, saved.Data, 1)
			require.True(t, saved.Data[0].Stored)
			require.False(t, saved.Data[0].AuxiliaryImages[name].Stored)
			require.Equal(t, "result_processing", saved.Status)
			require.Error(t, model.CompleteImageArchive(&saved))
			require.NoError(t, archiveImageTask(context.Background(), &saved, func(ctx context.Context, src string) (string, ownedimage.Metadata, error) {
				calls++
				require.Equal(t, name, ownedimage.AuxiliaryName(ctx))
				require.Equal(t, "https://provider.test/mask.png", src)
				return strings.ReplaceAll("https://media.test/generated-results/images/aux-task/0-mask_image.png", "mask_image", name), ownedimage.Metadata{Width: 10, Height: 20, ContentType: "image/png"}, nil
			}))
			require.Equal(t, 3, calls)
			require.NoError(t, db.First(&saved, "id = ?", task.ID).Error)
			require.Equal(t, "completed", saved.Status)
			require.Len(t, saved.Data, 1)
			require.True(t, saved.Data[0].AuxiliaryImages[name].Stored)
			var submitted int64
			require.NoError(t, db.Model(&model.AsyncUsageInfo{}).Count(&submitted).Error)
			require.Zero(t, submitted)
			expired := time.Now().Add(-time.Hour)
			saved.ResultExpiresAt = &expired
			require.NoError(t, db.Model(&saved).Update("result_expires_at", expired).Error)
			deleted := []string{}
			changed, err := cleanupExpiredImageTask(context.Background(), &saved, func(ctx context.Context, id string, index int, mime string) error {
				require.Equal(t, "aux-task", id)
				require.Equal(t, 0, index)
				deleted = append(deleted, ownedimage.AuxiliaryName(ctx))
				return nil
			})
			require.NoError(t, err)
			require.True(t, changed)
			require.ElementsMatch(t, []string{"", name}, deleted)
			require.NoError(t, db.First(&saved, "id = ?", task.ID).Error)
			require.Empty(t, saved.Data[0].URL)
			require.Empty(t, saved.Data[0].AuxiliaryImages[name].URL)
			require.Equal(t, "completed", saved.Status)

		})
	}
}
func TestAuxiliaryImageNullAndInvalidNames(t *testing.T) {
	for _, name := range []string{"mask_image", "transparent_overlay"} {
		t.Run(name, func(t *testing.T) {
			require.True(t, model.ValidAuxiliaryImages(model.ImageOutput{AuxiliaryImages: map[string]*model.ImageOutput{name: nil}}))
			for _, name := range []string{"../mask", "unknown", strings.ReplaceAll("mask_image/0", "mask_image", name)} {
				require.False(t, model.ValidAuxiliaryImages(model.ImageOutput{AuxiliaryImages: map[string]*model.ImageOutput{name: {URL: "https://image.test/a"}}}))
			}
			require.False(t, model.ValidAuxiliaryImages(model.ImageOutput{AuxiliaryImages: map[string]*model.ImageOutput{name: {AuxiliaryImages: map[string]*model.ImageOutput{name: nil}}}}))

		})
	}
}
func TestAuxiliaryCleanupFailureResumesOnlyRemainingAsset(t *testing.T) {
	for _, name := range []string{"mask_image", "transparent_overlay"} {
		t.Run(name, func(t *testing.T) {
			db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "aux-cleanup.db"))
			require.NoError(t, err)
			require.NoError(t, db.AutoMigrate(&model.ImageTask{}))
			old := model.LogDB
			model.LogDB = db
			t.Cleanup(func() { model.LogDB = old })
			past := time.Now().Add(-time.Hour)
			record := model.ImageTask{ID: "retry", Status: "completed", ArchiveRequired: true, ResultExpiresAt: &past, Data: []model.ImageOutput{{URL: "https://owned.test/generated-results/images/retry/0.png", ContentType: "image/png", Stored: true, AuxiliaryImages: map[string]*model.ImageOutput{name: {URL: strings.ReplaceAll("https://owned.test/generated-results/images/retry/0-mask_image.png", "mask_image", name), ContentType: "image/png", Stored: true}}}}}
			require.NoError(t, db.Create(&record).Error)
			changed, err := cleanupExpiredImageTask(context.Background(), &record, func(ctx context.Context, _ string, _ int, _ string) error {
				if ownedimage.AuxiliaryName(ctx) != "" {
					return errors.New("temporary failure")
				}
				return nil
			})
			require.True(t, changed)
			require.Error(t, err)
			var saved model.ImageTask
			require.NoError(t, db.First(&saved, "id = ?", record.ID).Error)
			require.Empty(t, saved.Data[0].URL)
			require.NotEmpty(t, saved.Data[0].AuxiliaryImages[name].URL)
			changed, err = cleanupExpiredImageTask(context.Background(), &saved, func(ctx context.Context, _ string, _ int, _ string) error {
				require.Equal(t, name, ownedimage.AuxiliaryName(ctx))
				return nil
			})
			require.NoError(t, err)
			require.True(t, changed)
			require.Empty(t, saved.Data[0].AuxiliaryImages[name].URL)

		})
	}
}
