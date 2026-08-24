//nolint:testpackage
package controller

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
)

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
