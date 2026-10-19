package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"gorm.io/gorm"
)

func GetAccessConfiguration(c *gin.Context) {
	if !migrationReadNoBody(c) {
		return
	}

	receipt, e := model.ReadAccessConfiguration(c.Request.Context(), c.Param("id"))
	if errors.Is(e, gorm.ErrRecordNotFound) {
		middleware.SuccessResponse(c, (*model.AccessConfigurationReceipt)(nil))
		return
	}

	if e != nil {
		middleware.ErrorResponse(c, http.StatusConflict, "access_configuration_unavailable")
		return
	}

	middleware.SuccessResponse(c, receipt)
}

func EnsureAccessConfiguration(c *gin.Context) {
	// Deliberately small creation contract. Never echoes a request or validator error.
	var input struct {
		Name    string            `json:"name"`
		Key     string            `json:"key"`
		Type    model.ChannelType `json:"type"`
		BaseURL string            `json:"baseUrl"`
	}

	raw, e := io.ReadAll(io.LimitReader(c.Request.Body, 70001))
	if e != nil || len(raw) > 70000 {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid_access_configuration")
		return
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF ||
		len(input.Name) == 0 ||
		utf8.RuneCountInString(input.Name) > 64 ||
		len(input.Key) < 8 ||
		len(input.Key) > 65536 ||
		len(input.BaseURL) > 128 {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid_access_configuration")
		return
	}

	ch, e := (&AddChannelRequest{Name: input.Name, Key: input.Key, Type: input.Type, BaseURL: input.BaseURL, Status: model.ChannelStatusEnabled, Models: []string{}, ModelMapping: map[string]string{}, Sets: []string{"default"}, Configs: map[string]any{"outbound_policy": "public_only"}}).ToChannel()
	if e != nil {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid_access_configuration")
		return
	}

	receipt, e := model.EnsureAccessConfiguration(c.Request.Context(), c.Param("id"), ch)
	if e != nil {
		middleware.ErrorResponse(c, http.StatusConflict, "access_configuration_not_ready")
		return
	}

	middleware.SuccessResponse(c, receipt)
}
