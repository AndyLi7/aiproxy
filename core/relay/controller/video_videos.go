package controller

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/bytedance/sonic/ast"
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/model"
)

type videosRequestUsageParams struct {
	seconds         int
	secondsProvided bool
	size            string
	resolution      string
	aspectRatio     string
	generateAudio   *bool
	imageInput      bool
	initialCreate   bool
}

const (
	videoInvalidParameterCode   = "invalid_parameter"
	videoMissingParameterCode   = "missing_parameter"
	videoUnsupportedByModelCode = "unsupported_by_model"
)

func ValidateVideosRequest(c *gin.Context, mc model.ModelConfig) error {
	params, err := getVideosRequestUsageParams(c)
	if err != nil {
		return err
	}

	return validateVideosRequestUsageParams(params, mc)
}

func GetVideosRequestPrice(c *gin.Context, mc model.ModelConfig) (model.Price, error) {
	params, err := getVideosRequestUsageParams(c)
	if err != nil {
		return model.Price{}, err
	}

	if err := validateVideosRequestUsageParams(params, mc); err != nil {
		return model.Price{}, err
	}

	return getVideoRequestPrice(mc.Price), nil
}

func GetVideosRequestUsage(c *gin.Context, mc model.ModelConfig) (RequestUsage, error) {
	params, err := getVideosRequestUsageParams(c)
	if err != nil {
		return RequestUsage{}, err
	}

	if err := validateVideosRequestUsageParams(params, mc); err != nil {
		return RequestUsage{}, err
	}

	return RequestUsage{
		// Video usage is provider-specific and often async. Do not use requested
		// seconds as a preflight balance estimate; final usage is supplied by the
		// response or async usage fetcher.
		Usage: model.Usage{},
		Context: model.UsageContext{
			Resolution:       params.size,
			NativeResolution: params.resolution,
			Seconds:          params.seconds,
			OutputAudio:      params.generateAudio,
		},
	}, nil
}

