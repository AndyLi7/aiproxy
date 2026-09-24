package registryvalidation

import (
	"bytes"
	"encoding/json"
	"github.com/labring/aiproxy/core/common/imagecapabilities"
	"math"
	"regexp"
	"strings"
)

// ImageMetering is server-owned metadata using flat native request parameter names.
type RoleImageInput struct {
	Parameter string `json:"parameter"`
	Role      string `json:"role"`
	Shape     string `json:"shape"`
	Required  *bool  `json:"required"`
	Maximum   int    `json:"maximum"`
}
type ImageMetering struct {
	InputImages                    []RoleImageInput `json:"inputImages,omitempty"`
	MaxOutputPixels                *int64           `json:"maxOutputPixels,omitempty"`
	OutputCountFixed               *int64           `json:"outputCountFixed,omitempty"`
	InputImagesOptional            *bool            `json:"inputImagesOptional,omitempty"`
	Version                        int              `json:"version"`
	InputImagesShape               string           `json:"inputImagesShape,omitempty"`
	OutputCountParameter           string           `json:"outputCountParameter,omitempty"`
	InputImagesParameter           string           `json:"inputImagesParameter,omitempty"`
	MaxInputImages                 *int64           `json:"maxInputImages,omitempty"`
	OutputCountMultiplierParameter string           `json:"outputCountMultiplierParameter,omitempty"`
	MaxCombinedImages              *int64           `json:"maxCombinedImages,omitempty"`
}

var meteringParameter = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type ImageMeteringEvidence struct {
	MaxOutputPixels int64
	InputCount      int64
	MaximumOutputs  int
	Explicit        bool
}

