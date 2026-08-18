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
	generateAudio   *bool
}

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
			Resolution: params.size,
		},
	}, nil
}

func getVideosRequestUsageParams(c *gin.Context) (videosRequestUsageParams, error) {
	if strings.HasPrefix(c.Request.Header.Get("Content-Type"), "multipart/form-data") {
		if err := common.ParseMultipartFormWithLimit(c.Request); err != nil {
			return videosRequestUsageParams{}, NewBadRequestParamError(err.Error())
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

		return videosRequestUsageParams{
			seconds:         seconds,
			secondsProvided: secondsProvided,
			size:            c.PostForm("size"),
			generateAudio:   generateAudio,
		}, nil
	}

	node, err := common.UnmarshalRequest2NodeReusable(c.Request)
	if err != nil {
		return videosRequestUsageParams{}, NewBadRequestParamError(err.Error())
	}

	seconds, secondsProvided, err := intValueFromNode(&node, "seconds")
	if err != nil {
		return videosRequestUsageParams{}, err
	}

	generateAudio, err := optionalBoolValueFromNode(&node, "generate_audio")
	if err != nil {
		return videosRequestUsageParams{}, err
	}

	return videosRequestUsageParams{
		seconds:         seconds,
		secondsProvided: secondsProvided,
		size:            firstNonEmptyStringValueFromNode(&node, "size", "resolution"),
		generateAudio:   generateAudio,
	}, nil
}

func validateVideosRequestUsageParams(params videosRequestUsageParams, mc model.ModelConfig) error {
	fuzzy := !mc.DisableResolutionFuzzyMatch
	if err := validateOpenAIVideoSizeFormat(params.size, mc.AllowedResolutions, fuzzy); err != nil {
		return err
	}
	if supportedSizes, ok := exactVideoSizesFromCapabilities(mc.Config); ok {
		if size := strings.ToLower(strings.TrimSpace(params.size)); size != "" &&
			!slices.Contains(supportedSizes, size) {
			return NewBadRequestParamError(fmt.Sprintf(
				"unsupported video size `%s`, allowed values: %s",
				size,
				strings.Join(supportedSizes, ", "),
			))
		}
	} else if err := validateSupportedVideoResolution(
		params.size,
		mc,
		openAIVideoSupportedResolutionOptions(mc.AllowedResolutions, fuzzy),
	); err != nil {
		return err
	}

	if supportedDurations, ok := exactVideoDurationsFromCapabilities(mc.Config); ok {
		if params.secondsProvided && !slices.Contains(supportedDurations, params.seconds) {
			allowed := make([]string, len(supportedDurations))
			for i, duration := range supportedDurations {
				allowed[i] = strconv.Itoa(duration)
			}
			return NewBadRequestParamError(fmt.Sprintf(
				"unsupported video duration `%d`, allowed values: %s",
				params.seconds,
				strings.Join(allowed, ", "),
			))
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

func optionalBoolValueFromNode(node *ast.Node, name string) (*bool, error) {
	valueNode := node.Get(name)
	if valueNode == nil || !valueNode.Exists() || valueNode.TypeSafe() == ast.V_NULL {
		return nil, nil
	}
	if valueNode.TypeSafe() != ast.V_TRUE && valueNode.TypeSafe() != ast.V_FALSE {
		return nil, NewBadRequestParamError(fmt.Sprintf("invalid %s: must be a boolean", name))
	}
	value, err := valueNode.Bool()
	if err != nil {
		return nil, NewBadRequestParamError(fmt.Sprintf("invalid %s: must be a boolean", name))
	}
	return &value, nil
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
		for _, aspectRatio := range aspectRatios {
			var size string
			switch strings.TrimSpace(aspectRatio) {
			case "16:9":
				size = landscape
			case "9:16":
				size = parts[1] + "x" + parts[0]
			case "1:1":
				size = parts[1] + "x" + parts[1]
			default:
				continue
			}
			if !slices.Contains(result, size) {
				result = append(result, size)
			}
		}
	}
	return result, len(result) != 0
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
			return NewBadRequestParamError(
				"generate_audio is not supported by this model; allowed value: false",
			)
		}
	case "required":
		if !*requested {
			return NewBadRequestParamError(
				"generate_audio is required by this model; allowed value: true",
			)
		}
	}
	return nil
}