func getVideosRequestUsageParams(c *gin.Context) (videosRequestUsageParams, error) {
	if strings.HasPrefix(c.Request.Header.Get("Content-Type"), "multipart/form-data") {
		if err := common.ParseMultipartFormWithLimit(c.Request); err != nil {
			return videosRequestUsageParams{}, NewBadRequestParamError(err.Error())
		}

		if c.Request.URL.Path == "/v1/videos" && strings.TrimSpace(c.PostForm("prompt")) == "" {
			return videosRequestUsageParams{}, NewDetailedBadRequestParamError(
				videoMissingParameterCode,
				"prompt is required and must be a non-empty string",
				"prompt", nil, nil, "non-empty string",
			)
		}

		secondsValue, secondsProvided := c.GetPostForm("seconds")
		seconds, err := parseOptionalPositiveInt(secondsValue, "seconds")
		if err != nil {
			return videosRequestUsageParams{}, err
		}

		generateAudio, err := parseOptionalBool(c, "generate_audio")
		if err != nil {
			return videosRequestUsageParams{}, err
		}
		size := strings.TrimSpace(c.PostForm("size"))
		resolution := strings.TrimSpace(c.PostForm("resolution"))
		aspectRatio := strings.TrimSpace(c.PostForm("aspect_ratio"))
		if c.Request.URL.Path == "/v1/videos" && size == "" && resolution == "" && aspectRatio == "" {
			return videosRequestUsageParams{}, NewDetailedBadRequestParamError(
				videoMissingParameterCode,
				"provide either size or resolution with aspect_ratio",
				"size", nil, nil, "<width>x<height> string",
			)
		}

		return videosRequestUsageParams{
			seconds:         seconds,
			secondsProvided: secondsProvided,
			size:            size,
			resolution:      resolution,
			aspectRatio:     aspectRatio,
			generateAudio:   generateAudio,
			imageInput:      multipartVideoImageInputProvided(c),
			initialCreate:   c.Request.URL.Path == "/v1/videos",
		}, nil
	}

	node, err := common.UnmarshalRequest2NodeReusable(c.Request)
	if err != nil {
		return videosRequestUsageParams{}, NewDetailedBadRequestParamError(
			videoInvalidParameterCode,
			"request body must contain valid JSON",
			"body", nil, nil, "valid JSON object",
		)
	}
	if node.TypeSafe() != ast.V_OBJECT {
		return videosRequestUsageParams{}, NewDetailedBadRequestParamError(
			videoInvalidParameterCode,
			"request body must be a JSON object",
			"body", videoJSONTypeName(node.TypeSafe()), nil, "JSON object",
		)
	}
	if c.Request.URL.Path == "/v1/videos" {
		promptNode := node.Get("prompt")
		if promptNode == nil || !promptNode.Exists() || promptNode.TypeSafe() == ast.V_NULL {
			return videosRequestUsageParams{}, NewDetailedBadRequestParamError(
				videoMissingParameterCode,
				"prompt is required and must be a non-empty string",
				"prompt", nil, nil, "non-empty string",
			)
		}
		if promptNode.TypeSafe() != ast.V_STRING {
			return videosRequestUsageParams{}, NewDetailedBadRequestParamError(
				videoInvalidParameterCode,
				"prompt must be a non-empty string",
				"prompt", videoNodeValue(promptNode), nil, "non-empty string",
			)
		}
		prompt, promptErr := promptNode.String()
		if promptErr != nil || strings.TrimSpace(prompt) == "" {
			return videosRequestUsageParams{}, NewDetailedBadRequestParamError(
				videoMissingParameterCode,
				"prompt is required and must be a non-empty string",
				"prompt", prompt, nil, "non-empty string",
			)
		}
	}

	seconds, secondsProvided, err := strictOptionalPositiveIntValueFromNode(&node, "seconds")
	if err != nil {
		return videosRequestUsageParams{}, err
	}

	generateAudio, err := strictOptionalBoolValueFromNode(&node, "generate_audio")
	if err != nil {
		return videosRequestUsageParams{}, err
	}

	size, err := optionalStringValueFromNode(&node, "size")
	if err != nil {
		return videosRequestUsageParams{}, err
	}
	resolution, err := optionalEnumStringValueFromNode(&node, "resolution", "resolution string")
	if err != nil {
		return videosRequestUsageParams{}, err
	}
	aspectRatio, err := optionalEnumStringValueFromNode(&node, "aspect_ratio", "aspect ratio string")
	if err != nil {
		return videosRequestUsageParams{}, err
	}
	if c.Request.URL.Path == "/v1/videos" && strings.TrimSpace(size) == "" &&
		strings.TrimSpace(resolution) == "" && strings.TrimSpace(aspectRatio) == "" {
		return videosRequestUsageParams{}, NewDetailedBadRequestParamError(
			videoMissingParameterCode,
			"provide either size or resolution with aspect_ratio",
			"size", nil, nil, "<width>x<height> string",
		)
	}

	return videosRequestUsageParams{
		seconds:         seconds,
		secondsProvided: secondsProvided,
		size:            size,
		resolution:      resolution,
		aspectRatio:     aspectRatio,
		generateAudio:   generateAudio,
		imageInput:      videoImageInputProvided(&node),
		initialCreate:   c.Request.URL.Path == "/v1/videos",
	}, nil
}

