package fal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/labring/aiproxy/core/common/failover"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/relay/adaptor"
)

// NativeResult carries the complete provider output, not an image projection.
// The executor must validate it using the frozen contract before persistence.
type NativeResult = nativeresult.PollResult

// nativeRequest preserves the complete bounded response, including trailing
// bytes, so the native validator can reject ambiguous/malformed JSON itself.
func (c *Client) nativeRequest(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	base := c.BaseURL
	if base == "" {
		base = "https://queue.fal.run"
	}
	ctx, classify := failover.TraceTransport(ctx)
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+"/"+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, errors.New("invalid fal endpoint")
	}
	if method != http.MethodGet && method != http.MethodHead {
		request.GetBody = nil
	}
	request.Header.Set("Authorization", "Key "+c.Key)
	request.Header.Set("Content-Type", "application/json")
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 45 * time.Second}
	}
	transport := *client
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := transport.Do(request)
	if err != nil {
		return nil, 0, &adaptor.ImageSubmissionFailure{Failure: classify(err)}
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, nativeresult.MaxBytes+1))
	if err != nil {
		return nil, response.StatusCode, errors.New("invalid fal response")
	}
	if len(raw) > nativeresult.MaxBytes {
		return nil, response.StatusCode, nativeresult.ErrTooLarge
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, response.StatusCode, &queueResultFailure{status: response.StatusCode}
	}
	return raw, response.StatusCode, nil
}

// SubmitNative shares fal's paid-submission acceptance classification and model
// path validation; the input is already frozen-schema validated by the executor.
func (c *Client) SubmitNative(ctx context.Context, modelName string, input []byte) (string, error) {
	return c.Submit(ctx, modelName, input)
}

func (c *Client) PollNative(ctx context.Context, modelName, id string, contract *nativeresult.CompiledTask) (NativeResult, error) {
	root, err := endpoint(modelName)
	if err != nil {
		return NativeResult{}, err
	}
	if !segment.MatchString(id) || contract == nil {
		return NativeResult{}, errors.New("invalid native task binding")
	}
	path := root + "/requests/" + id
	raw, _, err := c.nativeRequest(ctx, http.MethodGet, path+"/status", nil)
	if err != nil {
		return NativeResult{}, err
	}
	statusValidator, err := nativeresult.Compile([]byte(`{"type":"object","required":["status"],"properties":{"status":{"type":"string"}}}`))
	if err != nil {
		return NativeResult{}, err
	}
	if _, err = statusValidator.Validate(raw); err != nil {
		return NativeResult{}, errors.New("invalid fal status")
	}
	var state struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(raw, &state) != nil {
		return NativeResult{}, errors.New("invalid fal status")
	}
	switch state.Status {
	case "IN_QUEUE":
		return NativeResult{Status: "queued"}, nil
	case "IN_PROGRESS":
		return NativeResult{Status: "running"}, nil
	case "FAILED":
		return NativeResult{Status: "failed", ErrorCode: "upstream_task_failed"}, nil
	case "COMPLETED":
		raw, code, err := c.nativeRequest(ctx, http.MethodGet, path, nil)
		if err != nil {
			if code == 400 || code == 422 {
				return NativeResult{Status: "failed", ErrorCode: "upstream_result_rejected"}, nil
			}
			return NativeResult{}, err
		}
		output, err := contract.ValidateOutput(raw)
		if err != nil {
			return NativeResult{}, err
		}
		return NativeResult{Status: "result_received", Output: output}, nil
	default:
		return NativeResult{}, errors.New("unknown fal task status")
	}
}
