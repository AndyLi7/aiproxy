package model_test

import (
	"strings"
	"testing"

	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

func nativeConfig(route, publicID string, m mode.Mode) model.ModelConfig {
	return model.ModelConfig{
		Model:  route,
		Type:   m,
		Config: map[model.ModelConfigKey]any{model.ModelConfigPublicCapabilityModelKey: publicID},
	}
}

// capabilityConfig is a published capability config as the application writes
// it (JSON numbers decode as float64), plus extra keys.
func capabilityConfig(
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

func ttsConfig(extra map[model.ModelConfigKey]any) model.ModelConfig {
	return capabilityConfig(mode.NativeTasks, "elevenlabs/eleven-v4", "text-to-speech", extra)
}

func withPublicAPIID(id string, aliases ...any) map[model.ModelConfigKey]any {
	extra := map[model.ModelConfigKey]any{model.ModelConfigPublicAPIIDKey: id}
	if aliases != nil {
		extra[model.ModelConfigPublicCapabilityAliasesKey] = aliases
	}

	return extra
}

func TestPublicCapabilityIdentityFromConfig(t *testing.T) {
	const ck = "elevenlabs/eleven-v4/text-to-speech"

	// Without the new keys the callable ID is the capability ID, as today.
	identity, ok := model.PublicCapabilityIdentityFromConfig(ttsConfig(nil))
	require.True(t, ok)
	require.Equal(t, ck, identity.Callable())
	require.Equal(t, []string{ck}, identity.Accepted())
	require.Equal(t, "elevenlabs/eleven-v4", identity.PublicModel)
	require.Equal(t, "text-to-speech", identity.Capability)

	// public_api_id equal to public_model: the audio model ID.
	identity, ok = model.PublicCapabilityIdentityFromConfig(ttsConfig(withPublicAPIID("elevenlabs/eleven-v4")))
	require.True(t, ok)
	require.Equal(t, "elevenlabs/eleven-v4", identity.Callable())
	require.Equal(t, []string{ck, "elevenlabs/eleven-v4"}, identity.Accepted())

	// public_api_id equal to the capability ID adds nothing.
	identity, _ = model.PublicCapabilityIdentityFromConfig(ttsConfig(withPublicAPIID(ck)))
	require.Equal(t, ck, identity.Callable())
	require.Equal(t, []string{ck}, identity.Accepted())

	// Any other public_api_id is ignored; the capability ID keeps working.
	for _, invalid := range []any{"elevenlabs/eleven-v5", "elevenlabs", "Elevenlabs/Eleven-V4", 7, nil} {
		identity, ok = model.PublicCapabilityIdentityFromConfig(ttsConfig(
			map[model.ModelConfigKey]any{model.ModelConfigPublicAPIIDKey: invalid},
		))
		require.True(t, ok)
		require.Equal(t, ck, identity.Callable(), "%v", invalid)
		require.Equal(t, []string{ck}, identity.Accepted(), "%v", invalid)
	}

	// Aliases: valid ones kept once; every invalid item ignored.
	identity, _ = model.PublicCapabilityIdentityFromConfig(ttsConfig(withPublicAPIID(
		"elevenlabs/eleven-v4",
		"elevenlabs/eleven-4",                 // valid, 2 segments
		"elevenlabs/eleven-4/text-to-speech",  // valid, 3 segments
		"elevenlabs/eleven-4",                 // duplicate
		"elevenlabs",                          // 1 segment
		"a/b/c/d",                             // 4 segments
		"elevenlabs/eleven-4::text-to-speech", // route key
		"elevenlabs/eleven 4",                 // whitespace
		"elevenlabs//text-to-speech",          // empty segment
		"x/"+strings.Repeat("y", 191),         // too long
		ck,                                    // the capability ID
		"elevenlabs/eleven-v4",                // the public_api_id
		42,                                    // not a string
	)))
	require.Equal(t, "elevenlabs/eleven-v4", identity.Callable())
	require.Equal(t, []string{"elevenlabs/eleven-4", "elevenlabs/eleven-4/text-to-speech"}, identity.Aliases)
	require.True(t, identity.Accepts("elevenlabs/eleven-4"))
	require.False(t, identity.Accepts("ElevenLabs/Eleven-4"))

	// Aliases of the wrong type are ignored as a whole.
	identity, _ = model.PublicCapabilityIdentityFromConfig(ttsConfig(
		map[model.ModelConfigKey]any{model.ModelConfigPublicCapabilityAliasesKey: "elevenlabs/eleven-4"},
	))
	require.Empty(t, identity.Aliases)

	// Only native task configs may declare public IDs (images never resolve them).
	image := capabilityConfig(mode.ImagesGenerations, "bytedance/seedream-4.5", "text-to-image",
		withPublicAPIID("bytedance/seedream-4.5", "bytedance/seedream-4"))
	identity, ok = model.PublicCapabilityIdentityFromConfig(image)
	require.True(t, ok)
	require.Equal(t, "bytedance/seedream-4.5/text-to-image", identity.Callable())
	require.Empty(t, identity.Aliases)

	// parameter_schema is not needed; the identity itself must be consistent.
	broken := ttsConfig(nil)
	broken.Config[model.ModelConfigPublicCapabilityModelKey] = "elevenlabs/eleven-v4/other"
	_, ok = model.PublicCapabilityIdentityFromConfig(broken)
	require.False(t, ok)

	unversioned := ttsConfig(nil)
	delete(unversioned.Config, model.ModelConfigCapabilityContractVersionKey)
	_, ok = model.PublicCapabilityIdentityFromConfig(unversioned)
	require.False(t, ok)
}

func TestResolveNativeCapabilityRoute(t *testing.T) {
	flux := nativeConfig("vendor/flux::text-to-image", "vendor/flux/text-to-image", mode.NativeTasks)
	image := nativeConfig("vendor/img::text-to-image", "vendor/img/text-to-image", mode.ImagesGenerations)
	entitled := []model.ModelConfig{flux, image}

	resolved, match := model.ResolveNativeCapabilityRoute("vendor/flux/text-to-image", entitled)
	require.Equal(t, model.NativeRouteFound, match)
	require.Equal(t, "vendor/flux::text-to-image", resolved.Route)
	require.False(t, resolved.HasIdentity, "legacy config with only public_capability_model")

	// Only native configs are eligible; image capabilities keep their own resolver.
	_, match = model.ResolveNativeCapabilityRoute("vendor/img/text-to-image", entitled)
	require.Equal(t, model.NativeRouteNotFound, match)
	_, match = model.ResolveNativeCapabilityRoute("vendor/unknown/text-to-image", entitled)
	require.Equal(t, model.NativeRouteNotFound, match)

	// Two native configs claiming the same public id are never guessed between.
	copied := nativeConfig("vendor/flux::copy", "vendor/flux/text-to-image", mode.NativeTasks)
	_, match = model.ResolveNativeCapabilityRoute("vendor/flux/text-to-image", append(entitled, copied))
	require.Equal(t, model.NativeRouteAmbiguous, match)
}

func TestResolveNativeCapabilityRouteAcceptedIDs(t *testing.T) {
	const ck = "elevenlabs/eleven-v4/text-to-speech"
	tts := ttsConfig(withPublicAPIID("elevenlabs/eleven-v4", "elevenlabs/eleven-4"))
	wan := capabilityConfig(mode.NativeTasks, "alibaba/wan-2.7", "text-to-image", nil)
	entitled := []model.ModelConfig{tts, wan}

	for _, id := range []string{ck, "elevenlabs/eleven-v4", "elevenlabs/eleven-4"} {
		resolved, match := model.ResolveNativeCapabilityRoute(id, entitled)
		require.Equal(t, model.NativeRouteFound, match, id)
		require.Equal(t, "elevenlabs/eleven-v4::text-to-speech", resolved.Route, id)
		require.True(t, resolved.HasIdentity)
		require.Equal(t, ck, resolved.Identity.CapabilityModel)
		require.Equal(t, "elevenlabs/eleven-v4", resolved.Identity.PublicModel)
	}

	// Exact and case-sensitive; a group ID of an image/video model selects nothing.
	for _, id := range []string{"Elevenlabs/Eleven-V4", "elevenlabs/eleven-v4/", "alibaba/wan-2.7", "elevenlabs/eleven-v4/text-to-speech-2"} {
		_, match := model.ResolveNativeCapabilityRoute(id, entitled)
		require.Equal(t, model.NativeRouteNotFound, match, id)
	}

	// Only configs the key may call are searched.
	_, match := model.ResolveNativeCapabilityRoute("elevenlabs/eleven-v4", []model.ModelConfig{wan})
	require.Equal(t, model.NativeRouteNotFound, match)

	// An invalid public_api_id is ignored: only the capability ID calls it.
	invalid := ttsConfig(withPublicAPIID("elevenlabs/eleven-v5"))
	_, match = model.ResolveNativeCapabilityRoute("elevenlabs/eleven-v5", []model.ModelConfig{invalid})
	require.Equal(t, model.NativeRouteNotFound, match)
	_, match = model.ResolveNativeCapabilityRoute(ck, []model.ModelConfig{invalid})
	require.Equal(t, model.NativeRouteFound, match)

	// The same public_api_id on two configs is a configuration error.
	other := capabilityConfig(mode.NativeTasks, "elevenlabs/eleven-v4", "text-to-music",
		withPublicAPIID("elevenlabs/eleven-v4"))
	_, match = model.ResolveNativeCapabilityRoute("elevenlabs/eleven-v4", []model.ModelConfig{tts, other})
	require.Equal(t, model.NativeRouteAmbiguous, match)
	// The capability IDs still resolve.
	resolved, match := model.ResolveNativeCapabilityRoute(ck, []model.ModelConfig{tts, other})
	require.Equal(t, model.NativeRouteFound, match)
	require.Equal(t, "elevenlabs/eleven-v4::text-to-speech", resolved.Route)

	// Priority: a capability ID wins over another config's alias.
	renamed := capabilityConfig(mode.NativeTasks, "elevenlabs/eleven-v5", "text-to-speech",
		withPublicAPIID("elevenlabs/eleven-v5", ck))
	resolved, match = model.ResolveNativeCapabilityRoute(ck, []model.ModelConfig{renamed, tts})
	require.Equal(t, model.NativeRouteFound, match)
	require.Equal(t, "elevenlabs/eleven-v4::text-to-speech", resolved.Route)
	resolved, match = model.ResolveNativeCapabilityRoute(ck, []model.ModelConfig{renamed})
	require.Equal(t, model.NativeRouteFound, match, "an alias keeps an old ID callable")
	require.Equal(t, "elevenlabs/eleven-v5::text-to-speech", resolved.Route)
}

func TestSuggestNativeModels(t *testing.T) {
	tts := ttsConfig(withPublicAPIID("elevenlabs/eleven-v4", "elevenlabs/eleven-4"))
	wanText := capabilityConfig(mode.NativeTasks, "alibaba/wan-2.7", "text-to-image", nil)
	wanImage := capabilityConfig(mode.NativeTasks, "alibaba/wan-2.7", "image-to-image", nil)
	seedream := capabilityConfig(mode.ImagesGenerations, "bytedance/seedream-4.5", "text-to-image", nil)
	chat := model.ModelConfig{Model: "openai/gpt-5", Type: mode.ChatCompletions}
	internal := model.ModelConfig{Model: "vendor/hidden::text-to-image", Type: mode.ImagesGenerations}
	entitled := []model.ModelConfig{tts, wanText, wanImage, seedream, chat, internal}

	tests := []struct {
		requested string
		other     bool
		want      []string
	}{
		// Rule 0: the model is on another endpoint (capability ID, group ID or model name).
		{"bytedance/seedream-4.5/text-to-image", true, []string{"bytedance/seedream-4.5/text-to-image"}},
		{"bytedance/seedream-4.5", true, []string{"bytedance/seedream-4.5/text-to-image"}},
		{"bytedance/seedream-4.5::text-to-image", true, []string{"bytedance/seedream-4.5/text-to-image"}},
		{"openai/gpt-5", true, []string{"openai/gpt-5"}},
		{"vendor/hidden::text-to-image", true, []string{}},
		// Rule 1: an accepted ID with other letter case; never the alias itself.
		{"Elevenlabs/Eleven-V4", false, []string{"elevenlabs/eleven-v4"}},
		{"ElevenLabs/Eleven-4", false, []string{"elevenlabs/eleven-v4"}},
		{"Alibaba/Wan-2.7/Text-To-Image", false, []string{"alibaba/wan-2.7/text-to-image"}},
		// Rule 2: a native model group ID.
		{"alibaba/wan-2.7", false, []string{"alibaba/wan-2.7/image-to-image", "alibaba/wan-2.7/text-to-image"}},
		// Rule 3: a wrong capability on a known model.
		{"elevenlabs/eleven-v4/text-to-speech-2", false, []string{"elevenlabs/eleven-v4"}},
		{"alibaba/wan-2.7/text-to-video", false, []string{"alibaba/wan-2.7/image-to-image", "alibaba/wan-2.7/text-to-image"}},
		// Rule 4: nothing.
		{"elevenlabs/eleven-v9", false, []string{}},
		{"other/model/text-to-image", false, []string{}},
	}
	for _, test := range tests {
		got := model.SuggestNativeModels(test.requested, entitled)
		require.Equal(t, test.other, got.OtherEndpoint, test.requested)
		require.Equal(t, test.want, got.Models, test.requested)
		for _, id := range got.Models {
			require.NotContains(t, id, "::")
			require.NotEqual(t, "elevenlabs/eleven-4", id, "aliases are never suggested")
		}
	}

	// Only configs the key may call are suggested.
	got := model.SuggestNativeModels("alibaba/wan-2.7", []model.ModelConfig{tts})
	require.Equal(t, []string{}, got.Models)

	// Before the audio model is republished its group ID suggests the capability ID.
	got = model.SuggestNativeModels("elevenlabs/eleven-v4", []model.ModelConfig{ttsConfig(nil)})
	require.False(t, got.OtherEndpoint)
	require.Equal(t, []string{"elevenlabs/eleven-v4/text-to-speech"}, got.Models)

	// At most 10, sorted.
	many := make([]model.ModelConfig, 0, 12)
	for _, capability := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"} {
		many = append(many, capabilityConfig(mode.NativeTasks, "vendor/many", "cap-"+capability, nil))
	}
	got = model.SuggestNativeModels("vendor/many", many)
	require.Len(t, got.Models, 10)
	require.Equal(t, "vendor/many/cap-a", got.Models[0])
}

func TestNativeCallableModelID(t *testing.T) {
	const ck = "elevenlabs/eleven-v4/text-to-speech"
	configs := map[string]model.ModelConfig{
		"elevenlabs/eleven-v4::text-to-speech": ttsConfig(withPublicAPIID("elevenlabs/eleven-v4", "elevenlabs/eleven-4")),
		"alibaba/wan-2.7::text-to-image":       capabilityConfig(mode.NativeTasks, "alibaba/wan-2.7", "text-to-image", nil),
	}
	require.Equal(t, "elevenlabs/eleven-v4", model.NativeCallableModelID(ck, configs))
	require.Equal(t, "alibaba/wan-2.7/text-to-image", model.NativeCallableModelID("alibaba/wan-2.7/text-to-image", configs))
	// No config (renamed, removed or not loaded): the stored ID is returned.
	require.Equal(t, "gone/model/text-to-speech", model.NativeCallableModelID("gone/model/text-to-speech", configs))
	require.Equal(t, "native", model.NativeCallableModelID("native", configs))
	require.Equal(t, ck, model.NativeCallableModelID(ck, nil))
}

func TestListedPublicModelIDNeverListsRouteKeys(t *testing.T) {
	require.Equal(t, "elevenlabs/eleven-v4",
		model.ListedPublicModelID(ttsConfig(withPublicAPIID("elevenlabs/eleven-v4", "elevenlabs/eleven-4"))))
	require.Equal(t, "alibaba/wan-2.7/text-to-image",
		model.ListedPublicModelID(capabilityConfig(mode.NativeTasks, "alibaba/wan-2.7", "text-to-image", nil)))
	require.Equal(t, "vendor/flux/text-to-image",
		model.ListedPublicModelID(nativeConfig("vendor/flux::text-to-image", "vendor/flux/text-to-image", mode.NativeTasks)))
	require.Equal(t, "openai/gpt-5", model.ListedPublicModelID(model.ModelConfig{Model: "openai/gpt-5"}))
	require.Empty(t, model.ListedPublicModelID(model.ModelConfig{Model: "vendor/x::text-to-image"}))
}

func TestImageEndpointsResolveOnlyFullCapabilityIDs(t *testing.T) {
	schema := map[model.ModelConfigKey]any{
		model.ModelConfigParameterSchemaKey: map[string]any{
			"type":       "object",
			"properties": map[string]any{"prompt": map[string]any{"type": "string"}},
		},
		model.ModelConfigDefaultParametersKey: map[string]any{},
	}
	seedream := capabilityConfig(mode.ImagesGenerations, "bytedance/seedream-4.5", "text-to-image", schema)
	configs := []model.ModelConfig{seedream}
	fields := map[string]any{"prompt": "a cat"}

	resolved, err := model.ResolveExactCapabilityModel("bytedance/seedream-4.5/text-to-image", fields, configs)
	require.NoError(t, err)
	require.Equal(t, "bytedance/seedream-4.5::text-to-image", resolved.InternalModel)

	_, err = model.ResolveExactCapabilityModel("bytedance/seedream-4.5", fields, configs)
	var routingErr *model.CapabilityRoutingError
	require.ErrorAs(t, err, &routingErr)
	require.Equal(t, model.CapabilityModelNotFound, routingErr.Code)
	require.Equal(t, []string{"bytedance/seedream-4.5/text-to-image"},
		model.CapabilityGroupSuggestions("bytedance/seedream-4.5", configs))
	require.Empty(t, model.CapabilityGroupSuggestions("bytedance/seedream-4.5/text-to-image", configs))

	// The video endpoints keep the group-ID selection.
	resolved, err = model.ResolveCapabilityModel("bytedance/seedream-4.5", fields, configs)
	require.NoError(t, err)
	require.Equal(t, "bytedance/seedream-4.5::text-to-image", resolved.InternalModel)
}