func validateVideosRequestUsageParams(params videosRequestUsageParams, mc model.ModelConfig) error {
	if err := validateVideoCapabilityInput(params, mc.Config); err != nil {
		return err
	}
	if err := validateVideoDimensionSelection(params, mc); err != nil {
		return err
	}

	if strings.TrimSpace(params.resolution) != "" {
		return validateVideosRequestDurationAndAudio(params, mc)
	}

	fuzzy := !mc.DisableResolutionFuzzyMatch
	supportedSizes, hasExactSizes := exactVideoSizesFromCapabilities(mc.Config)
	size := strings.ToLower(strings.TrimSpace(params.size))
	if size != "" && !dimensionResolutionValue(size) {
		allowedValues := supportedSizes
		if !hasExactSizes {
			options := openAIVideoSupportedResolutionOptions(mc.AllowedResolutions, fuzzy)
			if options != "<width>x<height>" {
				allowedValues = strings.Split(options, ", ")
			}
		}
		message := fmt.Sprintf("invalid video size `%s`: expected <width>x<height>", size)
		if len(allowedValues) != 0 {
			message = fmt.Sprintf(
				"invalid video size `%s`, allowed values: %s",
				size,
				strings.Join(allowedValues, ", "),
			)
		}
		return NewDetailedBadRequestParamError(
			videoInvalidParameterCode,
			message,
			"size", size, allowedValues, "<width>x<height> string",
		)
	}
	if hasExactSizes {
		if len(supportedSizes) == 0 && size != "" {
			return NewDetailedBadRequestParamError(
				videoUnsupportedByModelCode,
				"fixed video sizes are not supported by this model; use resolution with aspect_ratio",
				"size", size, nil, "resolution with aspect_ratio",
			)
		}
		if size := strings.ToLower(strings.TrimSpace(params.size)); size != "" &&
			!slices.Contains(supportedSizes, size) {
			return NewDetailedBadRequestParamError(videoUnsupportedByModelCode, fmt.Sprintf(
				"unsupported video size `%s`, allowed values: %s",
				size,
				strings.Join(supportedSizes, ", "),
			), "size", size, supportedSizes, "one of the allowed size values")
		}
	} else if err := validateSupportedVideoResolution(
		params.size,
		mc,
		openAIVideoSupportedResolutionOptions(mc.AllowedResolutions, fuzzy),
	); err != nil {
		return err
	}

	return validateVideosRequestDurationAndAudio(params, mc)
}

func validateVideoCapabilityInput(
	params videosRequestUsageParams,
	config map[model.ModelConfigKey]any,
) error {
	if !params.initialCreate {
		return nil
	}

	capability, _ := config[model.ModelConfigKey("capability")].(string)
	switch capability {
	case string(model.ModelCapabilityImageToVideo):
		if !params.imageInput {
			return NewDetailedBadRequestParamError(
				videoMissingParameterCode,
				"input_reference is required for image-to-video",
				"input_reference", nil, nil, "image URL or image file",
			)
		}
	case string(model.ModelCapabilityTextToVideo):
		if params.imageInput {
			return NewDetailedBadRequestParamError(
				videoUnsupportedByModelCode,
				"input_reference is not supported for text-to-video",
				"input_reference", nil, nil, "omit image input",
			)
		}
	}

	return nil
}

func multipartVideoImageInputProvided(c *gin.Context) bool {
	for _, key := range []string{"input_reference", "image", "image_url", "first_frame_url"} {
		if strings.TrimSpace(c.PostForm(key)) != "" {
			return true
		}
		if c.Request.MultipartForm != nil && len(c.Request.MultipartForm.File[key]) != 0 {
			return true
		}
	}
	return false
}

