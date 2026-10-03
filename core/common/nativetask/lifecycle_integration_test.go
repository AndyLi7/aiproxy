package nativetask

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor/fal"
	"github.com/stretchr/testify/require"
)

type nativeTransport func(*http.Request) (*http.Response, error)

func (f nativeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Real fal protocol adapter + public HTTP handlers + reopened durable SQLite.
// Provider transport, wallet and archive boundary are isolated fakes: no network,
// paid generation or claim of live provider/billing/storage verification.
func TestNativeLifecycleProtocolRestartAndOwnedDownload(t *testing.T) {
	for _, family := range []struct {
		name, output string
		files        bool
	}{
		{"svg-zip", `{"files":[{"url":"https://provider.example/a.svg"},{"url":"https://provider.example/b.zip"}],"seed":9007199254740993}`, true},
		{"ocr", `{"results":{"quad_boxes":[],"text":"你好"},"seed":9007199254740993}`, false},
		{"nullable", "null", false},
	} {
		t.Run(family.name, func(t *testing.T) {
			e, plan, w, _ := setup(t)
			var contract nativeresult.TaskContract
			require.NoError(t, json.Unmarshal(plan.Contract, &contract))
			switch family.name {
			case "svg-zip":
				contract.OutputSchema = json.RawMessage(`{"type":"object","required":["files","seed"],"additionalProperties":false,"properties":{"seed":{"type":"integer"},"files":{"type":"array","minItems":2,"maxItems":2,"items":{"type":"object","required":["url"],"additionalProperties":false,"properties":{"url":{"type":"string"}}}}}}`)
			case "ocr":
				contract.OutputSchema = json.RawMessage(`{"type":"object","required":["results","seed"],"additionalProperties":false,"properties":{"seed":{"type":"integer"},"results":{"type":"object","required":["quad_boxes","text"],"additionalProperties":false,"properties":{"quad_boxes":{"type":"array","items":{"type":"array","items":{"type":"number"}}},"text":{"type":"string"}}}}}`)
			case "nullable":
				contract.OutputSchema = json.RawMessage(`{"type":"null"}`)
			}
			if family.files {
				contract.Artifacts = []nativeresult.ArtifactBinding{{Path: []string{"files", "*", "url"}}}
			}
			plan.Contract, _ = json.Marshal(contract)
			plan.DeliveryBase = "https://gateway.example"
			posts, polls := 0, 0
			client := &fal.Client{Key: "test-native-key", HTTP: &http.Client{Transport: nativeTransport(func(r *http.Request) (*http.Response, error) {
				require.Equal(t, "queue.fal.run", r.URL.Host)
				require.Equal(t, "Key test-native-key", r.Header.Get("Authorization"))
				output := ""
				switch r.URL.Path {
				case "/fal-ai/vector/model":
					require.Equal(t, "POST", r.Method)
					posts++
					input, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					require.Contains(t, string(input), "9007199254740993")
					output = `{"request_id":"accepted-1"}`
				case "/fal-ai/vector/requests/accepted-1/status":
					polls++
					output = `{"status":"COMPLETED"}`
				case "/fal-ai/vector/requests/accepted-1":
					output = family.output
				default:
					t.Fatalf("unexpected protocol path %s", r.URL.Path)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(output))}, nil
			})}}
			owner, token := "g", 1
			root := t.TempDir()
			archiveCalls := map[int]int{}
			archive := func(_ context.Context, id string, index int, source string) (ownedartifact.Receipt, error) {
				archiveCalls[index]++
				data := []byte("<svg>isolated</svg>")
				if index == 1 {
					data = []byte{'P', 'K', 3, 4, 0, 1}
				}
				require.Equal(t, []string{"https://provider.example/a.svg", "https://provider.example/b.zip"}[index], source)
				sum := sha256.Sum256(data)
				digest := hex.EncodeToString(sum[:])
				key := fmt.Sprintf("native-results/%s/%d/%s.bin", id, index, digest)
				file := filepath.Join(root, key)
				require.NoError(t, os.MkdirAll(filepath.Dir(file), 0700))
				require.NoError(t, os.WriteFile(file, data, 0600))
				return ownedartifact.Receipt{Key: key, SHA256: digest, Size: int64(len(data))}, nil
			}
			h := &HTTP{Engine: e, Identity: func(*http.Request) (string, int, error) { return owner, token, nil }, ResolvePlan: func(*http.Request, []byte) (Plan, Provider, error) { return plan, client, nil }, ResolvePoller: func(_ context.Context, task *model.NativeTask) (Poller, error) {
				require.Equal(t, "accepted-1", task.UpstreamID)
				return client, nil
			}, Archive: archive,
				Download: func(_ *http.Request, key string) (*http.Response, error) {
					data, err := os.ReadFile(filepath.Join(root, key))
					return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data))}, err
				}}
			submit := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest("POST", "/v1/model-tasks", bytes.NewReader(body))
				r.Header.Set("X-Request-Id", "req")
				out := httptest.NewRecorder()
				h.Create(out, r)
				return out
			}
			created := submit()
			require.Equal(t, 202, created.Code, created.Body.String())
			require.NotContains(t, created.Body.String(), "output")
			// Real connection shutdown/reopen, not just a new Engine pointer.
			var databases []struct{ File string }
			require.NoError(t, e.DB.Raw("PRAGMA database_list").Scan(&databases).Error)
			conn, err := e.DB.DB()
			require.NoError(t, err)
			require.NoError(t, conn.Close())
			reopened, err := model.OpenSQLite(databases[0].File)
			require.NoError(t, err)
			e = &Engine{DB: reopened, Wallet: w}
			h.Engine = e
			t.Cleanup(func() { conn, _ := reopened.DB(); _ = conn.Close() })
			// A customer read never polls the provider or archives anything.
			queued := httptest.NewRecorder()
			h.Get(queued, httptest.NewRequest("GET", "/", nil), "req")
			require.Equal(t, 200, queued.Code)
			require.Contains(t, queued.Body.String(), `"status":"queued"`)
			require.Zero(t, polls)
			w.fail = "delivered"
			report, err := e.RecoverOnce(context.Background(), "first-worker", time.Now(), h.ResolvePoller, archive)
			require.NoError(t, err)
			require.Equal(t, 1, report.Deferred)
			saved, err := model.GetNativeTask(reopened, "req", "g", 1)
			require.NoError(t, err)
			require.Equal(t, "delivery_ready", saved.Status)
			pending := httptest.NewRecorder()
			h.Get(pending, httptest.NewRequest("GET", "/", nil), "req")
			require.Equal(t, 200, pending.Code)
			require.Contains(t, pending.Body.String(), `"status":"running"`)
			require.NotContains(t, pending.Body.String(), "provider.example")
			w.fail = ""
			report, err = e.RecoverOnce(context.Background(), "restart-worker", time.Now().Add(2*time.Minute), h.ResolvePoller, archive)
			require.NoError(t, err)
			require.Equal(t, 1, report.Advanced)
			complete := httptest.NewRecorder()
			h.Get(complete, httptest.NewRequest("GET", "/", nil), "req")
			require.Equal(t, 200, complete.Code)
			require.Contains(t, complete.Body.String(), `"status":"completed"`)
			require.NotContains(t, complete.Body.String(), "provider.example")
			require.NotContains(t, complete.Body.String(), "accepted-1")
			if family.name != "nullable" {
				require.Contains(t, complete.Body.String(), "9007199254740993")
			} else {
				require.Contains(t, complete.Body.String(), `"output":null`)
			}
			require.Equal(t, 202, submit().Code)
			require.Equal(t, 1, posts)
			require.Equal(t, 1, polls)
			if family.files {
				for i := 0; i < 2; i++ {
					require.Equal(t, 1, archiveCalls[i])
					out := httptest.NewRecorder()
					h.GetArtifact(out, httptest.NewRequest("GET", "/", nil), "req", fmt.Sprint(i))
					require.Equal(t, 200, out.Code)
					require.Equal(t, "application/octet-stream", out.Header().Get("Content-Type"))
					require.Contains(t, out.Header().Get("Content-Disposition"), "attachment")
					if i == 0 {
						require.Equal(t, "<svg>isolated</svg>", out.Body.String())
					} else {
						require.Equal(t, []byte{'P', 'K', 3, 4, 0, 1}, out.Body.Bytes())
					}
					require.Equal(t, "nosniff", out.Header().Get("X-Content-Type-Options"))
				}

				var receipts map[int]ownedartifact.Receipt
				saved, err = model.GetNativeTask(reopened, "req", "g", 1)
				require.NoError(t, err)
				require.NoError(t, json.Unmarshal([]byte(saved.ArtifactManifest), &receipts))
				require.NoError(t, os.WriteFile(filepath.Join(root, receipts[0].Key), []byte("corrupt private bytes"), 0600))
				corrupted := httptest.NewRecorder()
				h.GetArtifact(corrupted, httptest.NewRequest("GET", "/", nil), "req", "0")
				require.Equal(t, 503, corrupted.Code)
				require.Contains(t, corrupted.Body.String(), "artifact_integrity_failed")
				require.NotContains(t, corrupted.Body.String(), "corrupt private bytes")
			} else {
				require.Empty(t, archiveCalls)
			}
			token = 2
			hidden := httptest.NewRecorder()
			h.Get(hidden, httptest.NewRequest("GET", "/", nil), "req")
			require.Equal(t, 404, hidden.Code)
			hidden = httptest.NewRecorder()
			h.GetArtifact(hidden, httptest.NewRequest("GET", "/", nil), "req", "0")
			require.Equal(t, 404, hidden.Code)
			require.Equal(t, 1, posts)
			require.Equal(t, 1, polls)
		})
	}
}
