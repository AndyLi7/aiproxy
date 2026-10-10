package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// Exact cached contracts, synthetic delivered records and injected authenticated
// identity. Tests HTTP delivery/replay, not provider admission or paid generation.
func TestNativeCorpusHTTPDelivery(t *testing.T) {
	path := os.Getenv("FAL_NATIVE_EXECUTION_CASES")
	if path == "" {
		t.Skip("requires exact 66-endpoint exported cases")
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var cases []struct {
		Endpoint, SourceHash    string
		Contract, Input, Output json.RawMessage
		Error                   string
	}
	require.NoError(t, json.Unmarshal(raw, &cases))
	require.Len(t, cases, 66)
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "corpus.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}))
	oldDB, oldWallet := model.LogDB, balance.Default
	model.LogDB, balance.Default = db, nativeControllerWallet{}
	t.Cleanup(func() { model.LogDB, balance.Default = oldDB, oldWallet; sql, _ := db.DB(); _ = sql.Close() })
	seen := map[string]bool{}
	evidence := []map[string]any{}
	for index, c := range cases {
		require.False(t, seen[c.Endpoint])
		seen[c.Endpoint] = true
		passed := t.Run(c.Endpoint, func(t *testing.T) {
			require.Empty(t, c.Error)
			require.Len(t, c.SourceHash, 64)
			var contract nativeresult.TaskContract
			require.NoError(t, json.Unmarshal(c.Contract, &contract))
			compiled, err := nativeresult.CompileTaskContract(c.Contract)
			require.NoError(t, err)
			artifacts, err := nativeresult.PlanArtifacts(c.Output, contract.Artifacts)
			require.NoError(t, err)
			id := fmt.Sprintf("corpus-http-%d", index)
			owned := map[string]string{}
			for n, artifact := range artifacts {
				owned[artifact.Pointer] = fmt.Sprintf("https://gateway.example/v1/model-tasks/%s/artifacts/%d", id, n)
			}
			delivered, err := nativeresult.RewriteArtifacts(c.Output, contract.Artifacts, owned)
			require.NoError(t, err)
			_, err = compiled.ValidateOutput(delivered)
			require.NoError(t, err)
			body, err := json.Marshal(map[string]any{"model": contract.Model, "input": c.Input})
			require.NoError(t, err)
			digest := sha256.Sum256(body)
			require.NoError(t, db.Create(&model.NativeTask{ID: id, GroupID: "owner", TokenID: 7, Model: contract.Model,
				Fingerprint: hex.EncodeToString(digest[:]), OutputSchema: string(contract.OutputSchema), OutputSchemaHash: c.SourceHash,
				Status: "completed", DeliveredOutput: string(delivered), NativeOutput: `{"private_marker":true}`,
				BillingOperationID: "native:" + id, CredentialScope: "private_marker"}).Error)
			request := func(group string, token int, method, requestBody string) *httptest.ResponseRecorder {
				router := gin.New()
				router.Use(func(ctx *gin.Context) {
					ctx.Set(middleware.Group, model.GroupCache{ID: group})
					ctx.Set(middleware.Token, model.TokenCache{ID: token})
					ctx.Next()
				})
				router.GET("/v1/model-tasks/:id", GetNativeTask)
				router.POST("/v1/model-tasks", NativeTasks()...)
				url := "/v1/model-tasks/" + id
				if method == http.MethodPost {
					url = "/v1/model-tasks"
				}
				req := httptest.NewRequest(method, url, strings.NewReader(requestBody))
				req.Header.Set("X-Request-Id", id)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, req)
				return response
			}
			for _, auth := range []struct {
				group         string
				token, status int
			}{{"owner", 7, 200}, {"other", 7, 404}, {"owner", 8, 404}, {"", 0, 401}} {
				response := request(auth.group, auth.token, http.MethodGet, "")
				require.Equal(t, auth.status, response.Code)
				require.NotContains(t, response.Body.String(), "private_marker")
				if auth.status == 200 {
					var result struct{ Output json.RawMessage }
					require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
					require.JSONEq(t, string(delivered), string(result.Output))
				}
			}
			replay := request("owner", 7, http.MethodPost, string(body))
			require.Equal(t, 202, replay.Code)
			require.NotContains(t, replay.Body.String(), "private_marker")
			changed := request("owner", 7, http.MethodPost, `{"model":"changed","input":{}}`)
			require.Equal(t, 409, changed.Code)
		})
		evidence = append(evidence, map[string]any{"endpoint": c.Endpoint, "sourceHash": c.SourceHash, "httpDeliveryReplayPassed": passed, "syntheticRecords": true, "injectedAuthenticatedIdentity": true, "providerAdmissionVerified": false, "paidGenerationVerified": false})
	}
	if out := os.Getenv("FAL_NATIVE_HTTP_REPORT"); out != "" {
		data, err := json.MarshalIndent(evidence, "", "  ")
		require.NoError(t, err)
		f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		require.NoError(t, err)
		_, err = f.Write(data)
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}
}
