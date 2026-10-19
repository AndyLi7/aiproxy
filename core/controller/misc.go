package controller

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/common/notify"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// usageAttributionRecheck bounds how often an unready LogDB is probed.
const usageAttributionRecheck = 10 * time.Second

// storageReadiness caches model.UsageAttributionStorageReady for /api/status.
// Once true it stays true: the reviewed SQL adds the columns and nothing drops
// them. While false it probes at most every usageAttributionRecheck.
type storageReadiness struct {
	mu        sync.Mutex
	db        *gorm.DB
	readyOnce bool
	checkedAt time.Time
	check     func(*gorm.DB) bool
	now       func() time.Time
}

var usageAttributionStorage = &storageReadiness{
	check: model.UsageAttributionStorageReady,
	now:   time.Now,
}

func (s *storageReadiness) ready(db *gorm.DB) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.db != db {
		// Another database handle (tests swap LogDB): forget the old answer.
		s.db, s.readyOnce, s.checkedAt = db, false, time.Time{}
	}

	if s.readyOnce {
		return true
	}

	now := s.now()
	if !s.checkedAt.IsZero() && now.Sub(s.checkedAt) < usageAttributionRecheck {
		return false
	}

	s.checkedAt = now
	if s.check(db) {
		s.readyOnce = true
		return true
	}

	message := "LogDB lacks logs.billing_operation_id or async_usage_infos.usage_model_id/usage_source/usage_at_ms:" +
		" apply migrations/usage-attribution-v1 before this binary serves traffic"
	log.Error(message)
	notify.ErrorThrottle(
		"usageAttributionStorage",
		5*time.Minute,
		"Usage attribution storage missing",
		message,
	)

	return false
}

type StatusData struct {
	StartTime int64    `json:"startTime"`
	Features  []string `json:"features"`
}

// GetStatus godoc
//
//	@Summary		Get status
//	@Description	Returns the status of the server
//	@Tags			misc
//	@Produce		json
//	@Success		200	{object}	middleware.APIResponse{data=StatusData}
//	@Router			/api/status [get]
func GetStatus(c *gin.Context) {
	if !usageAttributionStorage.ready(model.LogDB) {
		// Railway health-checks this route: a binary ahead of the LogDB schema
		// never receives traffic, and the previous binary keeps serving.
		middleware.ErrorResponse(
			c,
			http.StatusServiceUnavailable,
			"usage_attribution_storage_missing",
		)

		return
	}

	middleware.SuccessResponse(
		c,
		&StatusData{StartTime: common.StartTime, Features: gatewayRuntimeFeatures()},
	)
}

func gatewayRuntimeFeatures() []string {
	features := []string{
		model.AdminDemoFeature,
		model.AdminLogKindFeature,
		"image_billing_v1",
		"image_billing_pixels_v2",
		"image_style_pricing_v1",
		"image_billing_input_basis_v1",
		"image_task_media_v1",
		"token_platform.admin_demo.image_task.v1",
		"channel_failover_acceptance_v1",
		"image_task_batch_v1",
		"image_task_full_batch_delivery_v1",
		"image_task_primary_max_images_v1",
		"upstream_prepayment_v1",
		"registry_provider_fal_async_v1",
		"image_task_metering_v2",
		"image_task_metering_v3",
		"image_task_metering_v4",
		"image_task_metering_v5",
		"image_task_metering_v6",
		"image_task_metering_v7",
		"image_task_metering_v8",
		"image_task_metering_v9",
		"image_task_metering_v10",
		"image_task_metering_v11",
		"image_task_metering_v12",
		"image_task_num_samples_v1",
		"image_task_output_count_v1",
		"image_task_revised_prompt_v1",
		"image_task_used_seed_v1",
		"image_task_provider_metadata_v1",
		"image_task_provider_metadata_v2",
		"image_task_provider_metadata_v3",
		"image_task_combined_outputs_v1",
		"image_task_gif_output_v1",
		"image_task_auxiliary_images_v1",
		"image_task_mask_alias_v1",
		"image_task_overlay_output_v1",
		"image_task_overlay_output_v2",
		"image_task_ordered_layers_v1",
		"image_task_image_source_v1",
		"image_task_map_type_v1",
		"image_task_array_output_count_v1",
		"image_task_output_cardinality_v1",
		"image_task_conditional_count_v1",
		"image_task_named_outputs_v1",
		"image_task_output_controls_v1",
		"registry_provider_fal_queue_v2",
		"registry_provider_ark_sync_task_v1",
		"seedream45_ark_single_v1",
		ChannelRoutingGuardFeature,
	}
	native := nativeRuntimeFeatures()
	features = append(features, native...)
	// nativeRuntimeFeatures is empty exactly while the native engine is not
	// configured.
	features = append(features, nativeRouteFeatures(len(native) > 0)...)
	// Left out (never a 503) while the task view's storage or indexes are not
	// ready; its endpoints then answer 503 task_view_unavailable.
	if taskViewStorage.ready(model.LogDB) {
		features = append(features, model.LogTaskViewFeature)
	}

	return features
}