//nolint:gocyclo // This is one auditable validation pass for the provider metering contract.
func ResolveImageMetering(
	raw []byte,
	binding ProviderBinding,
	mapped []byte,
	n, maxOutputs int,
	required bool,
) (ImageMeteringEvidence, error) {
	result := ImageMeteringEvidence{MaximumOutputs: n}

	var native map[string]any
	if json.Unmarshal(mapped, &native) != nil || native == nil {
		return result, ErrProviderContract
	}

	batchParameter, hasBatchParameter := native["max_images"]
	hasBatchParameter = hasBatchParameter && batchParameter != float64(1)

	if n < 1 || n > maxOutputs {
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

	for _, key := range []string{"inputImages", "maxOutputPixels", "outputCountFixed", "inputImagesOptional", "inputImagesParameter", "outputCountMultiplierParameter", "maxInputImages", "maxCombinedImages", "inputImagesShape", "outputCountParameter"} {
		if value, exists := fields[key]; exists &&
			(string(value) == "null" || string(value) == `""`) {
			return result, ErrProviderContract
		}
	}

	var m ImageMetering

	d := json.NewDecoder(bytes.NewReader(p.Upstream.Metering))
	d.DisallowUnknownFields()

	if d.Decode(&m) != nil || !imagecapabilities.SupportsMetering(p.Adapter, p.Upstream.Execution.Mode, m.Version) {
		return result, ErrProviderContract
	}

	if m.Version == 4 || (m.Version == 5 && m.MaxOutputPixels != nil) {
		if m.MaxOutputPixels == nil || *m.MaxOutputPixels < 1 || *m.MaxOutputPixels > 100000000 {
			return result, ErrProviderContract
		}
		result.MaxOutputPixels = *m.MaxOutputPixels
	} else if m.MaxOutputPixels != nil {
		return result, ErrProviderContract
	}
	for _, key := range []string{m.InputImagesParameter, m.OutputCountMultiplierParameter, m.OutputCountParameter} {
		if key != "" && (!meteringParameter.MatchString(key)) {
			return result, ErrProviderContract
		}
	}

	if m.Version == 1 && (m.InputImagesShape != "" || m.OutputCountParameter != "") {
		return result, ErrProviderContract
	}
	if m.Version >= 2 {
		if (m.OutputCountParameter == "" && m.OutputCountFixed == nil) ||
			(m.InputImagesParameter == "" && m.InputImagesShape != "") ||
			(m.InputImagesParameter != "" && m.InputImagesShape != "single" && m.InputImagesShape != "array") ||
			(m.InputImagesShape == "single" && (m.MaxInputImages == nil || *m.MaxInputImages != 1)) {
			return result, ErrProviderContract
		}
	}

	if m.OutputCountFixed != nil {
		if (m.Version != 3 && m.Version != 4 && m.Version != 5) || *m.OutputCountFixed != 1 || n != 1 || m.OutputCountParameter != "" || m.OutputCountMultiplierParameter != "" {
			return result, ErrProviderContract
		}
		for _, key := range []string{"num_images", "max_images"} {
			if _, present := native[key]; present {
				return result, ErrProviderContract
			}
		}
	}
	if m.InputImagesOptional != nil && ((m.Version != 3 && m.Version != 4 && m.Version != 5) || !*m.InputImagesOptional || m.InputImagesParameter == "") {
		return result, ErrProviderContract
	}

	for _, limit := range []*int64{m.MaxInputImages, m.MaxCombinedImages} {
		if limit != nil && (*limit < 1 || *limit > int64(maxOutputs)) {
			return result, ErrProviderContract
		}
	}

	if m.MaxInputImages != nil && m.InputImagesParameter == "" {
		return result, ErrProviderContract
	}

	declared := map[string]bool{}
	if m.Version == 5 {
		if len(m.InputImages) < 1 || len(m.InputImages) > 4 || m.InputImagesParameter != "" || m.InputImagesShape != "" || m.InputImagesOptional != nil || m.MaxInputImages != nil || m.MaxCombinedImages != nil {
			return result, ErrProviderContract
		}
		roles := map[string]string{"image_url": "source", "mask_url": "mask", "model_image": "person", "garment_image": "garment"}
		for _, input := range m.InputImages {
			role, known := roles[input.Parameter]
			if !known || role != input.Role || declared[input.Parameter] || input.Shape != "single" || input.Maximum != 1 || input.Required == nil {
				return result, ErrProviderContract
			}
			declared[input.Parameter] = true
			value, present := native[input.Parameter]
			if !present && !*input.Required {
				continue
			}
			text, ok := value.(string)
			if !ok || strings.TrimSpace(text) == "" {
				return result, ErrProviderContract
			}
			result.InputCount++
		}
	} else if _, present := fields["inputImages"]; present {
		return result, ErrProviderContract
	}
	if samples, present := native["num_samples"]; present && (m.OutputCountFixed == nil || samples != float64(1)) {
		return result, ErrProviderContract
	}
	// A known fal reference field must be the explicitly metered parameter.
	// Never interpret its presence as text-only usage or infer a new counting
	// convention: declared inputs remain nonempty arrays of strings below.
	for _, key := range []string{"image_url", "image_urls", "reference_image_url", "reference_image_urls", "input_image_url", "input_image_urls", "mask_url", "model_image", "garment_image"} {
		if _, present := native[key]; present && key != m.InputImagesParameter && !declared[key] {
			return result, ErrProviderContract
		}
	}

	body := native

	if hasBatchParameter && m.OutputCountMultiplierParameter != "max_images" {
		return result, ErrProviderContract
	}

	countParameter := "num_images"
	if m.Version >= 2 {
		countParameter = m.OutputCountParameter
	}
	count, ok := body[countParameter].(float64)
	if m.OutputCountFixed == nil && (!ok || count != float64(n)) {
		return result, ErrProviderContract
	}

	if key := m.OutputCountMultiplierParameter; key != "" {
		multiplier, ok := body[key].(float64)
		if !ok || multiplier < 1 || math.Trunc(multiplier) != multiplier ||
			multiplier > float64(maxOutputs/n) {
			return result, ErrProviderContract
		}

		result.MaximumOutputs = n * int(multiplier)
	}

	if key := m.InputImagesParameter; key != "" {
		_, present := body[key]
		if !present && m.InputImagesOptional != nil && *m.InputImagesOptional {
			result.InputCount = 0
		} else if m.Version >= 2 && m.InputImagesShape == "single" {
			ref, ok := body[key].(string)
			if !ok || strings.TrimSpace(ref) == "" {
				return result, ErrProviderContract
			}
			result.InputCount = 1
		} else {
			refs, ok := body[key].([]any)
			if !ok || len(refs) == 0 {
				return result, ErrProviderContract
			}
			for _, ref := range refs {
				value, ok := ref.(string)
				if !ok || strings.TrimSpace(value) == "" {
					return result, ErrProviderContract
				}
			}
			result.InputCount = int64(len(refs))
		}
		if m.MaxInputImages != nil && result.InputCount > *m.MaxInputImages {
			return result, ErrProviderContract
		}
	}

	if m.MaxCombinedImages != nil &&
		result.InputCount+int64(result.MaximumOutputs) > *m.MaxCombinedImages {
		return result, ErrProviderContract
	}

	result.Explicit = true

	return result, nil
}
