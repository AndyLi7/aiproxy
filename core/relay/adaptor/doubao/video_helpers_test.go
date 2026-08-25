package doubao

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	coremodel "github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	relaymeta "github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
	relaymodel "github.com/labring/aiproxy/core/relay/model"
	"github.com/stretchr/testify/require"
)

func TestConvertVideosRequestAppliesModelAudioDefault(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/videos",
		bytes.NewBufferString(`{"prompt":"a cat","size":"854x480","seconds":2}`),
	)
	request.Header.Set("Content-Type", "application/json")
	meta := relaymeta.NewMeta(
		nil,
		mode.Videos,
		"doubao-seedance-1-0-pro-250528",
		coremodel.ModelConfig{
			Config: map[coremodel.ModelConfigKey]any{
				"audio": map[string]any{
					"mode":           "none",
					"defaultEnabled": false,
				},
			},
		},
	)

	converted, err := ConvertVideosRequest(meta, request)
	require.NoError(t, err)
	body, err := io.ReadAll(converted.Body)
	require.NoError(t, err)
	var upstream doubaoVideoRequest
	require.NoError(t, json.Unmarshal(body, &upstream))
	require.NotNil(t, upstream.GenerateAudio)
	require.False(t, *upstream.GenerateAudio)
	require.NotNil(t, doubaoVideoMetadataFromMeta(meta).OutputAudio)
	require.False(t, *doubaoVideoMetadataFromMeta(meta).OutputAudio)
}

func TestBuildDoubaoVideoEchoesEffectiveRequestAudio(t *testing.T) {
	t.Parallel()

	effectiveAudio := false
	meta := relaymeta.NewMeta(
		nil,
		mode.Videos,
		"bytedance/seedance-1.0-pro",
		coremodel.ModelConfig{},
	)
	setDoubaoVideoMetadata(meta, doubaoVideoStoreMetadata{
		OutputAudio: &effectiveAudio,
	})

	upstreamAudio := true
	video := buildDoubaoVideo(meta, "task-1", &relaymodel.DoubaoVideoTaskResponse{
		GenerateAudio: &upstreamAudio,
	})

	require.NotNil(t, video.GenerateAudio)
	require.False(t, *video.GenerateAudio)
}

func TestStoredDoubaoVideoAudioOverridesConflictingUpstreamValue(t *testing.T) {
	t.Parallel()

	store := &doubaoTestStore{saved: []adaptor.StoreCache{
		{ID: "video:task-1", Metadata: `{"output_audio":false}`},
	}}
	meta := relaymeta.NewMeta(
		nil,
		mode.Videos,
		"bytedance/seedance-1.0-pro",
		coremodel.ModelConfig{},
	)
	upstreamAudio := true
	response := relaymodel.DoubaoVideoTaskResponse{GenerateAudio: &upstreamAudio}

	applyStoredDoubaoVideoMetadata(meta, store, "video:task-1", &response)

	require.NotNil(t, response.GenerateAudio)
	require.False(t, *response.GenerateAudio)
}

func TestEffectiveDoubaoVideoOutputAudioUsesOptionalModelDefault(t *testing.T) {
	t.Parallel()

	meta := relaymeta.NewMeta(
		nil,
		mode.Videos,
		"doubao-seedance-optional",
		coremodel.ModelConfig{
			Config: map[coremodel.ModelConfigKey]any{
				"audio": map[string]any{
					"mode":           "optional",
					"defaultEnabled": false,
				},
			},
		},
	)

	effective := effectiveDoubaoVideoOutputAudio(meta, nil)

	require.NotNil(t, effective)
	require.False(t, *effective)
}

func TestRatioFromSizeRecognizesCanonicalRoundedVideoDimensions(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"854x480":  "16:9",
		"480x854":  "9:16",
		"1280x720": "16:9",
		"720x1280": "9:16",
		"480x480":  "1:1",
	}

	for size, expected := range tests {
		size, expected := size, expected
		t.Run(size, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, expected, ratioFromSize(size))
		})
	}
}

func TestDoubaoVideoDimensionsUsesCanonicalRoundedSixteenByNineSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		resolution string
		ratio      string
		wantWidth  int
		wantHeight int
	}{
		{resolution: "480p", ratio: "16:9", wantWidth: 854, wantHeight: 480},
		{resolution: "480p", ratio: "9:16", wantWidth: 480, wantHeight: 854},
		{resolution: "720p", ratio: "16:9", wantWidth: 1280, wantHeight: 720},
		{resolution: "1080p", ratio: "16:9", wantWidth: 1920, wantHeight: 1080},
	}

	for _, test := range tests {
		width, height := doubaoVideoDimensions(test.resolution, test.ratio)
		require.Equal(t, test.wantWidth, width)
		require.Equal(t, test.wantHeight, height)
	}
}
