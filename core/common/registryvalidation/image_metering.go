package registryvalidation

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"strings"
)

// ImageMetering is server-owned metadata using flat native request parameter names.
type ImageMetering struct {
	Version                        int    `json:"version"`
	InputImagesParameter           string `json:"inputImagesParameter,omitempty"`
	MaxInputImages                 *int64 `json:"maxInputImages,omitempty"`
	OutputCountMultiplierParameter string `json:"outputCountMultiplierParameter,omitempty"`
	MaxCombinedImages              *int64 `json:"maxCombinedImages,omitempty"`
}

var meteringParameter = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type ImageMeteringEvidence struct {
	InputCount     int64
	MaximumOutputs int
	Explicit       bool
}

func ResolveImageMetering(raw []byte, binding ProviderBinding, mapped []byte, n, cap int, required bool) (ImageMeteringEvidence, error) {
	result := ImageMeteringEvidence{MaximumOutputs: n}
	var native map[string]any
	if json.Unmarshal(mapped, &native) != nil || native == nil {
		return result, ErrProviderContract
	}
	batchParameter, hasBatchParameter := native["max_images"]
	hasBatchParameter = hasBatchParameter && batchParameter != float64(1)
	if n < 1 || n > cap {
		return result, ErrProviderContract
	}
	if !HasProviderContracts(raw) {
		if required {
			return result, ErrProviderContract
		}
		return result, nil
	}
	p, err := bound(raw, binding)
	if err != nil {
		return result, err
	}
	if len(p.Upstream.Metering) == 0 {
		if required || hasBatchParameter {
			return result, ErrProviderContract
		}
		return result, nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(p.Upstream.Metering, &fields) != nil || fields == nil {
		return result, ErrProviderContract
	}
	for _, key := range []string{"inputImagesParameter", "outputCountMultiplierParameter", "maxInputImages", "maxCombinedImages"} {
		if value, exists := fields[key]; exists && (string(value) == "null" || string(value) == `""`) {
			return result, ErrProviderContract
		}
	}
	var m ImageMetering
	d := json.NewDecoder(bytes.NewReader(p.Upstream.Metering))
	d.DisallowUnknownFields()
	if d.Decode(&m) != nil || m.Version != 1 || p.Adapter != "fal-image" || p.Upstream.Execution.Mode != "async" {
		return result, ErrProviderContract
	}
	for _, key := range []string{m.InputImagesParameter, m.OutputCountMultiplierParameter} {
		if key != "" && (!meteringParameter.MatchString(key)) {
			return result, ErrProviderContract
		}
	}
	for _, limit := range []*int64{m.MaxInputImages, m.MaxCombinedImages} {
		if limit != nil && (*limit < 1 || *limit > int64(cap)) {
			return result, ErrProviderContract
		}
	}
	if m.MaxInputImages != nil && m.InputImagesParameter == "" {
		return result, ErrProviderContract
	}
	// A known fal reference field must be the explicitly metered parameter.
	// Never interpret its presence as text-only usage or infer a new counting
	// convention: declared inputs remain nonempty arrays of strings below.
	for _, key := range []string{"image_url", "image_urls", "reference_image_url", "reference_image_urls", "input_image_url", "input_image_urls"} {
		if _, present := native[key]; present && key != m.InputImagesParameter {
			return result, ErrProviderContract
		}
	}
	body := native
	if hasBatchParameter && m.OutputCountMultiplierParameter != "max_images" {
		return result, ErrProviderContract
	}
	count, ok := body["num_images"].(float64)
	if !ok || count != float64(n) {
		return result, ErrProviderContract
	}
	if key := m.OutputCountMultiplierParameter; key != "" {
		multiplier, ok := body[key].(float64)
		if !ok || multiplier < 1 || math.Trunc(multiplier) != multiplier || multiplier > float64(cap/n) {
			return result, ErrProviderContract
		}
		result.MaximumOutputs = n * int(multiplier)
	}
	if key := m.InputImagesParameter; key != "" {
		refs, ok := body[key].([]any)
		if !ok || len(refs) == 0 {
			return result, ErrProviderContract
		}
		for _, ref := range refs {
			s, ok := ref.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return result, ErrProviderContract
			}
		}
		result.InputCount = int64(len(refs))
		if m.MaxInputImages != nil && result.InputCount > *m.MaxInputImages {
			return result, ErrProviderContract
		}
	}
	if m.MaxCombinedImages != nil && result.InputCount+int64(result.MaximumOutputs) > *m.MaxCombinedImages {
		return result, ErrProviderContract
	}
	result.Explicit = true
	return result, nil
}
