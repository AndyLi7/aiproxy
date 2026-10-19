package controller

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
)

func lifecycleInputReject(c *gin.Context) {
	middleware.ErrorResponse(c, http.StatusBadRequest, "invalid_migration_shadow_lifecycle_request")
}

func GetMigrationShadowLifecycle(c *gin.Context) {
	if !migrationReadNoBody(c) {
		return
	}

	op := c.Param("id")
	if !model.ValidMigrationReadOperationID(op) {
		lifecycleInputReject(c)
		return
	}

	middleware.SuccessResponse(
		c,
		migrationReadStore().ReadMigrationShadowLifecycleFacts(c.Request.Context(), op),
	)
}

func CancelMigrationShadow(c *gin.Context) {
	op := c.Param("id")
	if !model.ValidMigrationReadOperationID(op) {
		lifecycleInputReject(c)
		return
	}

	raw, e := io.ReadAll(io.LimitReader(c.Request.Body, migrationReadBodyLimit+1))
	if e != nil || len(raw) == 0 || len(raw) > migrationReadBodyLimit ||
		!shadowInputShape(raw, "cancel") {
		lifecycleInputReject(c)
		return
	}

	var body struct {
		Version             int    `json:"version"`
		ResourceRequestHash string `json:"resourceRequestHash"`
		CheckpointHash      string `json:"checkpointHash"`
	}

	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()

	if d.Decode(&body) != nil || body.Version != 2 ||
		!validLifecycleHash(body.ResourceRequestHash) ||
		!validLifecycleHash(body.CheckpointHash) {
		lifecycleInputReject(c)
		return
	}

	middleware.SuccessResponse(
		c,
		migrationReadStore().CancelMigrationShadowFacts(c.Request.Context(), op, body.ResourceRequestHash, body.CheckpointHash),
	)
}

func validLifecycleHash(v string) bool {
	if len(v) != 64 {
		return false
	}

	for _, c := range v {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}
