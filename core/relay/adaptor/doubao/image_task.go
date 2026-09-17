package doubao

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/utils"
)

func (*Adaptor) ImageAdapterName() string { return "volcengine-ark-image" }
func (*Adaptor) GenerateImage(ctx context.Context, m *meta.Meta, body, frozen []byte) (adaptor.ImageTaskResult, error) {
	var input map[string]any
	if !registryvalidation.HasProviderContracts(frozen) || json.Unmarshal(body, &input) != nil || input == nil || input["stream"] != false || input["response_format"] != "url" {
		return adaptor.ImageTaskResult{}, adaptor.ErrImageSubmissionRejected
	}
	input["model"] = m.ActualModel
	encoded, err := json.Marshal(input)
	if err != nil {
		return adaptor.ImageTaskResult{}, adaptor.ErrImageSubmissionRejected
	}
	base := strings.TrimRight(m.Channel.BaseURL, "/")
	if !strings.HasSuffix(base, "/api/v3") {
		base += "/api/v3"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/images/generations", bytes.NewReader(encoded))
	if err != nil {
		return adaptor.ImageTaskResult{}, adaptor.ErrImageSubmissionRejected
	}
	req.Header.Set("Authorization", "Bearer "+m.Channel.Key)
	req.Header.Set("Content-Type", "application/json")
	client, err := utils.LoadHTTPClientWithOutboundPolicyE(45*time.Second, m.Channel.ProxyURL, m.Channel.SkipTLSVerify, utils.OutboundPolicyFromConfigs(m.ChannelConfigs))
	if err != nil {
		return adaptor.ImageTaskResult{}, adaptor.ErrImageSubmissionRejected
	}
	once := *client
	once.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := once.Do(req)
	if err != nil {
		return adaptor.ImageTaskResult{}, errors.New("synchronous image submission outcome unknown")
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 && response.StatusCode < 500 {
		return adaptor.ImageTaskResult{}, adaptor.ErrImageSubmissionRejected
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return adaptor.ImageTaskResult{}, errors.New("synchronous image submission outcome unknown")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil {
		return adaptor.ImageTaskResult{}, errors.New("synchronous image response incomplete")
	}
	failed := adaptor.ImageTaskResult{Status: "failed", Error: &model.ImageTaskError{Code: "invalid_result", Message: "Image generation failed"}}
	if len(raw) > 2<<20 || registryvalidation.ValidateFrozenProviderOutput(frozen, raw) != nil {
		return failed, nil
	}
	var result struct {
		Data []struct {
			URL   string          `json:"url"`
			Error json.RawMessage `json:"error"`
		} `json:"data"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &result) != nil || len(result.Data) == 0 || (len(result.Error) > 0 && string(result.Error) != "null") {
		return failed, nil
	}
	output := make([]model.ImageOutput, 0, len(result.Data))
	for _, item := range result.Data {
		u, err := url.Parse(item.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (len(item.Error) > 0 && string(item.Error) != "null") {
			return failed, nil
		}
		output = append(output, model.ImageOutput{URL: item.URL})
	}
	return adaptor.ImageTaskResult{Status: "completed", Data: output}, nil
}
