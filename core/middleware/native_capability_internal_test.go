package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

func publicIDConfig(
	m mode.Mode,
	publicModel, capability string,
	extra map[model.ModelConfigKey]any,
) model.ModelConfig {
	config := map[model.ModelConfigKey]any{
		model.ModelConfigCapabilityContractVersionKey: float64(1),
		model.ModelConfigPublicModelKey:               publicModel,
		model.ModelConfigPublicCapabilityModelKey:     publicModel + "/" + capability,
		model.ModelConfigCapabilityKey:                capability,
	}
	for key, value := range extra {
		config[key] = value
	}

	return model.ModelConfig{Model: publicModel + "::" + capability, Type: m, Config: config}
}

type distributedRequest struct {
	status   int
	body     string
	routing  string
	request  string
	fields   model.OperationalFields
	reached  bool
	balanced bool
}

// distributeAs runs the distributor for one request of a key entitled to
// entitled, with every config enabled. An internal group skips the wallet.
func distributeAs(
	t *testing.T,
	requestMode mode.Mode,
	path string,
	configs []model.ModelConfig,
	entitled []string,
	internal bool,
	body string,
) distributedRequest {
	t.Helper()

	enabled := make(map[string]model.ModelConfig, len(configs))
	for _, config := range configs {
		enabled[config.Model] = config
	}

	token := model.TokenCache{ID: 7, Name: "key"}
	token.SetAvailableSets([]string{model.ChannelDefaultSet})
	token.SetModelsBySet(map[string][]string{model.ChannelDefaultSet: entitled})

	group := model.GroupCache{ID: "customer"}
	if internal {
		group.Status = model.GroupStatusInternal
	}

	var result distributedRequest
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(Group, group)
		c.Set(Token, token)
		c.Set(ModelCaches, &model.ModelCaches{
			ModelConfig:            imageCapabilityTestCache(enabled),
			EnabledModelConfigsMap: enabled,
		})
		c.Next()
		result.routing = c.GetString(RoutingModel)
		result.request = GetRequestModel(c)
		result.fields = OperationalFieldsFromContext(c)
		result.balanced = GetGroupBalanceConsumerFromContext(c) != nil
	})
	router.POST(path, NewDistribute(requestMode), func(c *gin.Context) {
		result.reached = true
		c.Status(http.StatusOK)
	})

	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	result.status = recorder.Code
	result.body = recorder.Body.String()

	return result
}

type modelErrorBody struct {
	Error struct {
		Code            string    `json:"code"`
		Message         string    `json:"message"`
		Type            string    `json:"type"`
		Param           string    `json:"param"`
		SuggestedModels *[]string `json:"suggested_models"`
	} `json:"error"`
}

func decodeModelError(t *testing.T, raw string) modelErrorBody {
	t.Helper()
	var decoded modelErrorBody
	require.NoError(t, json.Unmarshal([]byte(raw), &decoded), raw)
	return decoded
}

const ttsCapabilityID = "elevenlabs/eleven-v4/text-to-speech"

func publicIDCatalog() ([]model.ModelConfig, []string) {
	configs := []model.ModelConfig{
		publicIDConfig(mode.NativeTasks, "elevenlabs/eleven-v4", "text-to-speech", map[model.ModelConfigKey]any{
			model.ModelConfigPublicAPIIDKey:             "elevenlabs/eleven-v4",
			model.ModelConfigPublicCapabilityAliasesKey: []any{"elevenlabs/eleven-4"},
		}),
		publicIDConfig(mode.NativeTasks, "alibaba/wan-2.7", "text-to-image", nil),
		publicIDConfig(mode.NativeTasks, "alibaba/wan-2.7", "image-to-image", nil),
		publicIDConfig(mode.ImagesGenerations, "bytedance/seedream-4.5", "text-to-image", nil),
	}
	entitled := make([]string, 0, len(configs))
	for _, config := range configs {
		entitled = append(entitled, config.Model)
	}

	return configs, entitled
}

func TestNativeTasksResolveEveryAcceptedID(t *testing.T) {
	configs, entitled := publicIDCatalog()
	for _, id := range []string{ttsCapabilityID, "elevenlabs/eleven-v4", "elevenlabs/eleven-4"} {
		got := distributeAs(t, mode.NativeTasks, "/v1/model-tasks", configs, entitled, true,
			`{"model":"`+id+`","input":{}}`)
		require.True(t, got.reached, "%s: %d %s", id, got.status, got.body)
		require.Equal(t, "elevenlabs/eleven-v4::text-to-speech", got.routing, id)
		// Logs split the capability ID whichever ID was requested.
		require.Equal(t, "elevenlabs/eleven-v4", got.request, id)
		require.Equal(t, id, got.fields.RequestedModel)
		require.Equal(t, "elevenlabs/eleven-v4", got.fields.PublicModel)
		require.Equal(t, ttsCapabilityID, got.fields.PublicCapabilityModel)
		require.Equal(t, "text-to-speech", got.fields.ResolvedCapability)
	}
}

