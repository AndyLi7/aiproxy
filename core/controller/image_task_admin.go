package controller

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"gorm.io/gorm"
)

// GetGroupImageTask returns a group-scoped task through the admin API. The
// public task projection replaces upstream URLs with short-lived gateway URLs.
func GetGroupImageTask(c *gin.Context) {
	id, group := c.Param("id"), c.Param("group")
	if !imageRequestID.MatchString(id) || group == "" || len(group) > 64 {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid image task lookup")
		return
	}
	task, err := model.GetGroupImageTask(id, group)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		middleware.ErrorResponse(c, http.StatusNotFound, "record not found")
		return
	}
	if err != nil {
		middleware.ErrorResponse(c, http.StatusServiceUnavailable, "image task unavailable")
		return
	}
	result := publicImageTask(c, task)
	middleware.SuccessResponse(c, gin.H{
		"id":                  result.ID,
		"model":               result.Model,
		"status":              result.Status,
		"data":                result.Data,
		"result_expires_at":   result.ResultExpiresAt,
		"result_availability": result.ResultAvailability,
	})
}
