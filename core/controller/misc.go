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
	middleware.SuccessResponse(c, &StatusData{
		StartTime: common.StartTime,
		Features:  []string{model.AdminDemoFeature, "image_billing_v1", "image_billing_input_basis_v1", "image_task_media_v1", "image_task_batch_v1", "registry_provider_fal_async_v1", "registry_provider_fal_queue_v2", "registry_provider_ark_sync_task_v1"},
	})
}