// Unknown IDs on the native endpoint get the native envelope with suggested
// IDs, before any wallet lookup, and the rejected log row names the request.
func TestNativeTasksUnknownModelSuggestions(t *testing.T) {
	configs, entitled := publicIDCatalog()
	for _, tc := range []struct {
		requested string
		status    int
		code      string
		want      []string
	}{
		{"elevenlabs/eleven-v9", http.StatusNotFound, "model_not_found", []string{}},
		{"alibaba/wan-2.7", http.StatusNotFound, "model_not_found",
			[]string{"alibaba/wan-2.7/image-to-image", "alibaba/wan-2.7/text-to-image"}},
		{"Elevenlabs/Eleven-V4", http.StatusNotFound, "model_not_found", []string{"elevenlabs/eleven-v4"}},
		{"elevenlabs/eleven-v4/text-to-speech-2", http.StatusNotFound, "model_not_found", []string{"elevenlabs/eleven-v4"}},
		{"bytedance/seedream-4.5/text-to-image", http.StatusBadRequest, "native_model_unavailable",
			[]string{"bytedance/seedream-4.5/text-to-image"}},
		{"bytedance/seedream-4.5", http.StatusBadRequest, "native_model_unavailable",
			[]string{"bytedance/seedream-4.5/text-to-image"}},
		// An internal route key the key may call, on the wrong endpoint.
		{"bytedance/seedream-4.5::text-to-image", http.StatusBadRequest, "native_model_unavailable",
			[]string{"bytedance/seedream-4.5/text-to-image"}},
	} {
		got := distributeAs(t, mode.NativeTasks, "/v1/model-tasks", configs, entitled, false,
			`{"model":"`+tc.requested+`","input":{}}`)
		require.False(t, got.reached, tc.requested)
		require.Equal(t, tc.status, got.status, "%s: %s", tc.requested, got.body)
		require.False(t, got.balanced, "model errors come before the wallet")

		decoded := decodeModelError(t, got.body)
		require.Equal(t, tc.code, decoded.Error.Code, tc.requested)
		require.Equal(t, "model", decoded.Error.Param)
		require.NotNil(t, decoded.Error.SuggestedModels)
		require.Equal(t, tc.want, *decoded.Error.SuggestedModels, tc.requested)
		if tc.status == http.StatusNotFound {
			require.Equal(t, "not_found_error", decoded.Error.Type)
		} else {
			require.Equal(t, "invalid_request_error", decoded.Error.Type)
		}
		for _, leak := range []string{"aiproxy", "::", "fal", "elevenlabs/eleven-4\""} {
			require.NotContains(t, got.body, leak, tc.requested)
		}

		require.Equal(t, tc.requested, got.request)
		require.Equal(t, tc.requested, got.fields.RequestedModel)
		require.Equal(t, tc.code, got.fields.ErrorCode)
		require.Equal(t, model.FailureStageModel, got.fields.FailureStage)
	}
}

func TestNativeTasksSuggestOnlyWhatTheKeyMayCall(t *testing.T) {
	configs, _ := publicIDCatalog()
	got := distributeAs(t, mode.NativeTasks, "/v1/model-tasks", configs,
		[]string{"elevenlabs/eleven-v4::text-to-speech"}, false, `{"model":"alibaba/wan-2.7","input":{}}`)
	require.Equal(t, http.StatusNotFound, got.status)
	require.Equal(t, []string{}, *decodeModelError(t, got.body).Error.SuggestedModels)

	// Another key's model is unknown to this key, by every ID.
	got = distributeAs(t, mode.NativeTasks, "/v1/model-tasks", configs,
		[]string{"alibaba/wan-2.7::text-to-image"}, false, `{"model":"elevenlabs/eleven-v4","input":{}}`)
	require.Equal(t, http.StatusNotFound, got.status)
	require.Equal(t, []string{}, *decodeModelError(t, got.body).Error.SuggestedModels)

	// A model the key may call whose config is missing: 404 in the native envelope.
	got = distributeAs(t, mode.NativeTasks, "/v1/model-tasks", configs,
		[]string{"vendor/ghost::text-to-speech"}, false, `{"model":"vendor/ghost::text-to-speech","input":{}}`)
	require.Equal(t, http.StatusNotFound, got.status, got.body)
	require.Equal(t, "model_not_found", decodeModelError(t, got.body).Error.Code)
}

