package middleware

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/labring/aiproxy/core/model"
	"regexp"
)

const customerValidationDetailKey = "customer_validation_detail"

var diagnosticScalar = regexp.MustCompile(`^[a-zA-Z0-9_. -]{1,64}$`)

// Only bounded generation controls are persisted for pre-relay rejections.
// Never retain credentials, prompt content, reference URLs, or arbitrary fields.
func saveCustomerValidationDetail(c *gin.Context, failure *registryvalidation.ValidationError) {
	summary := map[string]any{}
	if common.IsJSONContentType(c.Request.Header.Get("Content-Type")) {
		body, cached := common.GetCachedRequestBody(c.Request)
		if cached {
			var input map[string]any
			if len(body) <= 65536 && json.Unmarshal(body, &input) == nil {
				// The customer can inspect their own text prompt in the request log.
				// Keep media, URLs, and credentials out of this diagnostic summary.
				for _, key := range []string{"prompt", "negative_prompt"} {
					if value, ok := input[key].(string); ok && len(value) <= 8192 {
						summary[key] = value
					}
				}
				for _, key := range []string{"n", "num_images", "size", "image_size", "width", "height", "seed", "steps", "num_inference_steps", "guidance_scale", "quality", "output_format", "response_format", "watermark", "enable_prompt_expansion", "sequential_image_generation", "max_images", "seconds", "resolution", "aspect_ratio", "stream"} {
					switch v := input[key].(type) {
					case float64, bool:
						summary[key] = v
					case string:
						if diagnosticScalar.MatchString(v) {
							summary[key] = v
						}
					}
				}
			}
		}
	}
	requestBody, _ := json.Marshal(summary)
	responseBody, _ := json.Marshal(map[string]any{"error": failure.PublicError()})
	c.Set(customerValidationDetailKey, &model.RequestDetail{RequestBody: string(requestBody), ResponseBody: string(responseBody)})
}
