package doubao

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/bytedance/sonic/ast"
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/common/image"
	coremodel "github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/labring/aiproxy/core/relay/meta"
	relaymodel "github.com/labring/aiproxy/core/relay/model"
	"github.com/labring/aiproxy/core/relay/render"
	relayutils "github.com/labring/aiproxy/core/relay/utils"
)

const metaDoubaoImageScenario = "doubao_image_scenario"

const metaDoubaoImageResponseFormat = "doubao_image_response_format"

const doubaoImageStreamEventPartialSucceeded = "image_generation.partial_succeeded"

type doubaoImageResponse struct {
	Created int64                   `json:"created,omitempty"`
	Data    []*doubaoImageData      `json:"data,omitempty"`
	Usage   *doubaoImageUsage       `json:"usage,omitempty"`
	Error   *relaymodel.OpenAIError `json:"error,omitempty"`
}

type doubaoImageData struct {
	ZIndex        *int64                  `json:"z_index,omitempty"`
	URL           string                  `json:"url,omitempty"`
	B64JSON       string                  `json:"b64_json,omitempty"`
	RevisedPrompt string                  `json:"revised_prompt,omitempty"`
	Size          string                  `json:"size,omitempty"`
	Error         *relaymodel.OpenAIError `json:"error,omitempty"`
}

type doubaoImageUsage struct {
	InputImages     *int64 `json:"input_images,omitempty"`
	generatedCount  *int64
	GeneratedImages int64                `json:"generated_images,omitempty"`
	OutputTokens    int64                `json:"output_tokens,omitempty"`
	TotalTokens     int64                `json:"total_tokens,omitempty"`
	ToolUsage       doubaoImageToolUsage `json:"tool_usage,omitempty"`
}

type doubaoImageToolUsage struct {
	WebSearch int64 `json:"web_search,omitempty"`
}

type doubaoImageStreamEvent struct {
	Type       string                  `json:"type,omitempty"`
	Model      string                  `json:"model,omitempty"`
	Created    int64                   `json:"created,omitempty"`
	ImageIndex int                     `json:"image_index,omitempty"`
	URL        string                  `json:"url,omitempty"`
	B64JSON    string                  `json:"b64_json,omitempty"`
	Size       string                  `json:"size,omitempty"`
	Usage      *doubaoImageUsage       `json:"usage,omitempty"`
	Error      *relaymodel.OpenAIError `json:"error,omitempty"`
}

func ConvertImageRequest(meta *meta.Meta, req *http.Request) (adaptor.ConvertResult, error) {
	node, err := common.UnmarshalRequest2NodeReusable(req)
	if err != nil {
		return adaptor.ConvertResult{}, err
	}

	responseFormat, err := node.Get("response_format").String()
	if err != nil && !errors.Is(err, ast.ErrNotExist) {
		return adaptor.ConvertResult{}, err
	}

	meta.Set(metaDoubaoImageResponseFormat, responseFormat)

	scenario := "generation"

	layer := node.Get("layer_decomposition")
	if layer != nil && layer.Exists() {
		enabled, err := layer.Bool()
		if err != nil {
			return adaptor.ConvertResult{}, errors.New("layer_decomposition must be a boolean")
		}

		if enabled {
			scenario = "layer_decomposition"
		}
	}

	meta.Set(metaDoubaoImageScenario, scenario)

	stream := node.Get("stream")
	if stream != nil && stream.Exists() {
		enabled, err := stream.Bool()
		if err != nil {
			return adaptor.ConvertResult{}, err
		}

		if enabled &&
			(scenario == "layer_decomposition" || strings.Contains(strings.ToLower(meta.ActualModel), "5-0-pro") || strings.Contains(strings.ToLower(meta.ActualModel), "5.0-pro") || meta.ModelConfig.Price.HasImageBilling()) {
			return adaptor.ConvertResult{}, errors.New(
				"measured image billing and Seedream Pro do not support streaming",
			)
		}
	}

	if err := normalizeDoubaoImageRequest(&node, meta.ActualModel); err != nil {
		return adaptor.ConvertResult{}, err
	}

	data, err := node.MarshalJSON()
	if err != nil {
		return adaptor.ConvertResult{}, err
	}

	return adaptor.ConvertResult{
		Header: http.Header{
			"Content-Type":   {"application/json"},
			"Content-Length": {strconv.Itoa(len(data))},
		},
		Body: bytes.NewReader(data),
	}, nil
}

