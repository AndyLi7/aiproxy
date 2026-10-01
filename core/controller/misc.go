package controller

import (
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
)

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
	status := &StatusData{
		StartTime: common.StartTime,
		Features: []string{
			model.AdminDemoFeature,
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
			"image_task_metering_v7",
			"image_task_metering_v8", "image_task_metering_v9", "image_task_metering_v10", "image_task_metering_v11", "image_task_metering_v12", "image_task_num_samples_v1",
			"image_task_output_count_v1", "image_task_revised_prompt_v1", "image_task_used_seed_v1", "image_task_provider_metadata_v1", "image_task_provider_metadata_v2", "image_task_provider_metadata_v3", "image_task_combined_outputs_v1", "image_task_gif_output_v1", "image_task_auxiliary_images_v1", "image_task_mask_alias_v1", "image_task_overlay_output_v1", "image_task_overlay_output_v2",
			"image_task_ordered_layers_v1", "image_task_image_source_v1", "image_task_map_type_v1", "image_task_array_output_count_v1", "image_task_output_cardinality_v1", "image_task_conditional_count_v1", "image_task_named_outputs_v1", "image_task_output_controls_v1",
			"registry_provider_fal_queue_v2",
			"registry_provider_ark_sync_task_v1",
			"seedream45_ark_single_v1",
		},
	}
	status.Features = append(status.Features, nativeRuntimeFeatures()...)
	middleware.SuccessResponse(c, status)
}
