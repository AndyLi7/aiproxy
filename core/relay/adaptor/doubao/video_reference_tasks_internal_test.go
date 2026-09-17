package doubao

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	coremodel "github.com/labring/aiproxy/core/model"
	relaymeta "github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

func referenceTaskMeta() *relaymeta.Meta {
	m := relaymeta.NewMeta(
		nil,
		mode.Videos,
		"bytedance/seedance-2.5/reference-to-video",
		coremodel.ModelConfig{
			Config: map[coremodel.ModelConfigKey]any{
				coremodel.ModelConfigCapabilityContractVersionKey: 1,
				coremodel.ModelConfigPublicModelKey:               "bytedance/seedance-2.5",
				coremodel.ModelConfigPublicCapabilityModelKey:     "bytedance/seedance-2.5/reference-to-video",
				coremodel.ModelConfigCapabilityKey:                "reference-to-video",
				coremodel.ModelConfigParameterSchemaKey: map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
				coremodel.ModelConfigDefaultParametersKey: map[string]any{},
			},
		},
	)
	m.ActualModel = "doubao-seedance-2-5-260628"

	return m
}

func TestReferenceTaskConvertsPublicFields(t *testing.T) {
	for _, task := range []string{"reference", "edit", "extend"} {
		t.Run(task, func(t *testing.T) {
			payload := map[string]any{
				"prompt":       "Change this scene",
				"task_type":    task,
				"seconds":      -1,
				"aspect_ratio": "adaptive",
				"video_urls":   []string{"https://example.com/source.mp4"},
				"image_urls":   []string{"https://example.com/ref.png"},
				"audio_urls":   []string{"https://example.com/ref.wav"},
			}
			data, err := json.Marshal(payload)
			require.NoError(t, err)

			req := httptest.NewRequestWithContext(
				context.Background(),
				http.MethodPost,
				"/v1/videos",
				bytes.NewReader(data),
			)
			req.Header.Set("Content-Type", "application/json")
			result, err := ConvertVideosRequest(referenceTaskMeta(), req)
			require.NoError(t, err)
			body, err := io.ReadAll(result.Body)
			require.NoError(t, err)

			var upstream map[string]any
			require.NoError(t, json.Unmarshal(body, &upstream))
			require.Equal(t, task, upstream["omni_reference_task_type"])
			require.Equal(t, float64(-1), upstream["duration"])
			require.Equal(t, "mp4", upstream["output_format"])
			content, ok := upstream["content"].([]any)
			require.True(t, ok)
			require.Len(t, content, 4)
			require.Equal(t, "reference_image", referenceContentRole(t, content[1]))
			require.Equal(t, "reference_video", referenceContentRole(t, content[2]))
			require.Equal(t, "reference_audio", referenceContentRole(t, content[3]))
		})
	}
}

func TestReferenceTaskRejectsInvalidPublicParameters(t *testing.T) {
	for _, payload := range []string{
		`{"prompt":"Edit scene","task_type":"edit","video_urls":["https://example.com/v.mp4"],"seconds":5}`,
		`{"prompt":"Extend scene","task_type":"extend","image_urls":["https://example.com/i.png"]}`,
		`{"prompt":"Edit scene","task_type":"edit","video_urls":["https://example.com/v.mp4"],"aspect_ratio":"16:9"}`,
		`{"prompt":"Scene","video_urls":["http://example.com/v.mp4"]}`,
		`{"prompt":"Scene","video_urls":[null]}`,
		`{"prompt":"Scene","video_urls":[]}`,
		`{"prompt":"Scene","audio_urls":["https://example.com/a.wav"],"seconds":3}`,
		`{"prompt":"Scene","audio_urls":["https://example.com/a.wav"],"task_type":"auto"}`,
		`{"prompt":"Scene","audio_urls":["https://example.com/a.wav"],"output_format":"mov"}`,
		`{"prompt":"Scene","audio_urls":["https://example.com/a.wav"],"omni_reference_task_type":"edit"}`,
		`{"content":[{"type":"video_url","role":"reference_video","video_url":{"url":"https://example.com/v.mp4"}}]}`,
	} {
		req := httptest.NewRequestWithContext(
			context.Background(),
			http.MethodPost,
			"/v1/videos",
			bytes.NewBufferString(payload),
		)
		req.Header.Set("Content-Type", "application/json")
		_, err := ConvertVideosRequest(referenceTaskMeta(), req)
		require.Error(t, err, payload)
	}
}

func TestReferenceTaskSupportsAudioOnlyAndRejectsTooManyReferences(t *testing.T) {
	for _, count := range []int{1, 10, 11} {
		refs := make([]string, count)
		for i := range refs {
			refs[i] = "https://example.com/ref.wav"
		}

		payload, err := json.Marshal(map[string]any{"prompt": "A scene", "audio_urls": refs})
		require.NoError(t, err)

		req := httptest.NewRequestWithContext(
			context.Background(),
			http.MethodPost,
			"/v1/videos",
			bytes.NewReader(payload),
		)
		req.Header.Set("Content-Type", "application/json")

		_, err = ConvertVideosRequest(referenceTaskMeta(), req)
		if count > 10 {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
	}
}

func referenceContentRole(t *testing.T, value any) any {
	t.Helper()

	content, ok := value.(map[string]any)
	require.True(t, ok)

	return content["role"]
}
