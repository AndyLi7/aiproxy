package model_test

import (
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"path/filepath"
	"testing"
	"time"
)

func TestDiscoveryCacheKeepsRealModelCreationTime(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "models.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ModelConfig{}))
	old := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = old })
	created := time.Date(2026, 9, 20, 1, 2, 3, 0, time.UTC)
	require.NoError(t, db.Create(&model.ModelConfig{Model: "brand/image", CreatedAt: created}).Error)
	rows, err := model.GetAllModelConfigs()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.True(t, rows[0].CreatedAt.Equal(created))
}
