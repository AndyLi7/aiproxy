package controller

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
)

// RequireMigrationInventoryHeaderAuth runs before AdminAuth. This review source
// never accepts credentials in a URL, and even denial responses cannot be cached.
func RequireMigrationInventoryHeaderAuth(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")

	if c.Request.URL.RawQuery != "" || c.GetHeader("Authorization") == "" {
		middleware.ErrorResponse(c, http.StatusUnauthorized, "inventory_header_auth_required")
		c.Abort()
		return
	}

	c.Next()
}

func GetMigrationInventory(c *gin.Context) {
	store := &model.ChannelAdmissionStore{DB: model.DB, LogDB: model.LogDB}
	inventory := store.ReadMigrationInventory(c.Request.Context(), gatewayRuntimeFeatures())
	middleware.SuccessResponse(c, inventory)
}
