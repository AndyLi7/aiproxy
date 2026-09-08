package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/requesttrace"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/labring/aiproxy/core/trace"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestRequestTraceCaptureRealGatewayRoutes(t *testing.T) {
	tests := []struct {
		name, path string
		body       func() (*bytes.Buffer, string)
		wantStages []requesttrace.Stage
	}{
		{
			name: "image generation", path: "/v1/images/generations",
			body: func() (*bytes.Buffer, string) {
				return bytes.NewBufferString(`{"model":"trace-image","prompt":"private prompt"}`), "application/json"
			},
			wantStages: []requesttrace.Stage{requesttrace.StageRequest, requesttrace.StageAuthentication, requesttrace.StageBalanceCheck, requesttrace.StageModelResolution, requesttrace.StageValidation, requesttrace.StageChannelSelection, requesttrace.StageUpstreamAttempt},
		},
		{
			name: "image edit", path: "/v1/images/edits",
			body: func() (*bytes.Buffer, string) {
				var body bytes.Buffer
				writer := multipart.NewWriter(&body)
				require.NoError(t, writer.WriteField("model", "trace-image"))
				require.NoError(t, writer.WriteField("prompt", "private edit prompt"))
				part, err := writer.CreateFormFile("image", "input.png")
				require.NoError(t, err)
				_, err = part.Write([]byte("not-a-real-image"))
				require.NoError(t, err)
				require.NoError(t, writer.Close())
				return &body, writer.FormDataContentType()
			},
			wantStages: []requesttrace.Stage{requesttrace.StageRequest, requesttrace.StageAuthentication, requesttrace.StageBalanceCheck, requesttrace.StageModelResolution, requesttrace.StageValidation, requesttrace.StageChannelSelection, requesttrace.StageUpstreamAttempt},
		},
		{
			name: "video submission", path: "/v1/videos",
			body: func() (*bytes.Buffer, string) {
				return bytes.NewBufferString(`{"model":"trace-video","capability":"text-to-video","prompt":"private video prompt","size":"480x480","seconds":4}`), "application/json"
			},
			wantStages: []requesttrace.Stage{requesttrace.StageRequest, requesttrace.StageAuthentication, requesttrace.StageBalanceCheck, requesttrace.StageModelResolution, requesttrace.StageValidation, requesttrace.StageChannelSelection, requesttrace.StageUpstreamAttempt},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newTraceCaptureFixture(t)
			requestID := "trace-capture-" + strings.ReplaceAll(tt.name, " ", "-")
			body, contentType := tt.body()
			request := httptest.NewRequest(http.MethodPost, tt.path, body)
			request.Header.Set("Authorization", "Bearer trace-test-key")
			request.Header.Set(middleware.RequestIDHeader, requestID)
			request.Header.Set("Content-Type", contentType)
			response := httptest.NewRecorder()
			fixture.engine.ServeHTTP(response, request)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			fixture.awaitConsume(t, requestID)
			fixture.flush(t)

			page, err := fixture.store.FindRequests(t.Context(), model.TraceRequestQuery{GroupID: "group-a", RequestID: requestID})
			require.NoError(t, err)
			require.Len(t, page.Items, 1)
			spans, err := fixture.store.List(t.Context(), model.TraceQuery{GroupID: "group-a", TraceID: page.Items[0].TraceID, Service: requesttrace.ServiceAIProxy, Limit: 100})
			require.NoError(t, err)
			require.Len(t, spans.Items, len(tt.wantStages))
			for _, span := range spans.Items {
				attributes, err := json.Marshal(span.Attributes)
				require.NoError(t, err)
				require.NotContains(t, string(attributes), "private")
				require.Equal(t, requesttrace.StatusSuccess, span.Status)
			}
			assertExactTraceLifecycle(t, spans.Items, tt.wantStages)
			if tt.name == "video submission" {
				for _, span := range spans.Items {
					require.NotEqual(t, requesttrace.StageAsyncObservedResult, span.Stage)
				}
			}
			require.Equal(t, int32(1), fixture.providerCalls.Load())
		})
	}
}

