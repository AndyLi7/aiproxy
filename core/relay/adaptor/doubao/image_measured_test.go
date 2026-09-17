//nolint:testpackage // These fixtures verify internal metering and persistence boundaries.
package doubao

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	coremodel "github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/stretchr/testify/require"
)

func TestMeasuredImageProviderEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, body, scenario, state string
		count                       int
	}{
		{"mixed sizes", `{"data":[{"url":"secret","size":"1000x2610"},{"b64_json":"secret","size":"2610001x1"}],"usage":{"input_images":2,"generated_images":2}}`, "generation", "complete", 2},
		{"missing input count", `{"data":[{"url":"secret","size":"2x2"}],"usage":{"generated_images":1}}`, "generation", "incomplete", 1},
		{"explicit zero input count", `{"data":[{"url":"secret","size":"2x2"}],"usage":{"input_images":0,"generated_images":1}}`, "generation", "complete", 1},
		{"zero", `{"data":[],"usage":{"generated_images":0}}`, "generation", "complete", 0},
		{"missing data", `{"usage":{"generated_images":0}}`, "generation", "incomplete", 0},
		{"missing size", `{"data":[{"url":"secret"}]}`, "generation", "incomplete", 1},
		{"count mismatch", `{"data":[{"url":"secret","size":"2x2"}],"usage":{"generated_images":8}}`, "generation", "incomplete", 1},
		{"layer failure", `{"data":[{"url":"secret","size":"2x2","z_index":0},{"error":{"message":"failed"}}]}`, "layer_decomposition", "failed", 0},
		{"layers", `{"usage":{"input_images":1},"data":[{"url":"secret","size":"2x2","z_index":0},{"url":"secret","size":"3x3","z_index":1}]}`, "layer_decomposition", "complete", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r doubaoImageResponse
			require.NoError(t, json.Unmarshal([]byte(tc.body), &r))
			got := measureDoubaoImages(r, tc.scenario)
			require.Equal(t, tc.state, got.State)
			require.Len(t, got.Outputs, tc.count)
			b, err := json.Marshal(got)
			require.NoError(t, err)
			require.NotContains(t, string(b), "secret")
		})
	}
}

func TestMeasuredImageStreamsRejectedBeforeInference(t *testing.T) {
	for _, modelName := range []string{"doubao-seedream-5-0-pro-260628", "custom-measured"} {
		m := meta.NewMeta(&coremodel.Channel{}, 5, modelName, coremodel.ModelConfig{})
		if modelName == "custom-measured" {
			m.ModelConfig.Price.ImageBilling = &coremodel.ImageBillingPolicy{
				Version:  1,
				Scenario: "generation",
			}
		}

		_, err := ConvertImageRequest(
			m,
			httptest.NewRequestWithContext(t.Context(),
				http.MethodPost,
				"/images",
				strings.NewReader(`{"model":"caller","prompt":"x","stream":true}`),
			),
		)
		require.Error(t, err)
	}

	m := meta.NewMeta(&coremodel.Channel{}, 5, "doubao-seedream-4-5", coremodel.ModelConfig{})
	_, err := ConvertImageRequest(
		m,
		httptest.NewRequestWithContext(t.Context(),
			http.MethodPost,
			"/images",
			strings.NewReader(`{"prompt":"x","stream":true}`),
		),
	)
	require.NoError(t, err)
}

func TestMeasuredTruncatedResponseIsIncomplete(t *testing.T) {
	m := meta.NewMeta(
		&coremodel.Channel{},
		5,
		"image",
		coremodel.ModelConfig{
			Price: coremodel.Price{
				ImageBilling: &coremodel.ImageBillingPolicy{Version: 1, Scenario: "generation"},
			},
		},
	)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/images", nil)
	got, err := ImageHandler(
		m,
		c,
		&http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"data":[`)),
		},
	)
	require.NotNil(t, err)
	require.Equal(t, "incomplete", got.UsageContext.ImageUsage.State)
}