func videoImageInputProvided(node *ast.Node) bool {
	for _, key := range []string{"input_reference", "image", "image_url", "first_frame_url"} {
		field := node.Get(key)
		if field == nil || !field.Exists() || field.TypeSafe() == ast.V_NULL {
			continue
		}
		if field.TypeSafe() != ast.V_STRING {
			return true
		}
		value, err := field.String()
		if err != nil || strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func validateVideoDimensionSelection(params videosRequestUsageParams, mc model.ModelConfig) error {
	size := strings.TrimSpace(params.size)
	resolution := strings.ToLower(strings.TrimSpace(params.resolution))
	aspectRatio := strings.TrimSpace(params.aspectRatio)

	if size != "" && (resolution != "" || aspectRatio != "") {
		return NewDetailedBadRequestParamError(
			videoInvalidParameterCode,
			"size cannot be combined with resolution or aspect_ratio",
			"size", size, nil, "either size or resolution with aspect_ratio",
		)
	}
	if resolution == "" && aspectRatio != "" {
		return NewDetailedBadRequestParamError(
			videoMissingParameterCode,
			"resolution is required when aspect_ratio is provided",
			"resolution", nil, nil, "resolution string",
		)
	}
	if resolution != "" && aspectRatio == "" {
		return NewDetailedBadRequestParamError(
			videoMissingParameterCode,
			"aspect_ratio is required when resolution is provided",
			"aspect_ratio", nil, nil, "aspect ratio string",
		)
	}
	if resolution == "" {
		return nil
	}

	allowedResolutions, resolutionsOK := model.GetModelConfigStringSlice(
		mc.Config,
		model.ModelConfigKey("resolutions"),
	)
	if !resolutionsOK || len(allowedResolutions) == 0 {
		allowedResolutions = mc.AllowedResolutions
	}
	allowedResolutions = normalizedVideoCapabilityValues(allowedResolutions, true)
	if len(allowedResolutions) != 0 && !slices.Contains(allowedResolutions, resolution) {
		return NewDetailedBadRequestParamError(videoUnsupportedByModelCode, fmt.Sprintf(
			"unsupported video resolution `%s`, allowed values: %s",
			resolution,
			strings.Join(allowedResolutions, ", "),
		), "resolution", resolution, allowedResolutions, "one of the allowed resolution values")
	}

	allowedAspectRatios, ratiosOK := model.GetModelConfigStringSlice(
		mc.Config,
		model.ModelConfigKey("aspectRatios"),
	)
	allowedAspectRatios = normalizedVideoCapabilityValues(allowedAspectRatios, false)
	if ratiosOK && len(allowedAspectRatios) != 0 && !slices.Contains(allowedAspectRatios, aspectRatio) {
		return NewDetailedBadRequestParamError(videoUnsupportedByModelCode, fmt.Sprintf(
			"unsupported video aspect_ratio `%s`, allowed values: %s",
			aspectRatio,
			strings.Join(allowedAspectRatios, ", "),
		), "aspect_ratio", aspectRatio, allowedAspectRatios, "one of the allowed aspect ratio values")
	}

	return nil
}

func normalizedVideoCapabilityValues(values []string, lower bool) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if lower {
			value = strings.ToLower(value)
		}
		if value != "" && !slices.Contains(result, value) {
			result = append(result, value)
		}
	}
	return result
}

func validateVideosRequestDurationAndAudio(
	params videosRequestUsageParams,
	mc model.ModelConfig,
) error {
	if supportedDurations, ok := exactVideoDurationsFromCapabilities(mc.Config); ok {
		if params.secondsProvided && !slices.Contains(supportedDurations, params.seconds) {
			allowed := make([]string, len(supportedDurations))
			for i, duration := range supportedDurations {
				allowed[i] = strconv.Itoa(duration)
			}
			return NewDetailedBadRequestParamError(videoUnsupportedByModelCode, fmt.Sprintf(
				"unsupported video duration `%d`, allowed values: %s",
				params.seconds,
				strings.Join(allowed, ", "),
			), "seconds", strconv.Itoa(params.seconds), allowed, "one of the allowed integer durations")
		}
	} else if err := validateVideoGenerationSeconds(
		params.seconds,
		mc.MaxVideoGenerationSeconds,
	); err != nil {
		return err
	}

	return validateGenerateAudioCapability(params.generateAudio, mc.Config)
}

func parseOptionalBool(c *gin.Context, name string) (*bool, error) {
	value, ok := c.GetPostForm(name)
	if !ok || strings.TrimSpace(value) == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return nil, NewBadRequestParamError(fmt.Sprintf("invalid %s: must be a boolean", name))
	}
	return &parsed, nil
}

