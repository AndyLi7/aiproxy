package controller

import (
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
)

func GetMigrationCacheObservation(c *gin.Context) {
	if !migrationReadNoBody(c) {
		return
	}

	id := c.Param("id")
	if !model.ValidMigrationReadOperationID(id) {
		migrationReadReject(c)
		return
	}

	middleware.SuccessResponse(
		c,
		migrationReadStore().ReadMigrationCacheObservation(c.Request.Context(), id),
	)
}
