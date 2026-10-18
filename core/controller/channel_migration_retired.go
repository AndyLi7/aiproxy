package controller

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
)

func retiredInputReject(c *gin.Context) {
	middleware.ErrorResponse(c, 400, "invalid_migration_retired_target_request")
}
func retiredDecode(c *gin.Context, shape string, out any) bool {
	raw, e := io.ReadAll(io.LimitReader(c.Request.Body, migrationReadBodyLimit+1))
	if e != nil || len(c.Request.URL.Query()) != 0 || len(raw) == 0 || len(raw) > migrationReadBodyLimit || !shadowInputShape(raw, shape) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(out) == nil
}
func retiredCommand(w model.MigrationRetiredResourceRequest, op string) (model.MigrationRetiredTargetRequest, bool) {
	command, ok := probeCommand(w.Command)
	if !ok || w.Version != 1 || !model.ValidMigrationReadOperationID(op) || command.Seal.OperationID != op || command.TargetResourceKind != model.MigrationReuseRetiredTarget || int64(command.Seal.From.ChannelID) > 9007199254740991 || int64(command.Seal.To.ChannelID) > 9007199254740991 || !validLifecycleHash(w.SourceSnapshotHash) {
		return model.MigrationRetiredTargetRequest{}, false
	}
	return model.MigrationRetiredTargetRequest{Version: 1, Command: command, SourceSnapshotHash: w.SourceSnapshotHash}, true
}
func (d migrationProbeDependencies) createRetired(c *gin.Context) {
	var wire model.MigrationRetiredResourceRequest
	if !retiredDecode(c, "retired", &wire) {
		retiredInputReject(c)
		return
	}
	q, ok := retiredCommand(wire, c.Param("id"))
	if !ok {
		retiredInputReject(c)
		return
	}
	s, e := d.store()
	if e != nil {
		middleware.SuccessResponse(c, model.MigrationRetiredResourceResult{Version: 1, Source: "gateway-migration-retired-target-v1", OperationID: c.Param("id"), State: "unavailable", Reason: "migration_retired_target_unavailable"})
		return
	}
	middleware.SuccessResponse(c, s.CreateMigrationRetiredTargetFacts(c.Request.Context(), q))
}
func CreateMigrationRetiredTarget(c *gin.Context) { migrationProbeDefaults().createRetired(c) }

// Read takes the complete original request; it neither obtains an issuer nor
// mutates state. Its result is historical evidence, never execution permission.
func ReadMigrationRetiredTarget(c *gin.Context) {
	var body struct {
		Version int                                   `json:"version"`
		Request model.MigrationRetiredResourceRequest `json:"request"`
	}
	if !retiredDecode(c, "retired-read", &body) || body.Version != 1 {
		retiredInputReject(c)
		return
	}
	q, ok := retiredCommand(body.Request, c.Param("id"))
	if !ok {
		retiredInputReject(c)
		return
	}
	middleware.SuccessResponse(c, migrationReadStore().ReadMigrationRetiredTargetFacts(c.Request.Context(), q))
}
func CancelMigrationRetiredTarget(c *gin.Context) {
	var body struct {
		Version             int    `json:"version"`
		ResourceRequestHash string `json:"resourceRequestHash"`
		CheckpointHash      string `json:"checkpointHash"`
	}
	if !retiredDecode(c, "cancel", &body) || body.Version != 1 || !model.ValidMigrationReadOperationID(c.Param("id")) || !validLifecycleHash(body.ResourceRequestHash) || !validLifecycleHash(body.CheckpointHash) {
		retiredInputReject(c)
		return
	}
	middleware.SuccessResponse(c, migrationReadStore().CancelMigrationRetiredTargetFacts(c.Request.Context(), c.Param("id"), body.ResourceRequestHash, body.CheckpointHash))
}
