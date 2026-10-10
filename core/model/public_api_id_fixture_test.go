package model_test

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"sort"
	"testing"

	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

// publicAPIIDFixture is testdata/public-api-id-v1.json, a byte-identical copy
// of the application's tests/fixtures/public-api-id-v1.json: the model configs
// the application publishes and what the gateway answers for them.
type publicAPIIDFixture struct {
	Feature    string            `json:"feature"`
	ConfigKeys map[string]string `json:"config_keys"`
	Modes      struct {
		NativeTasks       mode.Mode `json:"native_tasks"`
		ImagesGenerations mode.Mode `json:"images_generations"`
	} `json:"modes"`
	Configs        []model.ModelConfig `json:"configs"`
	Listed         []string            `json:"listed"`
	NativeRoutes   map[string]string   `json:"native_routes"`
	ResponseModels map[string]string   `json:"response_models"`
	NativeErrors   []publicAPIIDError  `json:"native_errors"`
	ImageErrors    []publicAPIIDError  `json:"image_errors"`
}

type publicAPIIDError struct {
	Requested string          `json:"requested"`
	Status    int             `json:"status"`
	Body      json.RawMessage `json:"body"`
}

func readPublicAPIIDFixture(t *testing.T) publicAPIIDFixture {
	t.Helper()

	raw, err := os.ReadFile("testdata/public-api-id-v1.json")
	require.NoError(t, err)

	var fixture publicAPIIDFixture
	require.NoError(t, json.Unmarshal(raw, &fixture))
	require.NotEmpty(t, fixture.Configs)

	return fixture
}

func TestPublicAPIIDFixtureNamesMatch(t *testing.T) {
	fixture := readPublicAPIIDFixture(t)
	require.Equal(t, model.PublicAPIIDFeature, fixture.Feature)
	require.Equal(t, map[string]string{
		"public_api_id":             string(model.ModelConfigPublicAPIIDKey),
		"public_capability_aliases": string(model.ModelConfigPublicCapabilityAliasesKey),
	}, fixture.ConfigKeys)
	require.Equal(t, mode.NativeTasks, fixture.Modes.NativeTasks)
	require.Equal(t, mode.ImagesGenerations, fixture.Modes.ImagesGenerations)
}

func TestPublicAPIIDFixtureListingAndResolution(t *testing.T) {
	fixture := readPublicAPIIDFixture(t)

	listed := make([]string, 0, len(fixture.Configs))
	byRoute := make(map[string]model.ModelConfig, len(fixture.Configs))
	for _, config := range fixture.Configs {
		listed = append(listed, model.ListedPublicModelID(config))
		byRoute[config.Model] = config
	}

	sort.Strings(listed)
	require.Equal(t, fixture.Listed, listed)

	for requested, route := range fixture.NativeRoutes {
		resolution, match := model.ResolveNativeCapabilityRoute(requested, fixture.Configs)
		if route == "" {
			require.Equal(t, model.NativeRouteNotFound, match, requested)
			continue
		}

		require.Equal(t, model.NativeRouteFound, match, requested)
		require.Equal(t, route, resolution.Route, requested)
	}

	for capabilityModel, want := range fixture.ResponseModels {
		require.Equal(t, want, model.NativeCallableModelID(capabilityModel, byRoute), capabilityModel)
	}

	for _, example := range fixture.NativeErrors {
		var body struct {
			Error struct {
				SuggestedModels []string `json:"suggested_models"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(example.Body, &body))

		suggestion := model.SuggestNativeModels(example.Requested, fixture.Configs)
		require.Equal(t, example.Status == 400, suggestion.OtherEndpoint, example.Requested)
		require.Equal(t, body.Error.SuggestedModels, suggestion.Models, example.Requested)
	}
}

// The native envelope the gateway writes is the one the application documents.
func TestPublicAPIIDFixtureNativeErrorEnvelopes(t *testing.T) {
	fixture := readPublicAPIIDFixture(t)
	for _, example := range fixture.NativeErrors {
		var body struct {
			Error struct {
				Code            string   `json:"code"`
				Param           string   `json:"param"`
				SuggestedModels []string `json:"suggested_models"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(example.Body, &body))

		recorder := httptest.NewRecorder()
		nativetask.WriteErrorDetail(recorder, example.Status, body.Error.Code,
			body.Error.Param, body.Error.SuggestedModels)
		require.Equal(t, example.Status, recorder.Code)
		require.JSONEq(t, string(example.Body), recorder.Body.String(), example.Requested)
	}
}
