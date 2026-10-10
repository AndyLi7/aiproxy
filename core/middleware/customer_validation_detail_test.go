package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/labring/aiproxy/core/model"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCustomerValidationDetailExcludesPrivateFields(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/images/generations", nil)
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetRequestBody(c.Request, []byte(`{"n":16,"size":"1536x1024","prompt":"private prompt","image":"https://private.example/token","api_key":"secret","model":"internal-name"}`))
	saveCustomerValidationDetail(c, &registryvalidation.ValidationError{Status: 400, Param: "n", Expected: map[string]any{"maximum": 15}})
	v, _ := c.Get(customerValidationDetailKey)
	d := v.(*model.RequestDetail)
	if d.RequestBody != `{"n":16,"prompt":"private prompt","size":"1536x1024"}` {
		t.Fatalf("unsafe summary: %s", d.RequestBody)
	}
	if !strings.Contains(d.ResponseBody, `"maximum":15`) {
		t.Fatal("missing expected constraint")
	}
	logs := captureOperationalLogs(t)
	if err := recordRejectedGatewayLog(c); err != nil {
		t.Fatal(err)
	}
	if len(*logs) != 1 || (*logs)[0].RequestDetail != d {
		t.Fatal("summary not attached to log")
	}
}
