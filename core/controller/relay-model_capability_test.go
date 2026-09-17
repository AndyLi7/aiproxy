//nolint:testpackage
package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
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
		publicModel,
		publicModel + "/text-to-video",
		publicModel + "/image-to-video",
		legacyModel,
	}, ids)
}
