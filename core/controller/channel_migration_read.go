package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
)

const migrationReadBodyLimit = 4096

func migrationReadStore() *model.ChannelAdmissionStore {
	return &model.ChannelAdmissionStore{DB: model.DB, LogDB: model.LogDB}
}
func migrationReadReject(c *gin.Context) {
	middleware.ErrorResponse(c, http.StatusBadRequest, "invalid_migration_read_request")
}
func migrationReadNoBody(c *gin.Context) bool {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 1))
	if err != nil || len(raw) != 0 {
		migrationReadReject(c)
		return false
	}
	return true
}
func GetMigrationCommandProtocol(c *gin.Context) {
	if !migrationReadNoBody(c) {
		return
	}
	middleware.SuccessResponse(c, migrationReadStore().ReadMigrationCommandProtocol(c.Request.Context()))
}
func GetPreparedMigrationFacts(c *gin.Context) {
	if !migrationReadNoBody(c) {
		return
	}
	id := c.Param("id")
	if !model.ValidMigrationReadOperationID(id) {
		migrationReadReject(c)
		return
	}
	middleware.SuccessResponse(c, migrationReadStore().ReadPreparedMigrationFacts(c.Request.Context(), id))
}
func ReadMigrationSnapshotFacts(c *gin.Context) {
	var body struct {
		From model.ChannelIdentity `json:"from"`
		To   model.ChannelIdentity `json:"to"`
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, migrationReadBodyLimit+1))
	if err != nil || len(raw) == 0 || len(raw) > migrationReadBodyLimit {
		migrationReadReject(c)
		return
	}
	// DisallowUnknownFields alone accepts duplicate keys. Reject ambiguous input
	// before typed decoding; this endpoint's body contains only bounded objects.
	check := json.NewDecoder(bytes.NewReader(raw))
	check.UseNumber()
	if migrationReadUniqueObject(check, 0) != nil {
		migrationReadReject(c)
		return
	}
	if _, err = check.Token(); err != io.EOF {
		migrationReadReject(c)
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil {
		migrationReadReject(c)
		return
	}
	middleware.SuccessResponse(c, migrationReadStore().ReadMigrationSnapshotFacts(c.Request.Context(), body.From, body.To))
}
func migrationReadUniqueObject(d *json.Decoder, depth int) error {
	if depth > 1 {
		return errors.New("depth")
	}
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return errors.New("object")
	}
	keys := map[string]bool{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok || keys[key] {
			return errors.New("duplicate")
		}
		if depth == 0 && key != "from" && key != "to" {
			return errors.New("field")
		}
		if depth == 1 && key != "channelId" && key != "keyDigest" && key != "identityHash" {
			return errors.New("field")
		}
		keys[key] = true
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return err
		}
		trimmed := bytes.TrimSpace(value)
		if len(trimmed) == 0 {
			return errors.New("value")
		}
		if trimmed[0] == '{' {
			nested := json.NewDecoder(bytes.NewReader(trimmed))
			nested.UseNumber()
			if err = migrationReadUniqueObject(nested, depth+1); err != nil {
				return err
			}
		} else if trimmed[0] == '[' {
			return errors.New("array")
		}
	}
	if depth == 0 && len(keys) != 2 || depth == 1 && len(keys) != 3 {
		return errors.New("missing")
	}
	_, err = d.Token()
	return err
}
