package router

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/stretchr/testify/require"
)

func nativeValidationRouter(t *testing.T) *gin.Engine {
	old := config.AdminKey
	config.AdminKey = "isolated-native-validation"
	t.Cleanup(func() { config.AdminKey = old })
	r := gin.New()
	SetAPIRouter(r)
	return r
}
func nativeValidationRequest(r *gin.Engine, body []byte, key string) *httptest.ResponseRecorder {
	request := httptest.NewRequest("POST", "/api/native-contract/validate", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	out := httptest.NewRecorder()
	r.ServeHTTP(out, request)
	return out
}
func TestNativeValidationRegisteredRouteAndAuth(t *testing.T) {
	r := nativeValidationRouter(t)
	body := []byte(`{"version":1,"model":"example/model/native","input_schema":{"type":"object"},"output_schema":{}}`)
	for _, key := range []string{"", "incorrect"} {
		require.Equal(t, 401, nativeValidationRequest(r, body, key).Code)
	}
	require.Equal(t, 200, nativeValidationRequest(r, body, config.AdminKey).Code)
	require.Equal(t, 400, nativeValidationRequest(r, append(body, []byte(`{}`)...), config.AdminKey).Code)
}

// Opt-in cached corpus rehearsal of the ACTUAL registered admin route. No DB,
// provider, network listener or wallet; this is schema API evidence only.
func TestNativeValidationCachedCorpus(t *testing.T) {
	input, output := os.Getenv("FAL_NATIVE_HTTP_CENSUS"), os.Getenv("FAL_NATIVE_HTTP_REPORT")
	if input == "" {
		t.Skip("explicit cached census required")
	}
	require.NotEmpty(t, output)
	raw, err := os.ReadFile(input)
	require.NoError(t, err)
	var rows []struct {
		Endpoint  string          `json:"endpoint"`
		SourceSHA string          `json:"sourceSha256"`
		Contract  json.RawMessage `json:"contract"`
		Error     string          `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &rows))
	require.NotEmpty(t, rows)
	r := nativeValidationRouter(t)
	passed, missing := 0, 0
	seen := map[string]bool{}
	results := []map[string]any{}
	for _, row := range rows {
		require.NotEmpty(t, row.Endpoint)
		require.False(t, seen[row.Endpoint], "duplicate corpus endpoint")
		seen[row.Endpoint] = true
		entry := map[string]any{"endpoint": row.Endpoint, "sourceSha256": row.SourceSHA, "publicReady": false, "schemaApiVerified": false}
		if row.Error != "" {
			missing++
			entry["sourceError"] = row.Error
			results = append(results, entry)
			continue
		}
		denied := nativeValidationRequest(r, row.Contract, "")
		entry["unauthenticatedStatus"] = denied.Code
		out := nativeValidationRequest(r, row.Contract, config.AdminKey)
		entry["httpStatus"] = out.Code
		var response struct {
			Success bool `json:"success"`
			Data    struct {
				SchemaCompiled bool   `json:"schemaCompiled"`
				Digest         string `json:"contractDigest"`
				Version        int    `json:"validatorVersion"`
			} `json:"data"`
		}
		decodeErr := json.Unmarshal(out.Body.Bytes(), &response)
		sum := sha256.Sum256(row.Contract)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		entry["contractDigest"] = digest
		if denied.Code == 401 && out.Code == 200 && decodeErr == nil && response.Success && response.Data.SchemaCompiled && response.Data.Version == 1 && response.Data.Digest == digest {
			passed++
			entry["schemaApiVerified"] = true
		} else {
			t.Errorf("schema API failed for %s (HTTP %d)", row.Endpoint, out.Code)
		}
		results = append(results, entry)
	}
	summary := map[string]any{"total": len(rows), "schemaApiVerified": passed, "sourceFailures": missing, "apiFailures": len(rows) - passed - missing, "publicReady": 0}
	report, err := json.MarshalIndent(map[string]any{"summary": summary, "results": results}, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(output, report, 0600))
	t.Logf("summary: %+v", summary)
}

func TestNativePrivateTrialRoutesRequireAdmin(t *testing.T) {
	r := nativeValidationRouter(t)
	for _, route := range []struct{ method, path string }{
		{"POST", "/api/native-trials/internal/7"},
		{"GET", "/api/native-trials/internal/7/trial_one"},
		{"GET", "/api/native-trials/internal/7/trial_one/artifacts/0"},
	} {
		request := httptest.NewRequest(route.method, route.path, nil)
		out := httptest.NewRecorder()
		r.ServeHTTP(out, request)
		require.Equal(t, 401, out.Code)
	}
}
