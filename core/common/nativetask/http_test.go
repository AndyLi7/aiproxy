package nativetask

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func request(body []byte) *http.Request {
	r := httptest.NewRequest("POST", "/v1/model-tasks", strings.NewReader(string(body)))
	r.Header.Set("X-Request-Id", "req")
	return r
}
func TestNativeHTTPRetryAndPendingPrivacy(t *testing.T) {
	e, plan, w, p := setup(t)
	h := &HTTP{Engine: e, Identity: func(*http.Request) (string, int, error) { return "g", 1, nil }, ResolvePlan: func(*http.Request, []byte) (Plan, Provider, error) { return plan, p, nil }}
	w.fail = "admit"
	response := httptest.NewRecorder()
	h.Create(response, request(body))
	require.Equal(t, 503, response.Code)
	require.Zero(t, p.calls)
	w.fail = ""
	response = httptest.NewRecorder()
	h.Create(response, request(body))
	require.Equal(t, 202, response.Code)
	require.Equal(t, 1, p.calls)
	require.NotContains(t, response.Body.String(), "output")
	require.NotContains(t, response.Body.String(), "upstream-1")
	require.NotContains(t, response.Body.String(), "scope")
	h.ResolvePlan = func(*http.Request, []byte) (Plan, Provider, error) {
		t.Fatal("accepted replay must not resolve changed plan")
		return Plan{}, nil, nil
	}
	response = httptest.NewRecorder()
	h.Create(response, request(body))
	require.Equal(t, 202, response.Code)
	require.Equal(t, 1, p.calls)
	response = httptest.NewRecorder()
	h.Create(response, request([]byte(`{"model":"another","input":{}}`)))
	require.Equal(t, 409, response.Code)
	h.Identity = func(*http.Request) (string, int, error) { return "", 0, errors.New("missing auth") }
	response = httptest.NewRecorder()
	h.Create(response, request(body))
	require.Equal(t, 401, response.Code)
}
func TestNativeHTTPArtifactOwnershipAndIntegrity(t *testing.T) {
	e, plan, _, p := setup(t)
	ctx := context.Background()
	var c nativeresult.TaskContract
	require.NoError(t, json.Unmarshal(plan.Contract, &c))
	c.Artifacts = []nativeresult.ArtifactBinding{{Path: []string{"file", "url"}}}
	plan.Contract, _ = json.Marshal(c)
	plan.DeliveryBase = "https://gateway.example"
	_, err := e.Submit(ctx, "req", "g", 1, body, plan)
	require.NoError(t, err)
	require.NoError(t, model.SaveNativeTaskResult(e.DB, "req", "g", 1, []byte(`{"file":{"url":"https://provider.example/file.svg"}}`)))
	svg := `<svg><script>unsafe()</script></svg>`
	digest := sha256.Sum256([]byte(svg))
	key := "native-results/req/0/" + strings.Repeat("a", 64) + ".bin"
	_, err = e.Deliver(ctx, "req", "g", 1, func(context.Context, string, int, string) (ownedartifact.Receipt, error) {
		return ownedartifact.Receipt{Key: key, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(svg))}, nil
	})
	require.NoError(t, err)
	downloads := 0
	corrupt := false
	h := &HTTP{Engine: e, Identity: func(*http.Request) (string, int, error) { return "g", 1, nil }, Download: func(_ *http.Request, k string) (*http.Response, error) {
		downloads++
		require.Equal(t, key, k)
		b := svg
		if corrupt {
			b += "bad"
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(b)), Header: http.Header{"Content-Type": []string{"image/svg+xml"}}}, nil
	}}
	r := httptest.NewRequest("GET", "/v1/model-tasks/req/artifacts/0", nil)
	response := httptest.NewRecorder()
	h.GetArtifact(response, r, "req", "0")
	require.Equal(t, 200, response.Code)
	require.Equal(t, svg, response.Body.String())
	require.Equal(t, "application/octet-stream", response.Header().Get("Content-Type"))
	require.Equal(t, "nosniff", response.Header().Get("X-Content-Type-Options"))
	require.Contains(t, response.Header().Get("Content-Disposition"), "attachment")
	corrupt = true
	response = httptest.NewRecorder()
	h.GetArtifact(response, r, "req", "0")
	require.Equal(t, 503, response.Code)
	require.NotContains(t, response.Body.String(), "<svg")
	h.Identity = func(*http.Request) (string, int, error) { return "g", 2, nil }
	response = httptest.NewRecorder()
	h.GetArtifact(response, r, "req", "0")
	require.Equal(t, 404, response.Code)
	require.Equal(t, 2, downloads)
	require.Equal(t, 1, p.calls)
}
func TestNativePublicNullIsCompletedNotPending(t *testing.T) {
	r := httptest.NewRecorder()
	WritePublic(r, 200, &model.NativeTask{ID: "t", Model: "m", Status: "completed", DeliveredOutput: "null", NativeOutput: `{"private":true}`, UpstreamID: "secret"})
	require.Contains(t, r.Body.String(), `"output":null`)
	require.NotContains(t, r.Body.String(), "secret")
	require.NotContains(t, r.Body.String(), "private")
}
