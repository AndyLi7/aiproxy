//nolint:testpackage
package middleware

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

type videoCapabilityContractFixture struct {
	ContractVersion int      `json:"contract_version"`
	RouteDelimiter  string   `json:"route_delimiter"`
	Capabilities    []string `json:"capabilities"`
	ValidRoutes     []struct {
		PublicModel      string `json:"public_model"`
		Capability       string `json:"capability"`
		RoutingModel     string `json:"routing_model"`
		ChannelID        string `json:"channel_id"`
		GatewayChannelID int    `json:"gateway_channel_id"`
	} `json:"valid_routes"`
	InvalidRequests []struct {
		PublicModel string  `json:"public_model"`
		Capability  *string `json:"capability"`
		ErrorCode   string  `json:"error_code"`
	} `json:"invalid_requests"`
	InvalidConfigs []struct {
		RoutePublicModel  string `json:"route_public_model"`
		RouteCapability   string `json:"route_capability"`
		ConfigPublicModel string `json:"config_public_model"`
		ConfigCapability  string `json:"config_capability"`
	} `json:"invalid_configs"`
	LogProjection struct {
		PublicFields       []string `json:"public_fields"`
		InternalOnlyFields []string `json:"internal_only_fields"`
	} `json:"log_projection"`
}

func readVideoCapabilityContractFixture(t *testing.T) videoCapabilityContractFixture {
	t.Helper()

	raw, err := os.ReadFile("../testdata/video-capability-contract-v1.json")
	require.NoError(t, err)

	var fixture videoCapabilityContractFixture
	require.NoError(t, json.Unmarshal(raw, &fixture))

	return fixture
}

func TestVideoCapabilityContractFixture(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	fixture := readVideoCapabilityContractFixture(t)
	require.Equal(t, model.ModelCapabilityContractVersion, fixture.ContractVersion)
	require.Equal(t, "::", fixture.RouteDelimiter)
	require.Equal(t, []string{"text-to-video", "image-to-video"}, fixture.Capabilities)

	for _, route := range fixture.ValidRoutes {
		route := route
		t.Run(route.Capability, func(t *testing.T) {
			t.Parallel()
			capability := model.ModelCapability(route.Capability)
			routingModel, err := model.BuildModelCapabilityKey(route.PublicModel, capability)
			require.NoError(t, err)
			require.Equal(t, route.RoutingModel, routingModel)
			require.NotEmpty(t, route.ChannelID)
			require.Positive(t, route.GatewayChannelID)
			require.NoError(t, model.ValidateModelCapabilityConfig(
				map[model.ModelConfigKey]any{
					"capability_contract_version": fixture.ContractVersion,
					"public_model":                route.PublicModel,
					"capability":                  route.Capability,
				},
				route.PublicModel,
				capability,
			))
		})
	}

	for _, requestCase := range fixture.InvalidRequests {
		requestCase := requestCase
		t.Run(requestCase.ErrorCode, func(t *testing.T) {
			t.Parallel()
			body := map[string]any{"model": requestCase.PublicModel}
			if requestCase.Capability != nil {
				body["capability"] = *requestCase.Capability
			}
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			req := httptest.NewRequestWithContext(
				t.Context(), http.MethodPost, "/v1/videos", bytes.NewReader(raw),
			)
			req.Header.Set("Content-Type", "application/json")
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = req

			_, _, err = resolveVideoCapability(
				ctx, mode.Videos, requestCase.PublicModel,
			)
			validationErr, ok := err.(*publicVideoRequestValidationError)
			require.True(t, ok)
			require.Equal(t, requestCase.ErrorCode, validationErr.code)
		})
	}

	for _, config := range fixture.InvalidConfigs {
		err := model.ValidateModelCapabilityConfig(
			map[model.ModelConfigKey]any{
				"capability_contract_version": fixture.ContractVersion,
				"public_model":                config.ConfigPublicModel,
				"capability":                  config.ConfigCapability,
			},
			config.RoutePublicModel,
			model.ModelCapability(config.RouteCapability),
		)
		require.Error(t, err)
	}

	require.Equal(t, []string{"model", "capability"}, fixture.LogProjection.PublicFields)
	require.Contains(t, fixture.LogProjection.InternalOnlyFields, "routing_model")
	require.Contains(t, fixture.LogProjection.InternalOnlyFields, "route_id")
}
