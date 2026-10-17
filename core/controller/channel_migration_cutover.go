package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
)

func (d migrationProbeDependencies) cutover(c *gin.Context) {
	var body model.MigrationProbeCutoverBinding
	raw, e := io.ReadAll(io.LimitReader(c.Request.Body, migrationReadBodyLimit+1))
	var fields map[string]json.RawMessage
	exactFields := json.Unmarshal(raw, &fields) == nil && len(fields) == 5
	for k := range fields {
		switch k {
		case "version", "authorizationHash", "requestHash", "probeFactHash", "storageGeneration":
		default:
			exactFields = false
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if e != nil || len(c.Request.URL.Query()) != 0 || !exactFields || len(raw) == 0 || len(raw) > migrationReadBodyLimit || !model.ValidMigrationProbeJSON(raw) || decoder.Decode(&body) != nil || !body.Valid() || !model.ValidMigrationReadOperationID(c.Param("id")) {
		middleware.ErrorResponse(c, 400, "invalid_migration_cutover_request")
		return
	}
	s, e := d.store()
	if e == nil {
		_, e = s.CutoverMigrationFromProbe(c.Request.Context(), c.Param("id"), body)
	}
	if e != nil {
		state, reason := "unavailable", "migration_cutover_unavailable"
		if errors.Is(e, model.ErrChannelAdmissionConflict) {
			state, reason = "conflict", "migration_cutover_binding_conflict"
		}
		middleware.SuccessResponse(c, model.MigrationReadResult{Version: 1, Source: model.MigrationReadSource, State: state, Reason: reason})
		return
	}
	// A readback failure is never converted to success. After an uncertain HTTP
	// outcome the caller may GET the exact operation without another write.
	middleware.SuccessResponse(c, s.ReadPreparedMigrationFacts(c.Request.Context(), c.Param("id")))
}
func CutoverMigrationProbe(c *gin.Context) { migrationProbeDefaults().cutover(c) }
