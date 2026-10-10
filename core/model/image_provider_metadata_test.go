package model_test

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestProviderMetadataSurvivesDurableResultAndReplay(t *testing.T) {
	for _, synchronous := range []bool{false, true} {
		t.Run(map[bool]string{false: "async", true: "sync"}[synchronous], func(t *testing.T) {
			db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
			require.NoError(t, err)
			require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))
			old := model.LogDB
			model.LogDB = db
			t.Cleanup(func() { model.LogDB = old })
			task, _, err := model.ReserveImageTask(&model.ImageTask{ID: "metadata", GroupID: "g", TokenID: 1, Model: "image", Fingerprint: "x"}, &model.AsyncUsageInfo{RequestID: "metadata"})
			require.NoError(t, err)
			metadata := model.ImageResultMetadata{ProviderMetadata: map[string]json.RawMessage{
				"caption":       json.RawMessage(`"A generated picture"`),
				"large_integer": json.RawMessage(`18446744073709551615`),
				"nullable":      json.RawMessage(`null`),
				"palette":       json.RawMessage(`["#ffffff","#000000"]`),
				"nested":        json.RawMessage(`{"zero":0,"flag":false,"empty":""}`),
			}}
			save := func(meta model.ImageResultMetadata) error {
				if synchronous {
					return model.CompleteSyncImageTask(task.ID, []model.ImageOutput{{URL: "https://example.com/a.png"}}, meta)
				}
				return model.SetImageTaskResult(task.ID, "completed", []model.ImageOutput{{URL: "https://example.com/a.png"}}, nil, meta)
			}
			require.Error(t, save(model.ImageResultMetadata{ProviderMetadata: map[string]json.RawMessage{"broken": json.RawMessage(`{`)}}))
			unchanged, err := model.GetImageTask(task.ID, "g", 1)
			require.NoError(t, err)
			require.NotEqual(t, "completed", unchanged.Status)
			require.NoError(t, save(metadata))
			got, err := model.GetImageTask(task.ID, "g", 1)
			require.NoError(t, err)
			require.Equal(t, metadata.ProviderMetadata, got.ProviderMetadata)
			encoded, err := json.Marshal(got)
			require.NoError(t, err)
			require.Contains(t, string(encoded), `"large_integer":18446744073709551615`)
			require.Contains(t, string(encoded), `"nullable":null`)
			require.Contains(t, string(encoded), `"provider_metadata":`)
			replayErr := save(model.ImageResultMetadata{ProviderMetadata: map[string]json.RawMessage{"caption": json.RawMessage(`"replacement"`)}})
			if synchronous {
				require.Error(t, replayErr)
			} else {
				require.NoError(t, replayErr)
			}
			got, err = model.GetImageTask(task.ID, "g", 1)
			require.NoError(t, err)
			require.Equal(t, metadata.ProviderMetadata, got.ProviderMetadata)
		})
	}
}

func TestLegacyImageTaskDoesNotExposeEmptyProviderMetadata(t *testing.T) {
	encoded, err := json.Marshal(model.ImageTask{ID: "legacy"})
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "provider_metadata")
}

func TestProviderMetadataNullableMigrationPreservesExistingRows(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "legacy.db"))
	require.NoError(t, err)
	require.NoError(t, db.Exec("CREATE TABLE image_tasks (id TEXT PRIMARY KEY, status TEXT)").Error)
	require.NoError(t, db.Exec("INSERT INTO image_tasks (id,status) VALUES ('legacy','completed')").Error)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}))
	var task model.ImageTask
	require.NoError(t, db.First(&task, "id = ?", "legacy").Error)
	require.Equal(t, "completed", task.Status)
	require.Nil(t, task.ProviderMetadata)
	require.True(t, db.Migrator().HasColumn(&model.ImageTask{}, "provider_metadata"))
	encoded, err := json.Marshal(task)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "provider_metadata")
}
