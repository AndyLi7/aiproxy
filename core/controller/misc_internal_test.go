package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// statusLogDB points the status check at a LogDB with every usage
// attribution column (key usage P2).
func statusLogDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "status.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() {
		model.LogDB = old

		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	return db
}

func TestStatusAdvertisesAdminDemoV1(t *testing.T) {
	gin.SetMode(gin.TestMode)
	statusLogDB(t)

	router := gin.New()
	router.GET("/api/status", GetStatus)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(
		recorder,
		httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/status", nil),
	)
	require.Equal(t, http.StatusOK, recorder.Code)

	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			StartTime int64    `json:"startTime"`
			Features  []string `json:"features"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	require.True(t, payload.Success)
	require.NotZero(t, payload.Data.StartTime)
	require.Equal(
		t,
		[]string{
			"token_platform.admin_demo.v1",
			"admin_log_kind_v1",
			"image_billing_v1",
			"image_billing_pixels_v2",
			"image_style_pricing_v1",
			"image_billing_input_basis_v1",
			"image_task_media_v1",
			"token_platform.admin_demo.image_task.v1",
			"channel_failover_acceptance_v1",
			"image_task_batch_v1", "image_task_full_batch_delivery_v1", "image_task_primary_max_images_v1",
			"upstream_prepayment_v1",
			"registry_provider_fal_async_v1",
			"image_task_metering_v2",
			"image_task_metering_v3",
			"image_task_metering_v4",
			"image_task_metering_v5",
			"image_task_metering_v6",
			"image_task_metering_v7", "image_task_metering_v8", "image_task_metering_v9", "image_task_metering_v10", "image_task_metering_v11", "image_task_metering_v12", "image_task_num_samples_v1",
			"image_task_output_count_v1", "image_task_revised_prompt_v1", "image_task_used_seed_v1", "image_task_provider_metadata_v1", "image_task_provider_metadata_v2", "image_task_provider_metadata_v3", "image_task_combined_outputs_v1", "image_task_gif_output_v1", "image_task_auxiliary_images_v1", "image_task_mask_alias_v1", "image_task_overlay_output_v1", "image_task_overlay_output_v2", "image_task_ordered_layers_v1", "image_task_image_source_v1", "image_task_map_type_v1", "image_task_array_output_count_v1", "image_task_output_cardinality_v1", "image_task_conditional_count_v1", "image_task_named_outputs_v1", "image_task_output_controls_v1",
			"registry_provider_fal_queue_v2",
			"registry_provider_ark_sync_task_v1",
			"seedream45_ark_single_v1",
			// The LogDB has the task view's columns (SQLite has no index check).
			"log_task_view_v1",
		},
		payload.Data.Features,
	)
}

func getStatus(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()

	router := gin.New()
	router.GET("/api/status", GetStatus)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/status", nil))

	return recorder
}

// A binary ahead of its LogDB schema would fail every insert into logs and
// async_usage_infos, and those failures are only logged: video and measured
// image calls would be delivered and never charged. /api/status (Railway's
// health check) therefore reports 503 until migrations/usage-attribution-v1
// is applied, so such a deploy never receives traffic. Old: 200.
func TestUsageAttributionStorageGatesStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := statusLogDB(t)

	// An unmigrated P1 LogDB: the four columns do not exist.
	for _, statement := range []string{
		"ALTER TABLE logs DROP COLUMN billing_operation_id",
		"ALTER TABLE async_usage_infos DROP COLUMN usage_model_id",
		"ALTER TABLE async_usage_infos DROP COLUMN usage_source",
		"ALTER TABLE async_usage_infos DROP COLUMN usage_at_ms",
	} {
		require.NoError(t, db.Exec(statement).Error, statement)
	}

	now := time.Now()
	oldReadiness := usageAttributionStorage
	usageAttributionStorage = &storageReadiness{check: model.UsageAttributionStorageReady, now: func() time.Time { return now }}
	t.Cleanup(func() { usageAttributionStorage = oldReadiness })

	require.False(t, model.UsageAttributionStorageReady(db))

	recorder := getStatus(t)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Contains(t, recorder.Body.String(), "usage_attribution_storage_missing")
	require.NotContains(t, recorder.Body.String(), "features")

	// What the binary would lose without the gate: the insert fails.
	info := &model.AsyncUsageInfo{RequestID: "video", GroupID: "g", TokenID: 1}
	info.SetUsageIdentity(model.NewUsageIdentity(1, "veo", "text-to-video", model.RequestSourceAPI, now))
	require.Error(t, model.CreateAsyncUsageInfo(info))

	raw, err := os.ReadFile(filepath.Join("..", "migrations", "usage-attribution-v1", "sqlite.sql"))
	require.NoError(t, err)

	var script []string
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			script = append(script, line)
		}
	}

	for _, statement := range strings.Split(strings.Join(script, "\n"), ";") {
		if body := strings.TrimSpace(statement); body != "" {
			require.NoError(t, db.Exec(body).Error, body)
		}
	}

	require.True(t, model.UsageAttributionStorageReady(db))

	// While unready the check runs at most every 10 s.
	recorder = getStatus(t)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)

	now = now.Add(usageAttributionRecheck)
	recorder = getStatus(t)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Contains(t, recorder.Body.String(), model.AdminDemoFeature)
	require.NoError(t, model.CreateAsyncUsageInfo(info))

	// Once ready it stays ready without probing again.
	usageAttributionStorage.check = func(*gorm.DB) bool { t.Fatal("probed after ready"); return false }
	require.Equal(t, http.StatusOK, getStatus(t).Code)
}
