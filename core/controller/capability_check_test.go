//nolint:testpackage // Exercise the handler with a synthetic routing cache.
package controller

import (
	"encoding/json"
	"maps"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

type diagnosticConfigs map[string]model.ModelConfig

func (configs diagnosticConfigs) GetModelConfig(key string) (model.ModelConfig, bool) {
	c, ok := configs[key]
	return c, ok
}

// Synthetic cache only: no database, credentials, token, or upstream client.
func TestCheckCapabilityResolutionRouting(t *testing.T) {
	native := model.ModelConfig{
		Model: "demo/voice::text-to-speech",
		Type:  mode.NativeTasks,
		Config: map[model.ModelConfigKey]any{
			"capability_contract_version": float64(
				1,
			),
			"public_model":              "demo/voice",
			"capability":                "text-to-speech",
			"public_capability_model":   "demo/voice/text-to-speech",
			"public_api_id":             "demo/voice",
			"public_capability_aliases": []string{"demo/alias"},
		},
	}
	image := model.ModelConfig{
		Model: "demo/image::text-to-image",
		Type:  mode.ImagesGenerations,
		Config: map[model.ModelConfigKey]any{
			"capability_contract_version": float64(
				1,
			),
			"public_model":            "demo/image",
			"capability":              "text-to-image",
			"public_capability_model": "demo/image/text-to-image",
		},
	}
	legacyNative := model.ModelConfig{
		Model:  "demo/old-route",
		Type:   mode.NativeTasks,
		Config: map[model.ModelConfigKey]any{"public_capability_model": "demo/old/text-to-speech"},
	}
	duplicate := native
	duplicate.Model = "demo/duplicate"
	nativeImage := image
	nativeImage.Type = mode.NativeTasks
	duplicateImage := nativeImage
	duplicateImage.Model = "demo/image-duplicate"
	video := model.ModelConfig{
		Model: "demo/video::text-to-video",
		Type:  mode.Videos,
		Config: map[model.ModelConfigKey]any{
			"capability_contract_version": float64(
				1,
			),
			"public_model":            "demo/video",
			"capability":              "text-to-video",
			"public_capability_model": "demo/video/text-to-video",
		},
	}
	aliasClaim := native
	aliasClaim.Model = "demo/alias-claim"
	aliasClaim.Config = maps.Clone(native.Config)
	aliasClaim.Config[model.ModelConfigPublicAPIIDKey] = "demo/other"
	aliasClaim.Config[model.ModelConfigPublicModelKey] = "demo/other"
	aliasClaim.Config[model.ModelConfigPublicCapabilityModelKey] = "demo/other/text-to-speech"
	aliasClaim.Config[model.ModelConfigPublicCapabilityAliasesKey] = []string{
		"demo/voice/text-to-speech",
	}
	cases := []struct {
		name, requested, route, capability string
		configs                            []model.ModelConfig
		mapped, registered                 bool
		channels                           int
	}{
		{
			"native",
			"demo/voice/text-to-speech",
			native.Model,
			"text-to-speech",
			[]model.ModelConfig{native},
			true,
			true,
			1,
		},
		{
			"native public API ID",
			"demo/voice",
			native.Model,
			"text-to-speech",
			[]model.ModelConfig{native},
			true,
			true,
			1,
		},
		{
			"alias",
			"demo/alias",
			native.Model,
			"text-to-speech",
			[]model.ModelConfig{native},
			true,
			true,
			1,
		},
		{
			"legacy protocol",
			"demo/image/text-to-image",
			image.Model,
			"text-to-image",
			[]model.ModelConfig{image},
			true,
			true,
			1,
		},
		{
			"legacy video protocol",
			"demo/video/text-to-video",
			video.Model,
			"text-to-video",
			[]model.ModelConfig{video},
			true,
			true,
			1,
		},
		{
			"legacy native identity",
			"demo/old/text-to-speech",
			legacyNative.Model,
			"",
			[]model.ModelConfig{legacyNative},
			true,
			true,
			1,
		},
		{"missing config", "demo/missing", "demo/missing", "", nil, false, false, 0},
		{
			"ambiguous config",
			"demo/voice",
			"demo/voice",
			"",
			[]model.ModelConfig{native, duplicate},
			false,
			false,
			0,
		},
		{
			"ambiguous canonical ID",
			"demo/voice/text-to-speech",
			"demo/voice/text-to-speech",
			"",
			[]model.ModelConfig{native, duplicate},
			false,
			false,
			0,
		},
		{
			"ambiguous alias",
			"demo/alias",
			"demo/alias",
			"",
			[]model.ModelConfig{native, duplicate},
			false,
			false,
			0,
		},
		{
			"ambiguous native image blocks legacy fallback",
			"demo/image/text-to-image",
			"demo/image/text-to-image",
			"",
			[]model.ModelConfig{nativeImage, duplicateImage},
			false,
			false,
			0,
		},
		{
			"canonical before alias",
			"demo/voice/text-to-speech",
			native.Model,
			"text-to-speech",
			[]model.ModelConfig{native, aliasClaim},
			true,
			true,
			1,
		},
		{
			"no enabled channels",
			"demo/voice",
			native.Model,
			"text-to-speech",
			[]model.ModelConfig{native},
			true,
			true,
			0,
		},
	}
	cache := model.LoadModelCaches()
	previous := *cache
	t.Cleanup(func() { *cache = previous })

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			configs := diagnosticConfigs{}

			channels := map[string][]*model.Channel{}
			for _, c := range tt.configs {
				configs[c.Model] = c
				if tt.name != "no enabled channels" {
					channels[c.Model] = []*model.Channel{{ID: 42}}
				}
			}

			*cache = model.ModelCaches{
				ModelConfig:            configs,
				EnabledModelConfigsMap: configs,
				EnabledModel2ChannelsBySet: map[string]map[string][]*model.Channel{
					"a": channels,
					"b": channels,
				},
			}
			if tt.name == "native" {
				resolved, match := model.ResolveNativeCapabilityRoute(tt.requested, tt.configs)
				require.Equal(t, model.NativeRouteFound, match)
				require.Equal(t, tt.route, resolved.Route)
			}

			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequestWithContext(
				t.Context(),
				"GET",
				"/?model="+url.QueryEscape(tt.requested),
				nil,
			)
			CheckCapabilityResolution(c)
			require.Equal(t, 200, recorder.Code)

			var response struct {
				Data map[string]any `json:"data"`
			}
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
			t.Logf("request=%s response=%s", tt.requested, recorder.Body.String())
			require.Equal(t, tt.route, response.Data["routing_model"])
			require.Equal(t, tt.capability, response.Data["capability"])
			require.Equal(t, tt.mapped, response.Data["mapped"])
			require.Equal(t, tt.registered, response.Data["registered"])
			require.EqualValues(t, tt.channels, response.Data["enabled_channels"])
			require.Equal(t, tt.requested, response.Data["public_id"])
			require.Equal(
				t,
				tt.mapped && tt.registered && tt.channels > 0,
				response.Data["config_ok"],
			)
			require.Equal(t, "not_attempted", response.Data["generation_status"])
			require.Equal(t, "global_enabled_config", response.Data["check_scope"])

			status := "unmapped"
			if tt.mapped {
				status = "resolved"
			} else if len(tt.configs) > 1 {
				status = "ambiguous"
			}

			require.Equal(t, status, response.Data["resolution_status"])
		})
	}
}