func assertExactTraceLifecycle(t *testing.T, spans []requesttrace.Span, stages []requesttrace.Stage) {
	t.Helper()
	byStage := make(map[requesttrace.Stage]requesttrace.Span, len(spans))
	for _, span := range spans {
		_, duplicate := byStage[span.Stage]
		require.False(t, duplicate, "duplicate terminal stage %s", span.Stage)
		byStage[span.Stage] = span
	}
	require.Len(t, byStage, len(stages))
	root, ok := byStage[requesttrace.StageRequest]
	require.True(t, ok, "missing request root")
	require.Empty(t, root.ParentSpanID)
	require.NotNil(t, root.EndedAt)
	var previous *requesttrace.Span
	for _, stage := range stages[1:] {
		span, ok := byStage[stage]
		require.True(t, ok, "missing stage %s", stage)
		require.Equal(t, root.SpanID, span.ParentSpanID)
		require.NotNil(t, span.EndedAt)
		require.False(t, span.StartedAt.Before(root.StartedAt))
		require.False(t, root.EndedAt.Before(*span.EndedAt))
		if previous != nil {
			require.False(t, span.StartedAt.Before(previous.StartedAt), "%s started before %s", stage, previous.Stage)
		}
		copy := span
		previous = &copy
	}
}

func assertExactModelTraceLifecycle(t *testing.T, spans []model.RequestTraceSpan, stages []requesttrace.Stage, statuses []requesttrace.Status) {
	t.Helper()
	require.Len(t, stages, len(statuses))
	converted := make([]requesttrace.Span, 0, len(spans))
	statusByStage := make(map[requesttrace.Stage]requesttrace.Status, len(spans))
	for _, span := range spans {
		converted = append(converted, requesttrace.Span{SpanID: span.SpanID, ParentSpanID: span.ParentSpanID, Stage: span.Stage, Status: span.Status, StartedAt: span.StartedAt, EndedAt: span.EndedAt})
		statusByStage[span.Stage] = span.Status
	}
	assertExactTraceLifecycle(t, converted, stages)
	for index, stage := range stages {
		require.Equal(t, statuses[index], statusByStage[stage], "stage %s", stage)
	}
}

func TestRequestTraceCaptureStopsBeforeUpstreamOnGatewayRejections(t *testing.T) {
	tests := []struct {
		name         string
		prepare      func(*testing.T, *traceCaptureFixture)
		authorized   bool
		body         string
		wantStatus   int
		wantStages   []requesttrace.Stage
		wantStatuses []requesttrace.Status
	}{
		{name: "authentication rejection", body: `{"model":"trace-image"}`, wantStatus: http.StatusUnauthorized, wantStages: []requesttrace.Stage{requesttrace.StageRequest, requesttrace.StageAuthentication}, wantStatuses: []requesttrace.Status{requesttrace.StatusError, requesttrace.StatusError}},
		{name: "parameter rejection", authorized: true, body: `{}`, wantStatus: http.StatusBadRequest, wantStages: []requesttrace.Stage{requesttrace.StageRequest, requesttrace.StageAuthentication, requesttrace.StageBalanceCheck, requesttrace.StageModelResolution}, wantStatuses: []requesttrace.Status{requesttrace.StatusError, requesttrace.StatusSuccess, requesttrace.StatusSuccess, requesttrace.StatusError}},
		{
			name: "channel unavailable", authorized: true, body: `{"model":"trace-image","prompt":"safe"}`, wantStatus: http.StatusNotFound,
			prepare: func(t *testing.T, _ *traceCaptureFixture) {
				require.NoError(t, model.DB.Model(&model.Channel{}).Where("type = ?", model.ChannelTypeOpenAI).Update("status", model.ChannelStatusDisabled).Error)
				require.NoError(t, model.InitModelConfigAndChannelCache())
			},
			wantStages: []requesttrace.Stage{requesttrace.StageRequest, requesttrace.StageAuthentication, requesttrace.StageBalanceCheck, requesttrace.StageModelResolution}, wantStatuses: []requesttrace.Status{requesttrace.StatusError, requesttrace.StatusSuccess, requesttrace.StatusSuccess, requesttrace.StatusError},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newTraceCaptureFixture(t)
			if tt.prepare != nil {
				tt.prepare(t, fixture)
			}
			requestID := "trace-reject-" + strings.ReplaceAll(tt.name, " ", "-")
			request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(tt.body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(middleware.RequestIDHeader, requestID)
			if tt.authorized {
				request.Header.Set("Authorization", "Bearer trace-test-key")
			}
			response := httptest.NewRecorder()
			fixture.engine.ServeHTTP(response, request)
			require.Equal(t, tt.wantStatus, response.Code, response.Body.String())
			require.Equal(t, int32(0), fixture.providerCalls.Load())
			fixture.flush(t)
			var spans []model.RequestTraceSpan
			require.NoError(t, model.LogDB.Where("request_id = ?", requestID).Find(&spans).Error)
			require.Len(t, spans, len(tt.wantStages))
			assertExactModelTraceLifecycle(t, spans, tt.wantStages, tt.wantStatuses)
		})
	}
}

