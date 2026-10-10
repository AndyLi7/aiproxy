package middleware

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

// Optional offline audit of public contracts and channel bindings exported by an
// operator. The input must contain no credentials or customer request data.
// This checks normalization through provider mapping without calling upstreams.
func TestExportedImageRegistryContracts(t *testing.T) {
	path := os.Getenv("IMAGE_CONTRACT_AUDIT_FILE")
	if path == "" {
		t.Skip("no exported public contracts supplied")
	}
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	count := 0
	for scanner.Scan() {
		var row struct {
			Model    string
			Contract json.RawMessage
			Mapping  string
			Binding  registryvalidation.ProviderBinding
		}
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &row))
		var contract struct {
			ID    string `json:"entry_id"`
			Input struct {
				Properties map[string]any `json:"properties"`
			} `json:"input_schema"`
		}
		require.NoError(t, json.Unmarshal(row.Contract, &contract))
		discovery := registryvalidation.DiscoverImage(row.Contract)
		require.NotNil(t, discovery, "every enabled image contract must be discoverable")
		require.Equal(t, "/v1/images/tasks", discovery.API["endpoint"])
		for _, alias := range []bool{false, true} {
			name := row.Model + "/canonical"
			if alias {
				name = row.Model + "/size-alias"
			}
			t.Run(name, func(t *testing.T) {
				input := map[string]any{"model": contract.ID, "prompt": "A teapot on a wooden table"}
				if _, ok := contract.Input.Properties["image_url"]; ok {
					input["image_url"] = "https://example.com/reference.png"
				}
				if _, ok := contract.Input.Properties["image_urls"]; ok {
					input["image_urls"] = []string{"https://example.com/reference.png"}
				}
				if alias {
					input["size"] = "1536x1024"
				} else {
					input["image_size"] = map[string]int{"width": 1536, "height": 1024}
				}
				body, err := json.Marshal(input)
				require.NoError(t, err)
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/tasks", strings.NewReader(string(body)))
				c.Request.Header.Set("Content-Type", "application/json")
				cfg := map[model.ModelConfigKey]any{"x_token_platform_capability_contract": map[string]any{"entry_id": contract.ID, "contract": row.Contract}}
				require.Nil(t, validateImageRegistryRequest(c, mode.ImagesGenerations, contract.ID, cfg))
				normalized, err := common.GetRequestBodyReusable(c.Request)
				require.NoError(t, err)
				_, err = registryvalidation.MapBoundProviderInput(row.Contract, row.Binding, "fal-image", row.Mapping, "async", normalized)
				require.NoError(t, err, "normalized request must remain compatible with its configured channel")
			})
		}
		count++
	}
	require.NoError(t, scanner.Err())
	require.Positive(t, count)
}
