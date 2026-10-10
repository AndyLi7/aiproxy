//nolint:testpackage
package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

func publicCapabilityModelConfig(internal, publicModel, capability string) model.ModelConfig {
	return model.ModelConfig{
		Model: internal,
		Owner: "ByteDance",
		Config: map[model.ModelConfigKey]any{
			model.ModelConfigCapabilityContractVersionKey: 1,
			model.ModelConfigPublicModelKey:               publicModel,
			model.ModelConfigPublicCapabilityModelKey:     publicModel + "/" + capability,
			model.ModelConfigCapabilityKey:                capability,
			model.ModelConfigParameterSchemaKey: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prompt": map[string]any{"type": "string"},
				},
				"required":             []string{"prompt"},
				"additionalProperties": false,
			},
			model.ModelConfigDefaultParametersKey: map[string]any{},
		},
	}
}

func TestListModelsProjectsEntitledCapabilityModelsWithoutInternalIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	publicModel := "bytedance/seedance-1-0-pro"
	textInternal := publicModel + "::text-to-video"
	imageInternal := publicModel + "::image-to-video"
	legacyModel := "openai/gpt-5"
	models := []string{textInternal, imageInternal, legacyModel}

	token := model.TokenCache{Models: models}
	token.SetAvailableSets([]string{model.ChannelDefaultSet})
	token.SetModelsBySet(map[string][]string{model.ChannelDefaultSet: models})

	caches := &model.ModelCaches{
		EnabledModelConfigsMap: map[string]model.ModelConfig{
			textInternal: publicCapabilityModelConfig(
				textInternal,
				publicModel,
				"text-to-video",
			),
			imageInternal: publicCapabilityModelConfig(
				imageInternal,
				publicModel,
				"image-to-video",
			),
			legacyModel: {Model: legacyModel, Owner: "OpenAI"},
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"/v1/models",
		nil,
	)
	ctx.Set(middleware.Token, token)
	ctx.Set(middleware.ModelCaches, caches)

	ListModels(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)

	var body struct {
		Data []OpenAIModels `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))

	ids := make([]string, 0, len(body.Data))
	for _, entry := range body.Data {
		ids = append(ids, entry.ID)
		require.False(t, strings.Contains(entry.ID, "::"))
	}

	require.ElementsMatch(t, []string{
		publicModel + "/text-to-video",
		publicModel + "/image-to-video",
		legacyModel,
	}, ids)
}

func nativePublicIDConfig(publicModel, capability string, extra map[model.ModelConfigKey]any) model.ModelConfig {
	config := map[model.ModelConfigKey]any{
		model.ModelConfigCapabilityContractVersionKey: float64(1),
		model.ModelConfigPublicModelKey:               publicModel,
		model.ModelConfigPublicCapabilityModelKey:     publicModel + "/" + capability,
		model.ModelConfigCapabilityKey:                capability,
		NativeResultConfigKey:                         map[string]any{"version": 1},
	}
	for key, value := range extra {
		config[key] = value
	}

	return model.ModelConfig{Model: publicModel + "::" + capability, Type: mode.NativeTasks, Config: config}
}

func publicIDModelContext(t *testing.T, path string, configs ...model.ModelConfig) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	names := make([]string, 0, len(configs))
	enabled := make(map[string]model.ModelConfig, len(configs))
	for _, config := range configs {
		names = append(names, config.Model)
		enabled[config.Model] = config
	}

	token := model.TokenCache{}
	token.SetAvailableSets([]string{model.ChannelDefaultSet})
	token.SetModelsBySet(map[string][]string{model.ChannelDefaultSet: names})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	ctx.Set(middleware.Token, token)
	ctx.Set(middleware.ModelCaches, &model.ModelCaches{EnabledModelConfigsMap: enabled})

	return ctx, recorder
}

// Native configs carry no parameter_schema, so the list used to fall back to
// their internal route keys. It lists the ID customers call each model by;
// capability IDs behind a public_api_id and aliases stay hidden.
func TestListModelsListsCallableIDsAndNeverRouteKeys(t *testing.T) {
	ctx, recorder := publicIDModelContext(t, "/v1/models",
		nativePublicIDConfig("elevenlabs/eleven-v4", "text-to-speech", map[model.ModelConfigKey]any{
			model.ModelConfigPublicAPIIDKey:             "elevenlabs/eleven-v4",
			model.ModelConfigPublicCapabilityAliasesKey: []any{"elevenlabs/eleven-4"},
		}),
		nativePublicIDConfig("alibaba/wan-2.7", "text-to-image", nil),
		// An invalid public_api_id is ignored: the capability ID is listed.
		nativePublicIDConfig("elevenlabs/eleven-v3", "text-to-speech", map[model.ModelConfigKey]any{
			model.ModelConfigPublicAPIIDKey: "elevenlabs/other",
		}),
		model.ModelConfig{Model: "vendor/legacy::text-to-image", Type: mode.ImagesGenerations},
		model.ModelConfig{Model: "openai/gpt-5", Owner: "OpenAI"},
	)

	ListModels(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	var body struct {
		Data []OpenAIModels `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	ids := make([]string, 0, len(body.Data))
	for _, entry := range body.Data {
		ids = append(ids, entry.ID)
	}
	require.Equal(t, []string{
		"alibaba/wan-2.7/text-to-image",
		"elevenlabs/eleven-v3/text-to-speech",
		"elevenlabs/eleven-v4",
		"openai/gpt-5",
	}, ids)
	require.NotContains(t, recorder.Body.String(), "::")
	require.NotContains(t, recorder.Body.String(), "eleven-4")
	require.NotContains(t, recorder.Body.String(), "elevenlabs/eleven-v4/text-to-speech")
}

func TestRetrieveModelAcceptsEveryCallableID(t *testing.T) {
	tts := nativePublicIDConfig("elevenlabs/eleven-v4", "text-to-speech", map[model.ModelConfigKey]any{
		model.ModelConfigPublicAPIIDKey:             "elevenlabs/eleven-v4",
		model.ModelConfigPublicCapabilityAliasesKey: []any{"elevenlabs/eleven-4"},
	})
	for _, id := range []string{"elevenlabs/eleven-v4", "elevenlabs/eleven-v4/text-to-speech", "elevenlabs/eleven-4", "ElevenLabs/Eleven-V4"} {
		ctx, recorder := publicIDModelContext(t, "/v1/models/"+id, tts)
		ctx.Params = gin.Params{{Key: "model", Value: id}}
		RetrieveModel(ctx)
		require.Equal(t, http.StatusOK, recorder.Code, id)
		var entry OpenAIModels
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &entry))
		require.Equal(t, "elevenlabs/eleven-v4", entry.ID, id)
	}

	for _, id := range []string{"elevenlabs/eleven-v9", "elevenlabs/eleven-v4::text-to-speech", "ElevenLabs/Eleven-4"} {
		ctx, recorder := publicIDModelContext(t, "/v1/models/"+id, tts)
		ctx.Params = gin.Params{{Key: "model", Value: id}}
		RetrieveModel(ctx)
		require.Equal(t, http.StatusNotFound, recorder.Code, id)
		var body struct {
			Error struct {
				Code  string `json:"code"`
				Type  string `json:"type"`
				Param string `json:"param"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
		require.Equal(t, "model_not_found", body.Error.Code)
		require.Equal(t, "not_found_error", body.Error.Type)
		require.Equal(t, "model", body.Error.Param)
		require.NotContains(t, recorder.Body.String(), "::")
	}
}

// Task responses name the callable ID (owner decision D2), on GET too, where
// the distributor does not run; the stored task keeps the capability ID.
func TestNativeTaskResponsesNameTheCallableID(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "native.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}))
	previousDB, previousWallet := model.LogDB, balance.Default
	t.Cleanup(func() { model.LogDB = previousDB; balance.Default = previousWallet })
	model.LogDB, balance.Default = db, nativeControllerWallet{}
	for _, task := range []model.NativeTask{
		{ID: "tts", Model: "elevenlabs/eleven-v4/text-to-speech"},
		{ID: "gone", Model: "vendor/removed/text-to-speech"},
	} {
		task.GroupID, task.TokenID, task.Fingerprint, task.OutputSchema, task.OutputSchemaHash = "owner", 7, "fp", `{}`, "sha"
		task.Status, task.DeliveredOutput, task.BillingOperationID = "completed", "null", "native:"+task.ID
		require.NoError(t, db.Create(&task).Error)
	}
	tts := nativePublicIDConfig("elevenlabs/eleven-v4", "text-to-speech", map[model.ModelConfigKey]any{
		model.ModelConfigPublicAPIIDKey: "elevenlabs/eleven-v4",
	})

	for id, want := range map[string]string{"tts": "elevenlabs/eleven-v4", "gone": "vendor/removed/text-to-speech"} {
		router := gin.New()
		router.Use(func(c *gin.Context) {
			c.Set(middleware.Group, model.GroupCache{ID: "owner"})
			c.Set(middleware.Token, model.TokenCache{ID: 7})
			c.Set(middleware.ModelCaches, &model.ModelCaches{
				EnabledModelConfigsMap: map[string]model.ModelConfig{tts.Model: tts},
			})
			c.Next()
		})
		router.GET("/v1/model-tasks/:id", GetNativeTask)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/model-tasks/"+id, nil))
		require.Equal(t, http.StatusOK, response.Code)
		var body struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
		require.Equal(t, want, body.Model, id)
	}
}

func TestNativeAcceptedModelOnlyForResolvedAcceptedIDs(t *testing.T) {
	tts := nativePublicIDConfig("elevenlabs/eleven-v4", "text-to-speech", map[model.ModelConfigKey]any{
		model.ModelConfigPublicAPIIDKey:             "elevenlabs/eleven-v4",
		model.ModelConfigPublicCapabilityAliasesKey: []any{"elevenlabs/eleven-4"},
	})
	for requested, want := range map[string]string{
		"elevenlabs/eleven-v4":                "elevenlabs/eleven-v4",
		"elevenlabs/eleven-4":                 "elevenlabs/eleven-4",
		"elevenlabs/eleven-v4/text-to-speech": "",
		"elevenlabs/eleven-v9":                "",
		"":                                    "",
	} {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Set(middleware.RequestedModel, requested)
		require.Equal(t, want, nativeAcceptedModel(ctx, tts), requested)
	}
}
