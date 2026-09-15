package doubao

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf16"

	"github.com/labring/aiproxy/core/common"
	coremodel "github.com/labring/aiproxy/core/model"
	relaymeta "github.com/labring/aiproxy/core/relay/meta"
)

// Resolve from server-owned capability metadata, never from the client's model
// string. Keep new task semantics out of legacy video and extension routes.
func isSeedance25ReferenceCapability(meta *relaymeta.Meta) bool {
	if meta == nil {
		return false
	}
	contract, ok := coremodel.CapabilityRoutingMetadataFromConfig(meta.ModelConfig)
	return ok && contract.PublicCapabilityModel == "bytedance/seedance-2.5/reference-to-video"
}

func parseSeedance25ReferenceRequest(req *http.Request) (doubaoVideoRequest, error) {
	var raw map[string]json.RawMessage
	if err := common.UnmarshalRequestReusable(req, &raw); err != nil {
		return doubaoVideoRequest{}, err
	}
	allowed := map[string]bool{"model": true, "prompt": true, "task_type": true, "image_urls": true, "video_urls": true, "audio_urls": true, "resolution": true, "seconds": true, "aspect_ratio": true, "generate_audio": true}
	for key, value := range raw {
		if !allowed[key] {
			return doubaoVideoRequest{}, fmt.Errorf("unsupported reference parameter: %s", key)
		}
		if strings.TrimSpace(string(value)) == "null" {
			return doubaoVideoRequest{}, fmt.Errorf("reference parameter %s must not be null", key)
		}
	}
	read := func(key string, destination any) error {
		if value, ok := raw[key]; ok {
			return json.Unmarshal(value, destination)
		}
		return nil
	}
	prompt, task, resolution, ratio := "", "reference", "720p", "adaptive"
	seconds, audio := -1, true
	for key, dest := range map[string]any{"prompt": &prompt, "task_type": &task, "resolution": &resolution, "aspect_ratio": &ratio, "seconds": &seconds, "generate_audio": &audio} {
		if err := read(key, dest); err != nil {
			return doubaoVideoRequest{}, fmt.Errorf("invalid reference parameter %s", key)
		}
	}
	if strings.TrimSpace(prompt) == "" || len(utf16.Encode([]rune(prompt))) > 2000 {
		return doubaoVideoRequest{}, fmt.Errorf("prompt must contain 1 to 2000 characters")
	}
	if task != "reference" && task != "edit" && task != "extend" {
		return doubaoVideoRequest{}, fmt.Errorf("invalid reference task type")
	}
	if seconds != -1 && (seconds < 4 || seconds > 30) {
		return doubaoVideoRequest{}, fmt.Errorf("reference duration must be -1 or 4 to 30 seconds")
	}
	if task == "edit" && seconds != -1 {
		return doubaoVideoRequest{}, fmt.Errorf("editing requires automatic duration")
	}
	if resolution != "480p" && resolution != "720p" && resolution != "1080p" {
		return doubaoVideoRequest{}, fmt.Errorf("unsupported reference resolution")
	}
	validRatios := map[string]bool{"adaptive": true, "16:9": true, "4:3": true, "1:1": true, "3:4": true, "9:16": true, "21:9": true}
	if !validRatios[ratio] || (task != "reference" && ratio != "adaptive") {
		return doubaoVideoRequest{}, fmt.Errorf("invalid aspect ratio for reference task")
	}
	request := doubaoVideoRequest{
		Content:               []doubaoVideoContent{{Type: "text", Text: prompt}},
		OmniReferenceTaskType: task, OutputFormat: "mp4", Resolution: resolution,
		Ratio: ratio, Duration: &seconds, GenerateAudio: &audio,
	}
	total, videos := 0, 0
	for _, field := range []struct {
		key, kind, role string
		max             int
	}{
		{"image_urls", "image_url", "reference_image", 30},
		{"video_urls", "video_url", "reference_video", 10},
		{"audio_urls", "audio_url", "reference_audio", 10},
	} {
		if _, exists := raw[field.key]; !exists {
			continue
		}
		var refs []string
		if err := read(field.key, &refs); err != nil || len(refs) == 0 || len(refs) > field.max {
			return doubaoVideoRequest{}, fmt.Errorf("invalid %s reference list", field.key)
		}
		for _, ref := range refs {
			parsed, err := url.Parse(ref)
			if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
				return doubaoVideoRequest{}, fmt.Errorf("%s references require HTTPS URLs", field.key)
			}
			item := doubaoVideoContent{Type: field.kind, Role: field.role}
			reference := &doubaoVideoURLContent{URL: ref}
			switch field.kind {
			case "image_url":
				item.ImageURL = reference
			case "video_url":
				item.VideoURL = reference
				videos++
			case "audio_url":
				item.AudioURL = reference
			}
			request.Content = append(request.Content, item)
		}
		total += len(refs)
	}
	if total == 0 || total > 50 {
		return doubaoVideoRequest{}, fmt.Errorf("provide 1 to 50 references")
	}
	if task != "reference" && videos == 0 {
		return doubaoVideoRequest{}, fmt.Errorf("editing and extension require a reference video")
	}
	return request, nil
}
