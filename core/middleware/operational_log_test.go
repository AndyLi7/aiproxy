//nolint:testpackage
package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
)

func captureOperationalLogs(t *testing.T) *[]*model.Log {
	t.Helper()

	original := recordOperationalLog
	logs := make([]*model.Log, 0, 1)
	recordOperationalLog = func(entry *model.Log) error {
		copy := *entry
		logs = append(logs, &copy)
		return nil
	}
	t.Cleanup(func() {
		recordOperationalLog = original
	})

	return &logs
}

func TestOperationalLogMiddlewareRecordsRejectedRequestOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logs := captureOperationalLogs(t)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		SetRequestID(c, "req_rejected_123")
		SetRequestAt(c, time.Unix(10, 0))
		c.Next()
	})
	router.Use(OperationalLogMiddleware())
	router.POST("/v1/videos", func(c *gin.Context) {
		SetFailureStage(c, model.FailureStageBalance, "Bearer top-secret\ninsufficient balance")
		c.JSON(http.StatusPaymentRequired, gin.H{"error": "rejected"})
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"prompt":"private prompt"}`))
	request.Header.Set(OperationalLogSourceHeader, model.RequestSourcePlayground)
	request.Header.Set("Authorization", "Bearer top-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if len(*logs) != 1 {
		t.Fatalf("recorded %d logs, want 1", len(*logs))
	}
	entry := (*logs)[0]
	if entry.RequestID != "req_rejected_123" {
		t.Fatalf("request ID = %q, want req_rejected_123", entry.RequestID)
	}
	if entry.RequestSource != model.RequestSourcePlayground {
		t.Fatalf("request source = %q, want playground", entry.RequestSource)
	}
	if entry.FailureStage != model.FailureStageBalance {
		t.Fatalf("failure stage = %q, want balance", entry.FailureStage)
	}
	if entry.ErrorCode != "insufficient_balance" {
		t.Fatalf("error code = %q, want insufficient_balance", entry.ErrorCode)
	}
	if strings.Contains(entry.SafeError, "top-secret") || strings.Contains(string(entry.Content), "private prompt") {
		t.Fatalf("operational log leaked a credential or request body: %+v", entry)
	}
}

func TestOperationalLogMiddlewareDoesNotDuplicateNormalResult(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logs := captureOperationalLogs(t)

	router := gin.New()
	router.Use(OperationalLogMiddleware())
	router.GET("/v1/test", func(c *gin.Context) {
		MarkOperationalLogRecorded(c)
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream"})
	})

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/test", nil))
	if len(*logs) != 0 {
		t.Fatalf("fallback recorded %d duplicate logs, want 0", len(*logs))
	}
}

func TestOperationalLogMiddlewareDefaultsSourceToAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logs := captureOperationalLogs(t)

	router := gin.New()
	router.Use(OperationalLogMiddleware())
	router.GET("/v1/test", func(c *gin.Context) {
		SetFailureStage(c, model.FailureStageAuth, "invalid key")
		c.Status(http.StatusUnauthorized)
	})

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/test", nil))
	if len(*logs) != 1 || (*logs)[0].RequestSource != model.RequestSourceAPI {
		t.Fatalf("logs = %+v, want one API-source log", *logs)
	}
}

func TestNewMetaByContextCarriesOperationalSource(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	c.Request.Header.Set(OperationalLogSourceHeader, model.RequestSourcePlayground)
	c.Set(Group, model.GroupCache{ID: "group-1"})
	c.Set(Token, model.TokenCache{ID: 2, Name: "playground"})
	c.Set(RequestModel, "video-model")
	c.Set(ModelConfig, model.ModelConfig{})
	SetRequestAt(c, time.Unix(10, 0))

	requestMeta := NewMetaByContext(c, nil, mode.Videos)
	if requestMeta.OperationalFields.RequestSource != model.RequestSourcePlayground {
		t.Fatalf(
			"request source = %q, want playground",
			requestMeta.OperationalFields.RequestSource,
		)
	}
}

func TestAdminDemoSourceRequiresAnInternalGroup(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, test := range []struct {
		name       string
		group      model.GroupCache
		wantSource string
	}{
		{
			name:       "customer token cannot spoof admin demo",
			group:      model.GroupCache{ID: "customer-group", Status: model.GroupStatusEnabled},
			wantSource: model.RequestSourceAPI,
		},
		{
			name:       "internal token can mark admin demo",
			group:      model.GroupCache{ID: "internal-group", Status: model.GroupStatusInternal},
			wantSource: model.RequestSourceAdminDemo,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
			c.Request.Header.Set(OperationalLogSourceHeader, model.RequestSourceAdminDemo)
			c.Set(Group, test.group)

			fields := OperationalFieldsFromContext(c)
			if fields.RequestSource != test.wantSource {
				t.Fatalf("request source = %q, want %q", fields.RequestSource, test.wantSource)
			}
		})
	}
}

func TestMaskOperationalIPRemovesHostBits(t *testing.T) {
	if got := maskOperationalIP("203.0.113.91"); got != "203.0.113.0" {
		t.Fatalf("masked IPv4 = %q, want 203.0.113.0", got)
	}
	if got := maskOperationalIP("2001:db8:1234:5678:abcd::1"); got != "2001:db8:1234:5678::" {
		t.Fatalf("masked IPv6 = %q, want 2001:db8:1234:5678::", got)
	}
}
