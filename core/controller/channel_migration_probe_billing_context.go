package controller

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
)

func ReadMigrationProbeBillingContext(c *gin.Context) {
	raw, e := io.ReadAll(io.LimitReader(c.Request.Body, migrationReadBodyLimit+1))
	if e != nil || len(raw) == 0 || len(raw) > migrationReadBodyLimit ||
		!model.ValidMigrationProbeJSON(raw) {
		migrationReadReject(c)
		return
	}

	var body model.MigrationProbeBillingContextRequest

	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()

	if d.Decode(&body) != nil || !model.ValidMigrationProbeBillingContextRequest(body) {
		migrationReadReject(c)
		return
	}

	middleware.SuccessResponse(
		c,
		migrationReadStore().ReadMigrationProbeBillingContext(c.Request.Context(), body),
	)
}