func normalizeDoubaoImageRequest(node *ast.Node, actualModel string) error {
	if _, err := node.Set("model", ast.NewString(actualModel)); err != nil {
		return err
	}

	if size, err := node.Get("size").String(); err == nil {
		if _, err := node.Set(
			"size",
			ast.NewString(normalizeDoubaoImageRequestSize(size)),
		); err != nil {
			return err
		}
	} else if !errors.Is(err, ast.ErrNotExist) {
		return err
	}

	if n, err := node.Get("n").Int64(); err == nil && n > 1 {
		if err := setDoubaoSequentialImages(node, n); err != nil {
			return err
		}
	}

	_, err := node.Unset("n")
	if err != nil && !errors.Is(err, ast.ErrNotExist) {
		return err
	}

	return nil
}

func setDoubaoSequentialImages(node *ast.Node, n int64) error {
	if n > 15 {
		n = 15
	}

	if _, err := node.Set("sequential_image_generation", ast.NewString("auto")); err != nil {
		return err
	}

	options := node.Get("sequential_image_generation_options")
	if options == nil || !options.Exists() || options.TypeSafe() == ast.V_NULL {
		optionsNode := ast.NewObject(nil)
		if _, err := node.Set("sequential_image_generation_options", optionsNode); err != nil {
			return err
		}

		options = node.Get("sequential_image_generation_options")
	}

	_, err := options.Set("max_images", ast.NewNumber(strconv.FormatInt(n, 10)))

	return err
}

func ImageHandler(
	meta *meta.Meta,
	c *gin.Context,
	resp *http.Response,
) (adaptor.DoResponseResult, adaptor.Error) {
	if resp.StatusCode != http.StatusOK {
		return adaptor.DoResponseResult{}, ErrorHandler(resp)
	}

	defer resp.Body.Close()

	log := common.GetLogger(c)

	var response doubaoImageResponse
	if err := common.UnmarshalResponse(resp, &response); err != nil {
		scenario := meta.GetString(metaDoubaoImageScenario)
		if scenario == "" {
			scenario = "generation"
		}

		return adaptor.DoResponseResult{
				UsageContext: coremodel.UsageContext{
					ImageUsage: &coremodel.ImageUsage{
						Version:  1,
						State:    "incomplete",
						Scenario: scenario,
						Outputs:  []coremodel.ImageUsageOutput{},
					},
				},
			},
			relaymodel.WrapperOpenAIError(
				err,
				"unmarshal_response_body_failed",
				http.StatusInternalServerError,
			)
	}

	if response.Error != nil && response.Error.Message != "" {
		return adaptor.DoResponseResult{}, relaymodel.NewOpenAIError(
			http.StatusBadGateway,
			*response.Error,
		)
	}

	usage := doubaoImageUsageToModelUsage(response.Usage, response.Data, 0)
	usageContext := doubaoImageUsageContext(response.Data).WithFallback(meta.RequestUsageContext)

	scenario := meta.GetString(metaDoubaoImageScenario)
	if scenario == "" {
		scenario = "generation"
	}

	measuredUsage := measureDoubaoImages(response, scenario)
	if meta.ModelConfig.Price.HasImageBilling() {
		usageContext.ImageUsage = measuredUsage
	}

	if measuredUsage.State == "failed" && scenario == "layer_decomposition" {
		return adaptor.DoResponseResult{
				UsageContext: usageContext,
			},
			relaymodel.WrapperOpenAIError(
				errors.New("layer decomposition failed"),
				"layer_decomposition_failed",
				http.StatusBadGateway,
			)
	}

	if meta.GetString(metaDoubaoImageResponseFormat) == "b64_json" {
		for _, data := range response.Data {
			if data == nil || data.B64JSON != "" || data.URL == "" {
				continue
			}

			var b64JSON string

			_, b64JSON, err := image.GetImageFromURL(c.Request.Context(), data.URL)
			if err != nil {
				log.Warnf("convert doubao image url to b64_json failed, keep original url: %v", err)
				continue
			}

			data.B64JSON = b64JSON
			data.URL = ""
		}
	}

	openAIResponse := doubaoImageResponseToOpenAI(
		response,
		usage,
		meta.ModelConfig.Price.HasImageBilling() || scenario == "layer_decomposition",
	)

	data, err := sonic.Marshal(&openAIResponse)
	if err != nil {
		return adaptor.DoResponseResult{Usage: usage, UsageContext: usageContext},
			relaymodel.WrapperOpenAIError(
				err,
				"marshal_response_body_failed",
				http.StatusInternalServerError,
			)
	}

	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.Header().Set("Content-Length", strconv.Itoa(len(data)))

	if _, err := c.Writer.Write(data); err != nil {
		log.Warnf("write response body failed: %v", err)
	}

	return adaptor.DoResponseResult{Usage: usage, UsageContext: usageContext}, nil
}