func TestRequestTraceCapturePreservesTwoRealRelayAttempts(t *testing.T) {
	fixture := newTraceCaptureFixture(t)
	fixture.failFirst.Store(true)
	require.NoError(t, model.DB.Exec("UPDATE model_configs SET retry_times = ? WHERE model = ?", 1, "trace-image").Error)
	require.NoError(t, model.InitModelConfigAndChannelCache())

	requestID := "trace-capture-retry"
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"trace-image","prompt":"retry privately"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer trace-test-key")
	request.Header.Set(middleware.RequestIDHeader, requestID)
	response := httptest.NewRecorder()
	fixture.engine.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, int32(2), fixture.providerCalls.Load())
	fixture.awaitConsume(t, requestID)
	fixture.flush(t)

	page, err := fixture.store.FindRequests(t.Context(), model.TraceRequestQuery{GroupID: "group-a", RequestID: requestID})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	spans, err := fixture.store.List(t.Context(), model.TraceQuery{GroupID: "group-a", TraceID: page.Items[0].TraceID, Service: requesttrace.ServiceAIProxy, Limit: 100})
	require.NoError(t, err)
	var attempts []requesttrace.Span
	for _, span := range spans.Items {
		if span.Stage == requesttrace.StageUpstreamAttempt && span.Status != requesttrace.StatusRunning {
			attempts = append(attempts, span)
		}
	}
	require.Len(t, attempts, 2)
	statusByAttempt := make(map[int]requesttrace.Status, 2)
	for _, attempt := range attempts {
		require.NotNil(t, attempt.Attributes.Attempt)
		statusByAttempt[*attempt.Attributes.Attempt] = attempt.Status
	}
	require.Equal(t, map[int]requesttrace.Status{1: requesttrace.StatusError, 2: requesttrace.StatusSuccess}, statusByAttempt)
}

func TestRequestTraceEnabledAndDisabledPreserveGatewayAndConsumption(t *testing.T) {
	type outcome struct {
		code, calls int
		body        string
		consumes    int64
	}
	run := func(t *testing.T, enabled bool) outcome {
		fixture := newTraceCaptureFixture(t)
		if !enabled {
			restore := trace.Install(nil)
			t.Cleanup(restore)
		}
		requestID := "trace-equivalence"
		request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"trace-image","prompt":"same"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer trace-test-key")
		request.Header.Set(middleware.RequestIDHeader, requestID)
		response := httptest.NewRecorder()
		fixture.engine.ServeHTTP(response, request)
		fixture.awaitConsume(t, requestID)
		var count int64
		require.NoError(t, model.LogDB.Model(&model.Log{}).Where("request_id = ?", requestID).Count(&count).Error)
		fixture.flush(t)
		return outcome{code: response.Code, calls: int(fixture.providerCalls.Load()), body: response.Body.String(), consumes: count}
	}
	enabled := run(t, true)
	disabled := run(t, false)
	require.Equal(t, enabled, disabled)
}