func strictOptionalBoolValueFromNode(node *ast.Node, name string) (*bool, error) {
	valueNode := node.Get(name)
	if valueNode == nil || !valueNode.Exists() {
		return nil, nil
	}
	if valueNode.TypeSafe() == ast.V_NULL {
		return nil, NewDetailedBadRequestParamError(
			videoInvalidParameterCode,
			fmt.Sprintf("%s must be a boolean", name),
			name, nil, []string{"true", "false"}, "boolean",
		)
	}
	if valueNode.TypeSafe() != ast.V_TRUE && valueNode.TypeSafe() != ast.V_FALSE {
		return nil, NewDetailedBadRequestParamError(
			videoInvalidParameterCode,
			fmt.Sprintf("%s must be a boolean", name),
			name, videoNodeValue(valueNode), []string{"true", "false"}, "boolean",
		)
	}
	value, err := valueNode.Bool()
	if err != nil {
		return nil, NewBadRequestParamError(fmt.Sprintf("invalid %s: must be a boolean", name))
	}
	return &value, nil
}

func strictOptionalPositiveIntValueFromNode(node *ast.Node, name string) (int, bool, error) {
	valueNode := node.Get(name)
	if valueNode == nil || !valueNode.Exists() {
		return 0, false, nil
	}
	if valueNode.TypeSafe() == ast.V_NULL {
		return 0, true, NewDetailedBadRequestParamError(
			videoInvalidParameterCode,
			fmt.Sprintf("%s must be a positive integer", name),
			name, nil, nil, "positive integer",
		)
	}
	if valueNode.TypeSafe() != ast.V_NUMBER {
		return 0, true, NewDetailedBadRequestParamError(
			videoInvalidParameterCode,
			fmt.Sprintf("%s must be a positive integer", name),
			name, videoNodeValue(valueNode), nil, "positive integer",
		)
	}
	raw, err := valueNode.Raw()
	if err != nil {
		return 0, true, NewDetailedBadRequestParamError(
			videoInvalidParameterCode,
			fmt.Sprintf("%s must be a positive integer", name),
			name, videoNodeValue(valueNode), nil, "positive integer",
		)
	}
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 0)
	if err != nil || value <= 0 {
		return 0, true, NewDetailedBadRequestParamError(
			videoInvalidParameterCode,
			fmt.Sprintf("%s must be a positive integer", name),
			name, videoNodeValue(valueNode), nil, "positive integer",
		)
	}
	return int(value), true, nil
}

func optionalStringValueFromNode(node *ast.Node, names ...string) (string, error) {
	for _, name := range names {
		valueNode := node.Get(name)
		if valueNode == nil || !valueNode.Exists() {
			continue
		}
		if valueNode.TypeSafe() != ast.V_STRING {
			return "", NewDetailedBadRequestParamError(
				videoInvalidParameterCode,
				fmt.Sprintf("%s must be a string in <width>x<height> format", name),
				name, videoNodeValue(valueNode), nil, "<width>x<height> string",
			)
		}
		value, err := valueNode.String()
		if err != nil {
			return "", NewDetailedBadRequestParamError(
				videoInvalidParameterCode,
				fmt.Sprintf("%s must be a string in <width>x<height> format", name),
				name, nil, nil, "<width>x<height> string",
			)
		}
		if strings.TrimSpace(value) != "" {
			return value, nil
		}
	}
	return "", nil
}

func optionalEnumStringValueFromNode(node *ast.Node, name, expected string) (string, error) {
	valueNode := node.Get(name)
	if valueNode == nil || !valueNode.Exists() {
		return "", nil
	}
	if valueNode.TypeSafe() != ast.V_STRING {
		return "", NewDetailedBadRequestParamError(
			videoInvalidParameterCode,
			fmt.Sprintf("%s must be a string", name),
			name, videoNodeValue(valueNode), nil, expected,
		)
	}
	value, err := valueNode.String()
	if err != nil {
		return "", NewDetailedBadRequestParamError(
			videoInvalidParameterCode,
			fmt.Sprintf("%s must be a string", name),
			name, nil, nil, expected,
		)
	}
	return strings.TrimSpace(value), nil
}

func videoNodeValue(node *ast.Node) any {
	if node == nil || !node.Exists() || node.TypeSafe() == ast.V_NULL {
		return nil
	}
	if raw, err := node.Raw(); err == nil {
		return raw
	}
	return videoJSONTypeName(node.TypeSafe())
}

