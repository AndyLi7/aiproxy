package fal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/labring/aiproxy/core/relay/adaptor/openai"
	"github.com/labring/aiproxy/core/relay/adaptor/registry"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type Adaptor struct{ openai.Adaptor }

func init()                                    { registry.Register(model.ChannelTypeFal, &Adaptor{}) }
func (*Adaptor) DefaultBaseURL() string        { return "https://queue.fal.run" }
func (*Adaptor) SupportMode(m *meta.Meta) bool { return m.Mode == mode.ImagesGenerations }
func (*Adaptor) Metadata() adaptor.Metadata {
	return adaptor.Metadata{KeyHelp: "fal API key; used only by durable image tasks"}
}
func (a *Adaptor) SubmitImage(ctx context.Context, m *meta.Meta, body []byte) (string, error) {
	return (&Client{BaseURL: m.Channel.BaseURL, Key: m.Channel.Key}).Submit(ctx, m.ActualModel, body)
}
func (a *Adaptor) PollImage(ctx context.Context, ch *model.Channel, info *model.AsyncUsageInfo, task *model.ImageTask) (adaptor.ImageTaskResult, error) {
	return (&Client{BaseURL: info.BaseURL, Key: ch.Key}).Poll(ctx, task.UpstreamModel, task.UpstreamID)
}

type Client struct {
	HTTP         *http.Client
	BaseURL, Key string
}

var segment = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func endpoint(modelName string) (string, error) {
	parts := strings.Split(modelName, "/")
	if len(parts) < 2 {
		return "", errors.New("invalid fal model")
	}
	for _, p := range parts {
		if !segment.MatchString(p) {
			return "", errors.New("invalid fal model")
		}
	}
	return strings.Join(parts[:2], "/"), nil
}
func (c *Client) request(ctx context.Context, method, path string, body []byte, out any) (int, error) {
	base := c.BaseURL
	if base == "" {
		base = "https://queue.fal.run"
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+"/"+path, bytes.NewReader(body))
	if err != nil {
		return 0, errors.New("invalid fal endpoint")
	}
	req.Header.Set("Authorization", "Key "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 45 * time.Second}
	}
	// Never follow an upstream redirect with credentials or resubmit a paid POST.
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := copyClient.Do(req)
	if err != nil {
		return 0, errors.New("fal transport unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("fal returned HTTP %d", resp.StatusCode)
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(out); err != nil {
		return resp.StatusCode, errors.New("invalid fal response")
	}
	return resp.StatusCode, nil
}
func (c *Client) Submit(ctx context.Context, modelName string, body []byte) (string, error) {
	if _, err := endpoint(modelName); err != nil {
		return "", err
	}
	var result struct {
		ID string `json:"request_id"`
	}
	code, err := c.request(ctx, http.MethodPost, modelName, body, &result)
	if code >= 400 && code < 500 {
		return "", adaptor.ErrImageSubmissionRejected
	}
	if err != nil {
		return "", err
	}
	if !segment.MatchString(result.ID) {
		return "", errors.New("invalid fal request id")
	}
	return result.ID, nil
}
func failed(code string) adaptor.ImageTaskResult {
	return adaptor.ImageTaskResult{Status: "failed", Error: &model.ImageTaskError{Code: code, Message: "Image generation failed"}}
}
func (c *Client) Poll(ctx context.Context, modelName, id string) (adaptor.ImageTaskResult, error) {
	root, err := endpoint(modelName)
	if err != nil {
		return adaptor.ImageTaskResult{}, err
	}
	if !segment.MatchString(id) {
		return adaptor.ImageTaskResult{}, errors.New("invalid fal request id")
	}
	path := root + "/requests/" + id
	var status struct {
		Status string `json:"status"`
		Error  any    `json:"error"`
	}
	_, err = c.request(ctx, http.MethodGet, path+"/status", nil, &status)
	if err != nil {
		return adaptor.ImageTaskResult{}, err
	}
	switch status.Status {
	case "IN_QUEUE":
		return adaptor.ImageTaskResult{Status: "queued"}, nil
	case "IN_PROGRESS":
		return adaptor.ImageTaskResult{Status: "in_progress"}, nil
	case "FAILED", "CANCELLED":
		return failed("upstream_failed"), nil
	case "COMPLETED":
	default:
		return adaptor.ImageTaskResult{}, errors.New("unrecognized fal queue status")
	}
	if status.Error != nil {
		return failed("upstream_failed"), nil
	}
	var result struct {
		Images []model.ImageOutput `json:"images"`
	}
	code, err := c.request(ctx, http.MethodGet, path, nil, &result)
	if err != nil {
		if code == 400 || code == 422 {
			return failed("upstream_failed"), nil
		}
		return adaptor.ImageTaskResult{}, err
	}
	if len(result.Images) == 0 {
		return failed("invalid_result"), nil
	}
	for _, img := range result.Images {
		u, e := url.Parse(img.URL)
		if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (img.ContentType != "" && !strings.HasPrefix(img.ContentType, "image/")) {
			return failed("invalid_result"), nil
		}
	}
	return adaptor.ImageTaskResult{Status: "completed", Data: result.Images}, nil
}

// Synchronous/demo relay must never accidentally send a paid queue request.
func (*Adaptor) GetRequestURL(*meta.Meta, adaptor.Store, *gin.Context) (adaptor.RequestURL, error) {
	return adaptor.RequestURL{}, errors.New("fal requires /v1/images/tasks")
}
func (*Adaptor) ImageAdapterName() string { return "fal-image" }