func TestNativeTasksAmbiguousPublicIDIsOurs(t *testing.T) {
	configs, entitled := publicIDCatalog()
	twin := publicIDConfig(mode.NativeTasks, "elevenlabs/eleven-v4", "text-to-music", map[model.ModelConfigKey]any{
		model.ModelConfigPublicAPIIDKey: "elevenlabs/eleven-v4",
	})
	configs = append(configs, twin)
	entitled = append(entitled, twin.Model)

	got := distributeAs(t, mode.NativeTasks, "/v1/model-tasks", configs, entitled, false,
		`{"model":"elevenlabs/eleven-v4","input":{}}`)
	require.Equal(t, http.StatusServiceUnavailable, got.status, got.body)
	require.Equal(t, "model_route_unavailable", decodeModelError(t, got.body).Error.Code)
	require.Equal(t, model.FailureStageRouting, got.fields.FailureStage)

	// Each capability ID still resolves.
	got = distributeAs(t, mode.NativeTasks, "/v1/model-tasks", configs, entitled, true,
		`{"model":"`+ttsCapabilityID+`","input":{}}`)
	require.True(t, got.reached, got.body)
}

// A native video capability's route key is also a video capability route key;
// its check compares the config's public_model with the resolved public model,
// not with the requested capability ID (which returned 503).
func TestNativeVideoCapabilityPassesRouteKeyCheck(t *testing.T) {
	video := publicIDConfig(mode.NativeTasks, "vendor/motion", "text-to-video", nil)
	got := distributeAs(t, mode.NativeTasks, "/v1/model-tasks", []model.ModelConfig{video},
		[]string{video.Model}, true, `{"model":"vendor/motion/text-to-video","input":{}}`)
	require.True(t, got.reached, "%d %s", got.status, got.body)
	require.Equal(t, "vendor/motion::text-to-video", got.routing)
	require.Equal(t, "vendor/motion", got.request)
}

func imageContractConfig(publicModel, capability string) model.ModelConfig {
	id := publicModel + "/" + capability
	return publicIDConfig(mode.ImagesGenerations, publicModel, capability, map[model.ModelConfigKey]any{
		model.ModelConfigParameterSchemaKey: map[string]any{
			"type":       "object",
			"properties": map[string]any{"prompt": map[string]any{"type": "string"}},
		},
		model.ModelConfigDefaultParametersKey: map[string]any{},
		"x_token_platform_capability_contract": map[string]any{
			"entry_id": id,
			"contract": map[string]any{
				"entry_id": id, "validation_version": 1,
				"input_schema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"prompt": map[string]any{"type": "string", "minLength": 1}},
					"required":   []string{"prompt"}, "additionalProperties": false,
				},
			},
		},
	})
}

// Owner decision D3 (2026-10-10), turned on by DISABLE_IMAGE_GROUP_IDS: image
// endpoints no longer pick a capability for a model group ID; they answer 404
// model_not_found with the group's capability IDs. The full capability ID
// keeps working, and without the switch the group ID keeps today's behaviour.
func TestImageEndpointsRefuseModelGroupIDs(t *testing.T) {
	seedream := imageContractConfig("bytedance/seedream-4.5", "text-to-image")
	native := publicIDConfig(mode.NativeTasks, "alibaba/wan-2.7", "text-to-image", nil)
	configs := []model.ModelConfig{seedream, native}
	entitled := []string{seedream.Model, native.Model}

	original := config.DisableImageGroupIDs
	t.Cleanup(func() { config.DisableImageGroupIDs = original })

	for _, path := range []string{"/v1/images/generations", "/v1/images/tasks"} {
		// Until the switch is set, a group ID still selects its capability.
		config.DisableImageGroupIDs = false
		got := distributeAs(t, mode.ImagesGenerations, path, configs, entitled, true,
			`{"model":"bytedance/seedream-4.5","prompt":"a cat"}`)
		require.True(t, got.reached, "%d %s", got.status, got.body)
		require.Equal(t, "bytedance/seedream-4.5::text-to-image", got.routing)

		config.DisableImageGroupIDs = true
		got = distributeAs(t, mode.ImagesGenerations, path, configs, entitled, false,
			`{"model":"bytedance/seedream-4.5","prompt":"a cat"}`)
		require.Equal(t, http.StatusNotFound, got.status, got.body)
		decoded := decodeModelError(t, got.body)
		require.Equal(t, "model_not_found", decoded.Error.Code)
		require.Equal(t, "not_found_error", decoded.Error.Type)
		require.Equal(t, "model", decoded.Error.Param)
		require.Equal(t, imageGroupModelMessage, decoded.Error.Message)
		require.Equal(t, []string{"bytedance/seedream-4.5/text-to-image"}, *decoded.Error.SuggestedModels)
		require.NotContains(t, got.body, "::")
		require.False(t, got.balanced)
		require.Equal(t, "bytedance/seedream-4.5", got.fields.RequestedModel)
		require.Equal(t, "model_not_found", got.fields.ErrorCode)

		got = distributeAs(t, mode.ImagesGenerations, path, configs, entitled, true,
			`{"model":"bytedance/seedream-4.5/text-to-image","prompt":"a cat"}`)
		require.True(t, got.reached, "%d %s", got.status, got.body)
		require.Equal(t, "bytedance/seedream-4.5::text-to-image", got.routing)
	}
}