func TestRequestTraceBlockedAndFullQueueDoNotChangeGatewayResult(t *testing.T) {
	baseline := newTraceCaptureFixture(t)
	baselineRequestID := "trace-unblocked-baseline"
	baselineRequest := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"trace-image","prompt":"unaffected"}`))
	baselineRequest.Header.Set("Content-Type", "application/json")
	baselineRequest.Header.Set("Authorization", "Bearer trace-test-key")
	baselineRequest.Header.Set(middleware.RequestIDHeader, baselineRequestID)
	baselineResponse := httptest.NewRecorder()
	baseline.engine.ServeHTTP(baselineResponse, baselineRequest)
	baseline.awaitConsume(t, baselineRequestID)
	var baselineConsumes int64
	require.NoError(t, model.LogDB.Model(&model.Log{}).Where("request_id = ?", baselineRequestID).Count(&baselineConsumes).Error)
	baselineCalls := baseline.providerCalls.Load()
	baseline.flush(t)

	fixture := newTraceCaptureFixture(t)
	blocked := make(chan struct{})
	require.NoError(t, model.LogDB.Callback().Create().Before("gorm:create").Register("test:block_trace_writes", func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "request_trace_spans" {
			<-blocked
		}
	}))
	t.Cleanup(func() { _ = model.LogDB.Callback().Create().Remove("test:block_trace_writes") })
	for i := 0; i < 5000; i++ {
		session := fixture.runtime.NewRequest(fmt.Sprintf("queue-%d", i))
		require.NotNil(t, session)
		_ = session.Finish(requesttrace.StatusSuccess)
	}
	require.Greater(t, fixture.runtime.Health().Writer.Dropped, uint64(0))

	requestID := "trace-blocked-full"
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"trace-image","prompt":"unaffected"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer trace-test-key")
	request.Header.Set(middleware.RequestIDHeader, requestID)
	response := httptest.NewRecorder()
	fixture.engine.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, int32(1), fixture.providerCalls.Load())
	close(blocked)
	fixture.awaitConsume(t, requestID)
	var blockedConsumes int64
	require.NoError(t, model.LogDB.Model(&model.Log{}).Where("request_id = ?", requestID).Count(&blockedConsumes).Error)
	fixture.flush(t)
	require.Equal(t, baselineResponse.Code, response.Code)
	require.JSONEq(t, baselineResponse.Body.String(), response.Body.String())
	require.Equal(t, baselineCalls, fixture.providerCalls.Load())
	require.Equal(t, baselineConsumes, blockedConsumes)
}

func TestRequestTraceRejectsClientOwnershipAndLeaksNoSensitiveTraceData(t *testing.T) {
	fixture := newTraceCaptureFixture(t)
	fixture.failFirst.Store(true)
	require.NoError(t, model.DB.Exec("UPDATE model_configs SET retry_times = ? WHERE model = ?", 1, "trace-image").Error)
	require.NoError(t, model.InitModelConfigAndChannelCache())
	requestID := "shared-malicious-request"
	for _, key := range []string{"trace-test-key", "trace-other-key"} {
		request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"trace-image","prompt":"PROMPT_SENTINEL"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+key)
		request.Header.Set(middleware.RequestIDHeader, requestID)
		request.Header.Set("Group", "group-a")
		request.Header.Set("X-Group-ID", "group-a")
		request.Header.Set("traceparent", "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01")
		request.Header.Set("X-Trace-ID", "cccccccccccccccccccccccccccccccc")
		request.Header.Set("X-Trace-Source", "SOURCE_SENTINEL")
		response := httptest.NewRecorder()
		fixture.engine.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	}
	require.Equal(t, int32(3), fixture.providerCalls.Load(), "the injected raw upstream error must be followed by one successful retry")
	require.Eventually(t, func() bool {
		var count int64
		return model.LogDB.Model(&model.Log{}).Where("request_id = ?", requestID).Count(&count).Error == nil && count == 2
	}, 3*time.Second, 10*time.Millisecond)
	fixture.flush(t)

	var traceIDs []string
	for _, groupID := range []string{"group-a", "group-b"} {
		page, err := fixture.store.FindRequests(t.Context(), model.TraceRequestQuery{GroupID: groupID, RequestID: requestID})
		require.NoError(t, err)
		require.Len(t, page.Items, 1)
		traceIDs = append(traceIDs, page.Items[0].TraceID)
	}
	require.NotEqual(t, traceIDs[0], traceIDs[1])

	var heads []model.RequestTraceHead
	var spans []model.RequestTraceSpan
	require.NoError(t, model.LogDB.Find(&heads).Error)
	require.NoError(t, model.LogDB.Find(&spans).Error)
	raw, err := json.Marshal(struct {
		Heads []model.RequestTraceHead
		Spans []model.RequestTraceSpan
	}{heads, spans})
	require.NoError(t, err)
	for _, forbidden := range []string{"PROMPT_SENTINEL", "SOURCE_SENTINEL", "provider-secret", "private upstream failure", "provider.invalid/private.png", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "cccccccccccccccccccccccccccccccc"} {
		require.NotContains(t, string(raw), forbidden)
	}

	apiRequest := httptest.NewRequest(http.MethodGet, "/api/trace/group-a/"+traceIDs[0]+"?service=aiproxy", nil)
	apiRequest.Header.Set("Authorization", "Bearer trace-admin-key")
	apiResponse := httptest.NewRecorder()
	fixture.engine.ServeHTTP(apiResponse, apiRequest)
	require.Equal(t, http.StatusOK, apiResponse.Code, apiResponse.Body.String())
	for _, forbidden := range []string{"PROMPT_SENTINEL", "SOURCE_SENTINEL", "provider-secret", "private upstream failure", "provider.invalid/private.png", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "cccccccccccccccccccccccccccccccc"} {
		require.NotContains(t, apiResponse.Body.String(), forbidden)
	}
}

type traceCaptureFixture struct {
	engine        *gin.Engine
	store         *model.TraceStore
	runtime       *trace.Runtime
	providerCalls atomic.Int32
	failFirst     atomic.Bool
}

func newTraceCaptureFixture(t *testing.T) *traceCaptureFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	f := &traceCaptureFixture{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := f.providerCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if f.failFirst.Load() && call == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"private upstream failure","type":"server_error","code":"server_error"}}`))
			return
		}
		if r.URL.Path == "/videos" {
			_, _ = w.Write([]byte(`{"id":"video_trace","object":"video","status":"queued","model":"trace-video","created_at":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"created":1,"data":[{"url":"https://provider.invalid/private.png"}]}`))
	}))
	t.Cleanup(provider.Close)

	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "trace-capture.db"))
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	oldDB, oldLogDB, oldRedis, oldSQLite := model.DB, model.LogDB, common.RedisEnabled, common.UsingSQLite
	oldAdminKey := config.AdminKey
	model.DB, model.LogDB, common.RedisEnabled, common.UsingSQLite = db, db, false, true
	config.AdminKey = "trace-admin-key"
	t.Cleanup(func() {
		model.DB, model.LogDB, common.RedisEnabled, common.UsingSQLite = oldDB, oldLogDB, oldRedis, oldSQLite
		config.AdminKey = oldAdminKey
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.ModelConfig{}, &model.Channel{}, &model.Log{}, &model.StoreV2{}, &model.AsyncUsageInfo{}))
	require.NoError(t, db.Create(&[]model.ModelConfig{
		{Model: "trace-image", Type: mode.ImagesGenerations},
		{
			Model: "trace-video::text-to-video", Type: mode.Videos,
			Config: map[model.ModelConfigKey]any{
				"capability_contract_version": model.ModelCapabilityContractVersion,
				"public_model":                "trace-video", "capability": "text-to-video",
			},
		},
	}).Error)
	require.NoError(t, db.Create(&[]model.Channel{
		{Name: "trace-image", Status: model.ChannelStatusEnabled, Type: model.ChannelTypeOpenAI, Key: "provider-secret", BaseURL: provider.URL, Models: []string{"trace-image"}},
		{Name: "trace-image-retry", Status: model.ChannelStatusEnabled, Type: model.ChannelTypeOpenAI, Key: "provider-secret", BaseURL: provider.URL, Models: []string{"trace-image"}},
		{Name: "trace-video", Status: model.ChannelStatusEnabled, Type: model.ChannelTypeOpenAI, Key: "provider-secret", BaseURL: provider.URL, Models: []string{"trace-video::text-to-video"}},
	}).Error)
	require.NoError(t, model.InitModelConfigAndChannelCache())
	require.NoError(t, model.CacheSetGroup(&model.GroupCache{ID: "group-a", Status: model.GroupStatusInternal}))
	require.NoError(t, model.CacheSetGroup(&model.GroupCache{ID: "group-b", Status: model.GroupStatusInternal}))
	require.NoError(t, model.CacheSetToken(&model.TokenCache{ID: 71, Key: "trace-test-key", Group: "group-a", Status: model.TokenStatusEnabled}))
	require.NoError(t, model.CacheSetToken(&model.TokenCache{ID: 72, Key: "trace-other-key", Group: "group-b", Status: model.TokenStatusEnabled}))
	t.Cleanup(func() {
		_ = model.CacheDeleteToken("trace-test-key")
		_ = model.CacheDeleteToken("trace-other-key")
		_ = model.CacheDeleteGroup("group-a")
		_ = model.CacheDeleteGroup("group-b")
	})

	f.store = model.NewTraceStore(db)
	f.runtime = trace.Start(t.Context(), db, trace.Options{Enabled: true})
	require.True(t, f.runtime.Health().Ready)
	restore := trace.Install(f.runtime)
	t.Cleanup(restore)
	f.engine = gin.New()
	f.engine.Use(middleware.RequestIDMiddleware)
	SetRelayRouter(f.engine)
	SetAPIRouter(f.engine)
	return f
}

func (f *traceCaptureFixture) flush(t *testing.T) {
	t.Helper()
	require.NoError(t, f.runtime.Close(context.Background()))
}

func (f *traceCaptureFixture) awaitConsume(t *testing.T, requestID string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var count int64
		return model.LogDB.Model(&model.Log{}).
			Where("request_id = ?", requestID).
			Count(&count).Error == nil && count == 1
	}, 3*time.Second, 10*time.Millisecond)
}