func doubaoImageResponseToOpenAI(
	response doubaoImageResponse,
	usage coremodel.Usage,
	preserveMeasurements ...bool,
) relaymodel.ImageResponse {
	data := make([]*relaymodel.ImageData, 0, len(response.Data))
	for _, item := range response.Data {
		if item == nil || item.Error != nil {
			continue
		}

		converted := &relaymodel.ImageData{
			URL:           item.URL,
			B64Json:       item.B64JSON,
			RevisedPrompt: item.RevisedPrompt,
		}
		if len(preserveMeasurements) > 0 && preserveMeasurements[0] {
			converted.Size = item.Size
			converted.ZIndex = item.ZIndex
		}

		data = append(data, converted)
	}

	return relaymodel.ImageResponse{
		Created: response.Created,
		Data:    data,
		Usage:   doubaoImageUsageToOpenAIUsage(usage),
	}
}

func ImageStreamHandler(
	meta *meta.Meta,
	c *gin.Context,
	resp *http.Response,
) (adaptor.DoResponseResult, adaptor.Error) {
	if resp.StatusCode != http.StatusOK {
		return adaptor.DoResponseResult{}, ErrorHandler(resp)
	}

	defer resp.Body.Close()

	log := common.GetLogger(c)

	scanner, cleanup := relayutils.NewStreamScanner(resp.Body, meta.ActualModel)
	defer cleanup()

	usage := coremodel.Usage{}
	usageContext := meta.RequestUsageContext

	var completedData relaymodel.ImageStreamEvent

	completedImageIndexes := map[int]struct{}{}

	for scanner.Scan() {
		line := scanner.Bytes()
		if !render.IsValidSSEData(line) {
			continue
		}

		data := render.ExtractSSEData(line)
		if render.IsSSEDone(data) {
			break
		}

		var event doubaoImageStreamEvent
		if err := sonic.Unmarshal(data, &event); err != nil {
			log.Errorf("error unmarshalling doubao image stream response: %v", err)
			render.OpenaiBytesData(c, data)
			continue
		}

		openAIEvent, eventUsage, eventContext := convertDoubaoImageStreamEvent(
			event,
			int64(len(completedImageIndexes)),
		)
		if eventUsage != nil {
			usage = *eventUsage
		}

		if openAIEvent.Type == relaymodel.ImageStreamEventPartialImage {
			if openAIEvent.Error == nil && (openAIEvent.URL != "" || openAIEvent.B64Json != "") {
				index := event.ImageIndex
				completedImageIndexes[index] = struct{}{}
			}

			completedData.B64Json = openAIEvent.B64Json
			completedData.URL = openAIEvent.URL
			completedData.Size = openAIEvent.Size
		}

		if openAIEvent.Type == relaymodel.ImageStreamEventCompleted {
			openAIEvent.B64Json = firstNonEmptyString(openAIEvent.B64Json, completedData.B64Json)
			openAIEvent.URL = firstNonEmptyString(openAIEvent.URL, completedData.URL)
			openAIEvent.Size = firstNonEmptyString(openAIEvent.Size, completedData.Size)
		}

		usageContext = eventContext.WithFallback(usageContext)

		if err := render.ResponsesObjectData(c, openAIEvent); err != nil {
			log.Errorf("write doubao image stream response failed: %v", err)
		}
	}

	if err := scanner.Err(); err != nil {
		log.Errorf("error reading doubao image stream: %v", err)
	}

	return adaptor.DoResponseResult{Usage: usage, UsageContext: usageContext}, nil
}

