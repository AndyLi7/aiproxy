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

const migrationShadowBodyLimit = 640 * 1024

func shadowInputReject(c *gin.Context) {
	middleware.ErrorResponse(c, http.StatusBadRequest, "invalid_migration_shadow_request")
}

func GetMigrationShadow(c *gin.Context) {
	if !migrationReadNoBody(c) {
		return
	}

	op := c.Param("id")
	if !model.ValidMigrationReadOperationID(op) {
		shadowInputReject(c)
		return
	}

	middleware.SuccessResponse(
		c,
		migrationReadStore().ReadMigrationShadow(c.Request.Context(), op, gatewayRuntimeFeatures()),
	)
}

func CreateMigrationShadow(c *gin.Context) {
	op := c.Param("id")
	if !model.ValidMigrationReadOperationID(op) {
		shadowInputReject(c)
		return
	}

	raw, e := io.ReadAll(io.LimitReader(c.Request.Body, migrationShadowBodyLimit+1))
	if e != nil || len(raw) == 0 || len(raw) > migrationShadowBodyLimit ||
		!shadowInputShape(raw, "root") {
		shadowInputReject(c)
		return
	}

	var body struct {
		Request model.MigrationShadowRequest `json:"request"`
		Key     string                       `json:"key"`
	}

	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()

	if d.Decode(&body) != nil || body.Request.OperationID != op {
		shadowInputReject(c)
		return
	}
	// Never bind through a generic logging/validation error that may echo body/key.
	middleware.SuccessResponse(
		c,
		migrationReadStore().CreateMigrationShadow(c.Request.Context(), body.Request, body.Key, gatewayRuntimeFeatures()),
	)
}

func shadowInputShape(raw []byte, kind string) bool {
	fields, valid := shadowShapeFields(kind)
	if !valid {
		return false
	}

	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()

	first, e := d.Token()
	if e != nil || first != json.Delim('{') {
		return false
	}

	seen := map[string]bool{}
	for d.More() {
		token, e := d.Token()
		if e != nil {
			return false
		}

		key, ok := token.(string)
		if !ok || seen[key] {
			return false
		}

		seen[key] = true

		nested, known := fields[key]
		if kind != "mapping" && !known {
			return false
		}

		var v json.RawMessage
		if d.Decode(&v) != nil {
			return false
		}

		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) &&
			(kind != "projection" || key != "configsJson") {
			return false
		}

		if nested != "" && !shadowInputShape(v, nested) {
			return false
		}

		if kind == "mapping" {
			var str string
			if json.Unmarshal(v, &str) != nil {
				return false
			}
		}
	}

	if _, e = d.Token(); e != nil {
		return false
	}

	if _, e = d.Token(); e != io.EOF {
		return false
	}

	return kind == "mapping" || len(seen) == len(fields)
}

func shadowShapeFields(kind string) (map[string]string, bool) {
	fields := map[string]string{}
	switch kind {
	case "retired":
		fields = map[string]string{
			"version":            "",
			"command":            "retired-command",
			"sourceSnapshotHash": "",
		}
	case "retired-read":
		fields = map[string]string{"version": "", "request": "retired"}
	case "retired-command":
		fields = map[string]string{
			"targetResourceKind": "",
			"seal":               "retired-seal",
			"fromCredentialId":   "",
			"toCredentialId":     "",
			"expectedPinHash":    "",
			"projectionHash":     "",
			"familyHash":         "",
		}
	case "retired-seal":
		fields = map[string]string{
			"operationId":      "",
			"epoch":            "",
			"expectedRevision": "",
			"from":             "identity",
			"to":               "identity",
			"revertOf":         "",
		}
	case "cancel":
		fields = map[string]string{"version": "", "resourceRequestHash": "", "checkpointHash": ""}
	case "root":
		fields = map[string]string{"request": "request", "key": ""}
	case "request":
		fields = map[string]string{
			"version":     "",
			"operationId": "",
			"source":      "source",
			"target":      "target",
		}
	case "source":
		fields = map[string]string{
			"sourceSnapshotHash": "",
			"epoch":              "",
			"revision":           "",
			"storageGeneration":  "",
			"from":               "identity",
		}
	case "identity":
		fields = map[string]string{"channelId": "", "keyDigest": "", "identityHash": ""}
	case "target":
		fields = map[string]string{
			"credentialId":     "",
			"credentialDigest": "",
			"projection":       "projection",
		}
	case "projection":
		fields = map[string]string{
			"name":                   "",
			"type":                   "",
			"baseUrl":                "",
			"proxyUrl":               "",
			"skipTlsVerify":          "",
			"models":                 "",
			"modelMapping":           "mapping",
			"sets":                   "",
			"priority":               "",
			"backupOnly":             "",
			"enabledNoPermissionBan": "",
			"warnErrorRate":          "",
			"maxErrorRate":           "",
			"configsJson":            "",
		}
	case "mapping": // arbitrary model IDs, string values only, duplicate keys rejected below
	default:
		return nil, false
	}

	return fields, true
}