// Every image model ID that calls nothing (a typo, a retired ID, another
// endpoint's model) gets the same documented 404 model_not_found envelope,
// with or without the D3 switch; suggestions are empty unless the ID names an
// image model group.
func TestImageEndpointsAnswerUnknownModelsWithModelNotFound(t *testing.T) {
	seedream := imageContractConfig("bytedance/seedream-4.5", "text-to-image")
	native := publicIDConfig(mode.NativeTasks, "alibaba/wan-2.7", "text-to-image", nil)
	configs := []model.ModelConfig{seedream, native}
	entitled := []string{seedream.Model, native.Model}

	original := config.DisableImageGroupIDs
	t.Cleanup(func() { config.DisableImageGroupIDs = original })

	for _, refuseGroupIDs := range []bool{false, true} {
		config.DisableImageGroupIDs = refuseGroupIDs
		for _, requested := range []string{
			"bytedance/seedream-4.5/text-to-imag",
			"bytedance/seedream-9",
			"alibaba/wan-2.7",
			"alibaba/wan-2.7/text-to-image",
		} {
			for _, path := range []string{"/v1/images/generations", "/v1/images/tasks"} {
				got := distributeAs(t, mode.ImagesGenerations, path, configs, entitled, false,
					`{"model":"`+requested+`","prompt":"a cat"}`)
				require.Equal(t, http.StatusNotFound, got.status, "%s %s", requested, got.body)
				decoded := decodeModelError(t, got.body)
				require.Equal(t, "model_not_found", decoded.Error.Code, got.body)
				require.Equal(t, "not_found_error", decoded.Error.Type)
				require.Equal(t, "model", decoded.Error.Param)
				require.Equal(t, nativetask.ErrorMessage("model_not_found"), decoded.Error.Message)
				require.NotNil(t, decoded.Error.SuggestedModels, got.body)
				require.Empty(t, *decoded.Error.SuggestedModels)
				require.NotContains(t, got.body, "aiproxy_error")
				require.Equal(t, requested, got.fields.RequestedModel)
				require.Equal(t, "model_not_found", got.fields.ErrorCode)
			}
		}
	}
}

// The rejected request's log row names the requested ID (it used to be empty)
// and the error code, for every model error on the native endpoint.
func TestNativeModelRejectionLogRowNamesRequestedID(t *testing.T) {
	var rows []*model.Log
	original := recordOperationalLog
	recordOperationalLog = func(entry *model.Log) error {
		rows = append(rows, entry)
		return nil
	}
	t.Cleanup(func() { recordOperationalLog = original })

	configs, entitled := publicIDCatalog()
	enabled := make(map[string]model.ModelConfig, len(configs))
	for _, config := range configs {
		enabled[config.Model] = config
	}
	token := model.TokenCache{ID: 7, Name: "key"}
	token.SetAvailableSets([]string{model.ChannelDefaultSet})
	token.SetModelsBySet(map[string][]string{model.ChannelDefaultSet: entitled})

	router := gin.New()
	router.Use(OperationalLogMiddleware(), func(c *gin.Context) {
		c.Set(Group, model.GroupCache{ID: "customer"})
		c.Set(Token, token)
		c.Set(ModelCaches, &model.ModelCaches{
			ModelConfig:            imageCapabilityTestCache(enabled),
			EnabledModelConfigsMap: enabled,
		})
		c.Next()
	})
	router.POST("/v1/model-tasks", NewDistribute(mode.NativeTasks))

	for requested, code := range map[string]string{
		"elevenlabs/eleven-v9":   "model_not_found",
		"bytedance/seedream-4.5": "native_model_unavailable",
	} {
		rows = nil
		request := httptest.NewRequest(http.MethodPost, "/v1/model-tasks",
			strings.NewReader(`{"model":"`+requested+`","input":{}}`))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(httptest.NewRecorder(), request)

		require.Len(t, rows, 1, requested)
		require.Equal(t, requested, rows[0].Model)
		require.Empty(t, rows[0].Capability)
		require.Equal(t, code, rows[0].ErrorCode)
		require.Equal(t, model.FailureStageModel, rows[0].FailureStage)
		require.Equal(t, requested, rows[0].Metadata["requested_model"])
	}
}