func convertDoubaoImageStreamEvent(
	event doubaoImageStreamEvent,
	fallbackImageCount int64,
) (relaymodel.ImageStreamEvent, *coremodel.Usage, coremodel.UsageContext) {
	openAIEvent := relaymodel.ImageStreamEvent{
		Type:      event.Type,
		B64Json:   event.B64JSON,
		URL:       event.URL,
		CreatedAt: event.Created,
		Size:      normalizeDoubaoImageSize(event.Size),
		Error:     event.Error,
	}

	usageContext := coremodel.UsageContext{}
	if openAIEvent.Size != "" {
		usageContext.Resolution = openAIEvent.Size
	}

	switch event.Type {
	case doubaoImageStreamEventPartialSucceeded:
		index := event.ImageIndex
		openAIEvent.Type = relaymodel.ImageStreamEventPartialImage
		openAIEvent.PartialImageIndex = &index
	case relaymodel.ImageStreamEventCompleted:
		openAIEvent.Type = relaymodel.ImageStreamEventCompleted

		if fallbackImageCount == 0 &&
			event.Error == nil &&
			(event.URL != "" || event.B64JSON != "") {
			fallbackImageCount = 1
		}

		usage := doubaoImageUsageToModelUsage(event.Usage, nil, fallbackImageCount)
		openAIEvent.Usage = doubaoImageUsageToOpenAIUsage(usage)

		return openAIEvent, &usage, usageContext
	}

	return openAIEvent, nil, usageContext
}

func doubaoImageUsageToOpenAIUsage(usage coremodel.Usage) *relaymodel.ImageUsage {
	return &relaymodel.ImageUsage{
		InputTokens:  int64(usage.InputTokens),
		OutputTokens: int64(usage.OutputTokens),
		TotalTokens:  int64(usage.TotalTokens),
		InputTokensDetails: relaymodel.ImageInputTokensDetails{
			ImageTokens: int64(usage.ImageInputTokens),
		},
		OutputTokensDetails: &relaymodel.ImageOutputTokensDetails{
			ImageTokens: int64(usage.ImageOutputTokens),
		},
	}
}

func doubaoImageUsageToModelUsage(
	usage *doubaoImageUsage,
	data []*doubaoImageData,
	fallbackImageCount int64,
) coremodel.Usage {
	if usage == nil {
		imageCount := countSuccessfulDoubaoImages(data)
		if imageCount == 0 {
			imageCount = fallbackImageCount
		}

		return coremodel.Usage{
			OutputTokens:      coremodel.ZeroNullInt64(imageCount),
			ImageOutputTokens: coremodel.ZeroNullInt64(imageCount),
			TotalTokens:       coremodel.ZeroNullInt64(imageCount),
		}
	}

	imageCount := usage.GeneratedImages
	if imageCount == 0 {
		imageCount = countSuccessfulDoubaoImages(data)
	}

	if imageCount == 0 {
		imageCount = fallbackImageCount
	}

	// Seedream returns both the successful image count and token usage. Token-priced
	// models should bill output_tokens; per-image fallbacks can use ImageOutputTokens.
	outputTokens := usage.OutputTokens
	if outputTokens == 0 {
		outputTokens = imageCount
	}

	totalTokens := outputTokens
	if usage.TotalTokens != 0 {
		totalTokens = usage.TotalTokens
	}

	return coremodel.Usage{
		OutputTokens:      coremodel.ZeroNullInt64(outputTokens),
		ImageOutputTokens: coremodel.ZeroNullInt64(imageCount),
		TotalTokens:       coremodel.ZeroNullInt64(totalTokens),
		WebSearchCount:    coremodel.ZeroNullInt64(usage.ToolUsage.WebSearch),
	}
}

