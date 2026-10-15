//nolint:testpackage
package controller

import (
	"context"
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/labring/aiproxy/core/relay/adaptor/openai"
	relaycontroller "github.com/labring/aiproxy/core/relay/controller"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func installRelayExecution(t *testing.T, h *relayAdmissionHarness) {
	t.Helper()
	require.NoError(t, h.db.AutoMigrate(&model.ChannelAdmissionExecutionProtocol{}, &model.ChannelAdmissionExecution{}))
	require.NoError(t, h.db.Create(&model.ChannelAdmissionExecutionProtocol{ID: 1, Version: 1}).Error)
}
func relayExecutions(t *testing.T, h *relayAdmissionHarness) []model.ChannelAdmissionExecution {
	t.Helper()
	var rows []model.ChannelAdmissionExecution
	require.NoError(t, h.db.Order("started_at_ms,id").Find(&rows).Error)
	return rows
}
func TestRelayExecutionActualHTTPBodyRemainsActiveUntilReturn(t *testing.T) {
	for _, scenario := range []string{"stream", "truncated-stream", "cancelled-client", "json"} {
		t.Run(scenario, func(t *testing.T) {
			h := newRelayAdmissionHarness(t)
			installRelayExecution(t, h)
			h.stream = scenario != "json"
			headers := make(chan struct{})
			release := make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			h.responseHook = func(w http.ResponseWriter, r *http.Request) {
				if h.stream {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				close(headers)
				<-release
				if scenario == "truncated-stream" {
					_, _ = w.Write([]byte("data: {broken"))
					return
				}
				if h.stream {
					_, _ = w.Write([]byte("data: {\"id\":\"synthetic\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"index\":0}]}\n\ndata: [DONE]\n\n"))
				} else {
					_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"synthetic","messages":[{"role":"user","content":"hi"}],"stream":`+map[bool]string{true: "true", false: "false"}[h.stream]+`}`))
			c.Request.Header.Set("Content-Type", "application/json")
			mt := meta.NewMeta(h.channels[0], mode.ChatCompletions, "synthetic", model.ModelConfig{}, meta.WithBillingOperationID("bill"))
			done := make(chan *relaycontroller.HandleResult, 1)
			go func() { done <- relaycontroller.Handle(&openai.Adaptor{}, c, mt, AdaptorStore) }()
			select {
			case <-headers:
			case <-time.After(5 * time.Second):
				t.Fatal("headers not reached")
			}
			rows := relayExecutions(t, h)
			require.Len(t, rows, 1)
			require.Equal(t, "active", rows[0].State)
			require.Zero(t, rows[0].ReturnedAtMs)
			if scenario == "cancelled-client" {
				cancel()
			}
			h.seal(t)
			close(release)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("body did not return")
			}
			rows = relayExecutions(t, h)
			require.NotEqual(t, "active", rows[0].State)
			require.Equal(t, "unproven", rows[0].ContinuationState)
			var use model.ChannelAdmissionUse
			require.NoError(t, h.db.First(&use, "id = ?", rows[0].UseID).Error)
			require.Equal(t, "open", use.State)
		})
	}
}

type executionTestAdaptor struct {
	openai.Adaptor
	requestHook  func(*http.Request) (*http.Response, error)
	convertHook  func()
	responseHook func(*http.Response) (adaptor.DoResponseResult, adaptor.Error)
}

func (a *executionTestAdaptor) DoRequest(m *meta.Meta, s adaptor.Store, c *gin.Context, r *http.Request) (*http.Response, error) {
	if a.requestHook != nil {
		return a.requestHook(r)
	}
	return a.Adaptor.DoRequest(m, s, c, r)
}
func (a *executionTestAdaptor) ConvertRequest(m *meta.Meta, s adaptor.Store, r *http.Request) (adaptor.ConvertResult, error) {
	if a.convertHook != nil {
		a.convertHook()
	}
	return a.Adaptor.ConvertRequest(m, s, r)
}
func (a *executionTestAdaptor) DoResponse(m *meta.Meta, s adaptor.Store, c *gin.Context, r *http.Response) (adaptor.DoResponseResult, adaptor.Error) {
	if a.responseHook != nil {
		return a.responseHook(r)
	}
	return a.Adaptor.DoResponse(m, s, c, r)
}
func TestRelayExecutionNestedAndPanicDoNotHideActiveCalls(t *testing.T) {
	for _, scenario := range []string{"nested", "panic", "async"} {
		t.Run(scenario, func(t *testing.T) {
			h := newRelayAdmissionHarness(t)
			installRelayExecution(t, h)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"synthetic","messages":[]}`))
			c.Request.Header.Set("Content-Type", "application/json")
			mt := meta.NewMeta(h.channels[0], mode.ChatCompletions, "synthetic", model.ModelConfig{}, meta.WithBillingOperationID("bill"))
			a := &executionTestAdaptor{}
			a.convertHook = func() {
				rows := relayExecutions(t, h)
				require.Equal(t, "active", rows[0].State)
				if scenario == "panic" {
					panic("synthetic crash")
				}
				if scenario == "nested" {
					result, _ := h.call(t, 1, true, true, mode.ChatCompletions)
					require.Nil(t, result.Error)
					rows = relayExecutions(t, h)
					require.Len(t, rows, 2)
					var active int
					for _, r := range rows {
						if r.State == "active" {
							active++
						}
					}
					require.Equal(t, 1, active)
				}
			}
			backgroundStarted := make(chan struct{})
			backgroundRelease := make(chan struct{})
			backgroundDone := make(chan struct{})
			if scenario == "async" {
				defer func() { close(backgroundRelease); <-backgroundDone }()
			}
			if scenario == "async" {
				a.responseHook = func(resp *http.Response) (adaptor.DoResponseResult, adaptor.Error) {
					_, _ = io.Copy(io.Discard, resp.Body)
					go func() { close(backgroundStarted); <-backgroundRelease; close(backgroundDone) }()
					<-backgroundStarted
					return adaptor.DoResponseResult{AsyncUsage: true, UpstreamID: "synthetic-job"}, nil
				}
			}
			if scenario == "panic" {
				require.Panics(t, func() { relaycontroller.Handle(a, c, mt, AdaptorStore) })
			} else {
				result := relaycontroller.Handle(a, c, mt, AdaptorStore)
				require.Nil(t, result.Error)
			}
			if scenario == "async" {
				select {
				case <-backgroundDone:
					t.Fatal("synthetic background worker already ended")
				default:
				}
			}
			rows := relayExecutions(t, h)
			for _, row := range rows {
				require.Equal(t, "unproven", row.ContinuationState)
				if scenario == "panic" {
					require.Equal(t, "active", row.State)
				} else if scenario == "async" {
					require.Equal(t, "async_returned", row.State)
				} else {
					require.Equal(t, "returned", row.State)
				}
			}
		})
	}
}

type executionCloseBody struct {
	io.ReadCloser
	entered, release chan struct{}
	once             sync.Once
}

func (b *executionCloseBody) Close() error {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.ReadCloser.Close()
}
func TestRelayExecutionWaitsForResponseBodyClose(t *testing.T) {
	h := newRelayAdmissionHarness(t)
	installRelayExecution(t, h)
	body := &executionCloseBody{ReadCloser: io.NopCloser(strings.NewReader("synthetic")), entered: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-body.release:
		default:
			close(body.release)
		}
	}()
	a := &executionTestAdaptor{requestHook: func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: body}, nil
	}, responseHook: func(*http.Response) (adaptor.DoResponseResult, adaptor.Error) {
		return adaptor.DoResponseResult{AsyncUsage: true}, nil
	}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"synthetic","messages":[]}`))
	c.Request.Header.Set("Content-Type", "application/json")
	mt := meta.NewMeta(h.channels[0], mode.ChatCompletions, "synthetic", model.ModelConfig{}, meta.WithBillingOperationID("bill"))
	done := make(chan struct{})
	go func() { relaycontroller.Handle(a, c, mt, AdaptorStore); close(done) }()
	select {
	case <-body.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("body close not reached")
	}
	require.Equal(t, "active", relayExecutions(t, h)[0].State)
	select {
	case <-done:
		t.Fatal("returned before body cleanup")
	default:
	}
	close(body.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("return not recorded")
	}
	require.Equal(t, "async_returned", relayExecutions(t, h)[0].State)
}
func TestRelayExecutionPartialStorageBlocksAndLateWriteFailureRetains(t *testing.T) {
	for _, scenario := range []string{"partial", "wrong-version", "late-write-failure"} {
		t.Run(scenario, func(t *testing.T) {
			h := newRelayAdmissionHarness(t)
			installRelayExecution(t, h)
			switch scenario {
			case "partial":
				require.NoError(t, h.db.Migrator().DropTable(&model.ChannelAdmissionExecutionProtocol{}))
			case "wrong-version":
				require.NoError(t, h.db.Model(&model.ChannelAdmissionExecutionProtocol{}).Where("id = ?", 1).Update("version", 2).Error)
			case "late-write-failure":
				h.responseHook = func(w http.ResponseWriter, r *http.Request) {
					require.NoError(t, h.db.Exec("CREATE TRIGGER reject_execution_return BEFORE UPDATE ON channel_admission_executions BEGIN SELECT RAISE(ABORT, 'synthetic write failure'); END").Error)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
				}
			}
			result, _ := h.call(t, 0, false, false, mode.ChatCompletions)
			if scenario == "late-write-failure" {
				require.Nil(t, result.Error)
				require.Equal(t, int32(1), h.calls.Load())
				rows := relayExecutions(t, h)
				require.Len(t, rows, 1)
				require.Equal(t, "active", rows[0].State)
				require.Zero(t, rows[0].ReturnedAtMs)
			} else {
				require.NotNil(t, result.Error)
				require.Zero(t, h.calls.Load())
				require.Empty(t, relayExecutions(t, h))
			}
			var closed int64
			require.NoError(t, h.db.Model(&model.ChannelAdmissionUse{}).Where("state = ?", "closed").Count(&closed).Error)
			require.Zero(t, closed)
		})
	}
}
