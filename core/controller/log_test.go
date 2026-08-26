//nolint:testpackage
package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
)

func TestParseExcludedModesAcceptsBoundedPositiveIntegers(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = &http.Request{URL: &url.URL{RawQuery: "exclude_modes=37%2C41"}}

	got, err := parseExcludedModes(c)
	if err != nil {
		t.Fatalf("parse excluded modes: %v", err)
	}
	if !reflect.DeepEqual(got, []int{37, 41}) {
		t.Fatalf("excluded modes = %v, want [37 41]", got)
	}
}

func TestParseExcludedModesRejectsMalformedOrUnboundedValues(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []string{
		"exclude_modes=",
		"exclude_modes=0",
		"exclude_modes=-1",
		"exclude_modes=37%2C",
		"exclude_modes=37%2Cinvalid",
		"exclude_modes=2147483648",
		"exclude_modes=37%2C37",
		"exclude_modes=" + strings.TrimSuffix(strings.Repeat("37%2C", 33), "%2C"),
	}
	for _, rawQuery := range tests {
		t.Run(rawQuery, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = &http.Request{URL: &url.URL{RawQuery: rawQuery}}

			if _, err := parseExcludedModes(c); err == nil {
				t.Fatalf("expected %q to be rejected", rawQuery)
			}
		})
	}
}

func TestGetGroupLogsRejectsInvalidExcludeModes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "group", Value: "group-a"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/api/log/group-a?exclude_modes=0", nil)

	GetGroupLogs(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid exclude_modes parameter") {
		t.Fatalf("body = %q, want safe invalid parameter message", w.Body.String())
	}
}

func TestGetGroupLogsExcludesModesFromRowsAndTotal(t *testing.T) {
	gin.SetMode(gin.TestMode)

	database, err := model.OpenSQLite(filepath.Join(t.TempDir(), "controller-group-logs.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	previousLogDB := model.LogDB
	model.LogDB = database
	t.Cleanup(func() {
		model.LogDB = previousLogDB
		sqlDB, sqlErr := database.DB()
		if sqlErr == nil {
			_ = sqlDB.Close()
		}
	})
	if err := database.AutoMigrate(&model.Log{}, &model.RequestDetail{}, &model.GroupSummary{}); err != nil {
		t.Fatalf("migrate log database: %v", err)
	}

	createdAt := time.Unix(1_787_083_200, 0)
	logs := []model.Log{
		{GroupID: "group-a", RequestID: "req-create", Model: "seedance", Mode: 22, Code: 200, CreatedAt: createdAt},
		{GroupID: "group-a", RequestID: "req-poll", Mode: 37, Code: 500, CreatedAt: createdAt.Add(time.Second)},
		{GroupID: "group-b", RequestID: "req-other-tenant", Model: "seedance", Mode: 22, Code: 200, CreatedAt: createdAt.Add(2 * time.Second)},
	}
	if err := database.Create(&logs).Error; err != nil {
		t.Fatalf("seed logs: %v", err)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "group", Value: "group-a"}}
	c.Request = httptest.NewRequest(
		http.MethodGet,
		"/api/log/group-a?page=1&per_page=20&exclude_modes=37&start_timestamp=1787083199&end_timestamp=1787083203",
		nil,
	)

	GetGroupLogs(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}
	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			Logs []struct {
				RequestID string `json:"request_id"`
			} `json:"logs"`
			Total int64 `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !payload.Success || payload.Data.Total != 1 {
		t.Fatalf("response = %s, want one filtered row", w.Body.String())
	}
	if len(payload.Data.Logs) != 1 || payload.Data.Logs[0].RequestID != "req-create" {
		t.Fatalf("response = %s, want only req-create", w.Body.String())
	}
}

func TestParseCommonParamsUsesIncludeDetail(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = &http.Request{
		URL: &url.URL{
			RawQuery: "include_detail=true",
		},
	}

	params := parseCommonParams(c)
	if !params.includeDetail {
		t.Fatal("expected include_detail=true to enable detailed log loading")
	}
}

func TestParseCommonParamsIgnoresLegacyWithBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = &http.Request{
		URL: &url.URL{
			RawQuery: "with_body=true",
		},
	}

	params := parseCommonParams(c)
	if params.includeDetail {
		t.Fatal("expected legacy with_body to be ignored")
	}
}

func TestParseOperationalLogFilterAcceptsStatusAndChannels(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = &http.Request{URL: &url.URL{RawQuery: "status=rejected&channels=10,12"}}

	filter, err := parseOperationalLogFilter(c)
	if err != nil {
		t.Fatalf("parse operational filter: %v", err)
	}
	if filter.Status != model.OperationalStatusRejected {
		t.Fatalf("status = %q, want %q", filter.Status, model.OperationalStatusRejected)
	}
	if len(filter.ChannelIDs) != 2 || filter.ChannelIDs[0] != 10 || filter.ChannelIDs[1] != 12 {
		t.Fatalf("channel IDs = %v, want [10 12]", filter.ChannelIDs)
	}
}

func TestParseOperationalLogFilterRejectsInvalidValues(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []string{
		"status=unknown",
		"channels=10,invalid",
		"channels=0",
	}
	for _, rawQuery := range tests {
		t.Run(rawQuery, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = &http.Request{URL: &url.URL{RawQuery: rawQuery}}

			if _, err := parseOperationalLogFilter(c); err == nil {
				t.Fatalf("expected %q to be rejected", rawQuery)
			}
		})
	}
}