func doubaoImageUsageContext(data []*doubaoImageData) coremodel.UsageContext {
	for _, item := range data {
		if item == nil || item.Size == "" {
			continue
		}

		return coremodel.UsageContext{
			Resolution: normalizeDoubaoImageSize(item.Size),
		}
	}

	return coremodel.UsageContext{}
}

func countSuccessfulDoubaoImages(data []*doubaoImageData) int64 {
	output := int64(0)
	for _, item := range data {
		if item != nil && item.Error == nil && (item.URL != "" || item.B64JSON != "") {
			output++
		}
	}

	return output
}

func normalizeDoubaoImageSize(size string) string {
	return normalizeDoubaoSize(size)
}

func normalizeDoubaoImageRequestSize(size string) string {
	size = strings.TrimSpace(size)
	size = strings.ReplaceAll(size, "×", "x")
	size = strings.ReplaceAll(size, "*", "x")

	return size
}

// Presence must be retained: an omitted count is not an explicit zero.
func (u *doubaoImageUsage) UnmarshalJSON(data []byte) error {
	type plain doubaoImageUsage

	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}

	var counts struct {
		Generated *int64 `json:"generated_images"`
	}
	if err := json.Unmarshal(data, &counts); err != nil {
		return err
	}

	*u = doubaoImageUsage(v)
	u.generatedCount = counts.Generated

	return nil
}

// Measurements come only from the final upstream body, never the request or URLs.
func measureDoubaoImages(response doubaoImageResponse, scenario string) *coremodel.ImageUsage {
	usage := &coremodel.ImageUsage{Version: 1, Scenario: scenario, State: "complete"}
	if response.Error != nil {
		usage.State = "failed"
		return usage
	}

	usage.Outputs = make(
		[]coremodel.ImageUsageOutput,
		0,
		min(len(response.Data), coremodel.ImageBillingMaxOutputs),
	)
	if response.Data == nil || len(response.Data) > coremodel.ImageBillingMaxOutputs {
		usage.State = "incomplete"
	}

	if response.Usage != nil {
		usage.InputCount = response.Usage.InputImages
		usage.GeneratedCount = response.Usage.generatedCount
	}

	for i, item := range response.Data {
		if i >= coremodel.ImageBillingMaxOutputs {
			break
		}

		if item != nil && item.Error != nil {
			if scenario == "layer_decomposition" {
				usage.State = "failed"
				usage.Outputs = nil
				return usage
			}

			continue
		}

		if item == nil || (item.URL == "" && item.B64JSON == "") {
			usage.State = "incomplete"
			continue
		}

		output := coremodel.ImageUsageOutput{Index: int64(i), ZIndex: item.ZIndex}

		parts := strings.Split(normalizeDoubaoImageSize(item.Size), "x")
		if len(parts) == 2 {
			w, ew := strconv.ParseInt(parts[0], 10, 64)

			h, eh := strconv.ParseInt(parts[1], 10, 64)
			if ew == nil && eh == nil && w > 0 && h > 0 &&
				w <= coremodel.ImageBillingMaxDimension &&
				h <= coremodel.ImageBillingMaxDimension {
				output.Width = &w
				output.Height = &h
			}
		}

		usage.Outputs = append(usage.Outputs, output)
	}

	// Supplier cost and retail evaluate the same evidence independently. A free
	// retail input rate cannot waive the provider's nonempty-result measurement.
	if len(usage.Outputs) > 0 && usage.InputCount == nil {
		usage.State = "incomplete"
	}

	if err := usage.Validate(true); err != nil {
		usage.State = "incomplete"
	}

	return usage
}