func videoJSONTypeName(valueType int) string {
	switch valueType {
	case ast.V_OBJECT:
		return "object"
	case ast.V_ARRAY:
		return "array"
	case ast.V_STRING:
		return "string"
	case ast.V_NUMBER:
		return "number"
	case ast.V_TRUE, ast.V_FALSE:
		return "boolean"
	case ast.V_NULL:
		return "null"
	default:
		return "unknown"
	}
}

var videoPixelsByResolution = map[string]string{
	"480p":  "854x480",
	"720p":  "1280x720",
	"1080p": "1920x1080",
	"4k":    "3840x2160",
}

func exactVideoSizesFromCapabilities(config map[model.ModelConfigKey]any) ([]string, bool) {
	resolutions, resolutionsOK := model.GetModelConfigStringSlice(
		config,
		model.ModelConfigKey("resolutions"),
	)
	aspectRatios, aspectRatiosOK := model.GetModelConfigStringSlice(
		config,
		model.ModelConfigKey("aspectRatios"),
	)
	if !resolutionsOK || !aspectRatiosOK || len(resolutions) == 0 || len(aspectRatios) == 0 {
		return nil, false
	}

	result := make([]string, 0, len(resolutions)*len(aspectRatios))
	for _, resolution := range resolutions {
		landscape, ok := videoPixelsByResolution[strings.ToLower(strings.TrimSpace(resolution))]
		if !ok {
			continue
		}
		parts := strings.Split(landscape, "x")
		if len(parts) != 2 {
			continue
		}
		height, err := strconv.Atoi(parts[1])
		if err != nil || height <= 0 {
			continue
		}
		for _, aspectRatio := range aspectRatios {
			var size string
			switch strings.TrimSpace(aspectRatio) {
			case "21:9":
				size = fmt.Sprintf("%dx%d", height*21/9, height)
			case "16:9":
				size = landscape
			case "4:3":
				size = fmt.Sprintf("%dx%d", height*4/3, height)
			case "9:16":
				size = parts[1] + "x" + parts[0]
			case "1:1":
				size = parts[1] + "x" + parts[1]
			case "3:4":
				size = fmt.Sprintf("%dx%d", height, height*4/3)
			default:
				continue
			}
			if !slices.Contains(result, size) {
				result = append(result, size)
			}
		}
	}
	return result, true
}

func exactVideoDurationsFromCapabilities(config map[model.ModelConfigKey]any) ([]int, bool) {
	value, ok := config[model.ModelConfigKey("durations")]
	if !ok {
		return nil, false
	}
	values, ok := value.([]any)
	if !ok {
		if durations, ok := value.([]int); ok && len(durations) != 0 {
			return durations, true
		}
		return nil, false
	}
	result := make([]int, 0, len(values))
	for _, raw := range values {
		var duration int
		switch typed := raw.(type) {
		case int:
			duration = typed
		case int64:
			duration = int(typed)
		case float64:
			duration = int(typed)
			if typed != float64(duration) {
				return nil, false
			}
		default:
			return nil, false
		}
		if duration <= 0 {
			return nil, false
		}
		result = append(result, duration)
	}
	return result, len(result) != 0
}

func validateGenerateAudioCapability(
	requested *bool,
	config map[model.ModelConfigKey]any,
) error {
	if requested == nil {
		return nil
	}
	audio, ok := config[model.ModelConfigKey("audio")].(map[string]any)
	if !ok {
		return nil
	}
	mode, _ := audio["mode"].(string)
	switch mode {
	case "none":
		if *requested {
			return NewDetailedBadRequestParamError(
				videoUnsupportedByModelCode,
				"generate_audio is not supported by this model; allowed value: false",
				"generate_audio", true, []string{"false"}, "false",
			)
		}
	case "required":
		if !*requested {
			return NewDetailedBadRequestParamError(
				videoUnsupportedByModelCode,
				"generate_audio is required by this model; allowed value: true",
				"generate_audio", false, []string{"true"}, "true",
			)
		}
	}
	return nil
}
