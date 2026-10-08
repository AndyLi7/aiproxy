package fal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/failover"
	"github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/labring/aiproxy/core/relay/adaptor/openai"
	"github.com/labring/aiproxy/core/relay/adaptor/registry"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
)

type Adaptor struct{ openai.Adaptor }

func init()                             { registry.Register(model.ChannelTypeFal, &Adaptor{}) }
func (*Adaptor) DefaultBaseURL() string { return "https://queue.fal.run" }

// fal channels serve durable image tasks and native model tasks only.
func (*Adaptor) SupportMode(m *meta.Meta) bool {
	return m.Mode == mode.ImagesGenerations || m.Mode == mode.NativeTasks
}
func (*Adaptor) Metadata() adaptor.Metadata {
	return adaptor.Metadata{KeyHelp: "fal API key; used by durable image tasks and native model tasks"}
}

func (a *Adaptor) SubmitImage(ctx context.Context, m *meta.Meta, body []byte) (string, error) {
	return (&Client{BaseURL: m.Channel.BaseURL, Key: m.Channel.Key}).Submit(
		ctx,
		m.ActualModel,
		body,
	)
}

func (a *Adaptor) PollImage(
	ctx context.Context,
	ch *model.Channel,
	info *model.AsyncUsageInfo,
	task *model.ImageTask,
) (adaptor.ImageTaskResult, error) {
	return (&Client{BaseURL: info.BaseURL, Key: ch.Key}).Poll(
		ctx,
		task.UpstreamModel,
		task.UpstreamID,
		[]byte(task.ValidationContract),
	)
}

// queueResultFailure identifies a structured runner failure, not a transient
// HTTP gateway failure. Never include upstream response text in public errors.
type queueResultFailure struct {
	status   int
	terminal bool
	issues   []model.ImageParameterIssue
	// reason is the sanitized summary for server logs (adaptor.ProviderErrorReason).
	reason string
}

func (e *queueResultFailure) Error() string { return fmt.Sprintf("fal returned HTTP %d", e.status) }

type Client struct {
	HTTP         *http.Client
	BaseURL, Key string
}

var (
	validSeed    = regexp.MustCompile(`^-?(0|[1-9][0-9]{0,39})$`)
	segment      = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	modelSegment = regexp.MustCompile(`^[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*$`)
)

func endpoint(modelName string) (string, error) {
	parts := strings.Split(modelName, "/")
	if len(parts) < 2 {
		return "", errors.New("invalid fal model")
	}

	for _, p := range parts {
		if !modelSegment.MatchString(p) {
			return "", errors.New("invalid fal model")
		}
	}

	return strings.Join(parts[:2], "/"), nil
}

func (c *Client) request(
	ctx context.Context,
	method, path string,
	body []byte,
	out any,
	captureHeaders ...*http.Header,
) (int, error) {
	base := c.BaseURL
	if base == "" {
		base = "https://queue.fal.run"
	}

	ctx, classifyTransport := failover.TraceTransport(ctx)
	req, err := http.NewRequestWithContext(
		ctx,
		method,
		strings.TrimRight(base, "/")+"/"+path,
		bytes.NewReader(body),
	)
	if err != nil {
		return 0, errors.New("invalid fal endpoint")
	}

	// Paid submissions must never be replayed implicitly by net/http, even
	// if an idempotency header is introduced by an outbound transport later.
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		req.GetBody = nil
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
		return 0, &adaptor.ImageSubmissionFailure{Failure: classifyTransport(err), ProviderReason: transportReason(err)}
	}
	defer resp.Body.Close()
	if len(captureHeaders) == 1 && captureHeaders[0] != nil {
		*captureHeaders[0] = resp.Header.Clone()
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		failure := &queueResultFailure{status: resp.StatusCode, reason: adaptor.ProviderErrorReason(resp.StatusCode, raw, c.Key)}
		var envelope struct {
			Detail []struct {
				Type string `json:"type"`
				Loc  []any  `json:"loc"`
			} `json:"detail"`
		}
		if json.NewDecoder(bytes.NewReader(raw)).Decode(&envelope) == nil {
			for _, detail := range envelope.Detail {
				if issue, ok := parameterIssue(detail.Type, detail.Loc); ok && len(failure.issues) < 8 {
					failure.issues = append(failure.issues, issue)
				}
				if detail.Type == "downstream_service_unavailable" {
					failure.terminal = true
				}
			}
		}
		return resp.StatusCode, failure
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
	var failure *queueResultFailure
	reason := ""
	if errors.As(err, &failure) {
		reason = failure.reason
	}
	// Owner rule 2026-10-08: see adaptor.InputRejectionStatus and
	// adaptor.ProviderUnavailableStatus. Only these statuses prove fal created
	// no request; every other answer leaves acceptance unknown.
	switch {
	case adaptor.InputRejectionStatus(code):
		var public *model.ImageTaskError
		if code == 422 && failure != nil && len(failure.issues) > 0 {
			public = &model.ImageTaskError{Code: "invalid_parameters", Message: "Request parameters are invalid; check the listed fields", Issues: failure.issues}
		}
		return "", adaptor.NewSubmissionRejected(code, reason, public)
	case adaptor.ProviderUnavailableStatus(code):
		return "", adaptor.NewProviderUnavailable(code, reason)
	case err != nil:
		var transport *adaptor.ImageSubmissionFailure
		if errors.As(err, &transport) {
			return "", transport
		}
		if reason == "" {
			reason = "unreadable response body"
		}
		return "", adaptor.NewSubmissionUnknown(code, reason)
	}

	if !segment.MatchString(result.ID) {
		return "", adaptor.NewSubmissionUnknown(code, "response without a valid request_id")
	}

	return result.ID, nil
}

// transportReason labels a transport error for server logs without its text,
// which can contain the request URL.
func transportReason(err error) string {
	var timeout interface{ Timeout() bool }
	switch {
	case errors.Is(err, context.Canceled):
		return "request canceled"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		return "request timed out"
	}
	return "transport error"
}

func failed(code string) adaptor.ImageTaskResult {
	return adaptor.ImageTaskResult{
		Status: "failed",
		Error:  &model.ImageTaskError{Code: code, Message: "Image generation failed"},
	}
}

func (c *Client) Poll(
	ctx context.Context,
	modelName, id string,
	frozenContracts ...[]byte,
) (adaptor.ImageTaskResult, error) {
	root, err := endpoint(modelName)
	if err != nil {
		return adaptor.ImageTaskResult{}, err
	}

	if !segment.MatchString(id) {
		return adaptor.ImageTaskResult{}, errors.New("invalid fal request id")
	}

	path := root + "/requests/" + id
	statusPath := path + "/status"

	if len(frozenContracts) > 1 {
		return adaptor.ImageTaskResult{}, errors.New("multiple task contracts")
	}

	if len(frozenContracts) == 1 && registryvalidation.HasProviderContracts(frozenContracts[0]) {
		statusPath, path, err = registryvalidation.FrozenFalQueuePaths(
			frozenContracts[0],
			modelName,
			id,
		)
		if err != nil {
			return failed("invalid_contract"), nil
		}
	}

	var status struct {
		Status string `json:"status"`
		Error  any    `json:"error"`
	}

	statusCode, err := c.request(ctx, http.MethodGet, statusPath, nil, &status)
	// Some fal snapshots use the inference subpath for queue reads.
	// On an explicit method rejection, query the same accepted request at the
	// validated owner/app queue root. This never resubmits or changes channels.
	legacyPath := root + "/requests/" + id
	if statusCode == http.StatusMethodNotAllowed &&
		statusPath == modelName+"/requests/"+id+"/status" && path != legacyPath {
		path = legacyPath
		statusPath = path + "/status"
		_, err = c.request(ctx, http.MethodGet, statusPath, nil, &status)
	}
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
		NumImages     *int64              `json:"num_images"`
		Seeds         []json.Number       `json:"seeds"`
		ActualPrompt  string              `json:"actual_prompt"`
		RevisedPrompt *string             `json:"revised_prompt"`
		Description   string              `json:"description"`
		Seed          *json.Number        `json:"seed"`
		UsedSeed      *json.Number        `json:"used_seed"`
		Prompt        string              `json:"prompt"`
		Images        []model.ImageOutput `json:"images"`
		Image         *model.ImageOutput  `json:"image"`
	}

	var rawResult json.RawMessage

	var resultHeaders http.Header
	code, err := c.request(ctx, http.MethodGet, path, nil, &rawResult, &resultHeaders)
	if err != nil {
		var runnerFailure *queueResultFailure
		errors.As(err, &runnerFailure)
		if code == 400 || code == 422 ||
			(errors.As(err, &runnerFailure) && runnerFailure.terminal) {
			if runnerFailure != nil && code == 422 && len(runnerFailure.issues) > 0 {
				return adaptor.ImageTaskResult{Status: "failed", Error: &model.ImageTaskError{Code: "invalid_parameters", Message: "Request parameters are invalid; check the listed fields", Issues: runnerFailure.issues}}, nil
			}
			return failed("upstream_failed"), nil
		}
		return adaptor.ImageTaskResult{}, err
	}

	mapTypeEnabled := false
	providerMetadata := map[string]json.RawMessage{}
	for _, frozen := range frozenContracts {
		mapTypeEnabled = mapTypeEnabled || registryvalidation.FrozenImageMapTypeEnabled(frozen)
		projected, err := registryvalidation.ExtractFrozenProviderMetadata(frozen, rawResult)
		if err != nil {
			return failed("invalid_result"), nil
		}
		for name, value := range projected {
			providerMetadata[name] = value
		}
	}

	if json.Unmarshal(rawResult, &result) != nil {
		return adaptor.ImageTaskResult{}, errors.New("invalid fal result")
	}

	var fields map[string]json.RawMessage
	if json.Unmarshal(rawResult, &fields) != nil {
		return failed("invalid_result"), nil
	}
	namedFields := []string(nil)
	for _, frozen := range frozenContracts {
		names, err := registryvalidation.FrozenNamedImageOutputs(frozen)
		if err != nil {
			return failed("invalid_result"), nil
		}
		if len(names) > 0 {
			if namedFields != nil && !reflect.DeepEqual(namedFields, names) {
				return failed("invalid_result"), nil
			}
			namedFields = names
		}
	}
	if len(namedFields) > 0 {
		result.Images = nil
		result.Image = nil
		for _, name := range namedFields {
			var image model.ImageOutput
			if len(fields[name]) == 0 || json.Unmarshal(fields[name], &image) != nil || image.URL == "" {
				return failed("invalid_result"), nil
			}
			image.SourceField = name
			result.Images = append(result.Images, image)
		}
	}
	_, hasImages := fields["images"]
	_, hasImage := fields["image"]
	if len(namedFields) == 0 && hasImages && hasImage {
		allowed := false
		for _, frozen := range frozenContracts {
			ok, err := registryvalidation.FrozenCombinedImageOutputs(frozen)
			if err != nil {
				return failed("invalid_result"), nil
			}
			allowed = allowed || ok
		}
		if !allowed || result.Image == nil {
			return failed("invalid_result"), nil
		}
		combined := []model.ImageOutput{*result.Image}
		for _, candidate := range result.Images {
			duplicate := false
			for _, existing := range combined {
				if candidate.URL == existing.URL {
					if !reflect.DeepEqual(candidate, existing) {
						return failed("invalid_result"), nil
					}
					duplicate = true
					break
				}
			}
			if !duplicate {
				combined = append(combined, candidate)
			}
		}
		result.Images = combined
	}
	if len(namedFields) == 0 && !hasImages && result.Image != nil {
		result.Images = []model.ImageOutput{*result.Image}
	}
	if len(result.Images) == 0 {
		return failed("invalid_result"), nil
	}

	for _, frozen := range frozenContracts {
		if err := registryvalidation.CheckFrozenOutputControls(frozen, rawResult, len(result.Images)); err != nil {
			if errors.Is(err, registryvalidation.ErrUnsafeImageResult) {
				return failed("content_filtered"), nil
			}
			return failed("invalid_result"), nil
		}
	}
	for _, img := range result.Images {
		u, e := url.Parse(img.URL)
		if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
			u.Fragment != "" ||
			(img.ContentType != "" && !strings.HasPrefix(img.ContentType, "image/")) {
			return failed("invalid_result"), nil
		}
	}

	if _, present := fields["seeds"]; present && len(result.Seeds) != len(result.Images) {
		return failed("invalid_result"), nil
	}
	if result.RevisedPrompt != nil {
		if raw, present := fields["actual_prompt"]; present && string(raw) != "null" && result.ActualPrompt != *result.RevisedPrompt {
			return failed("invalid_result"), nil
		}
		for _, img := range result.Images {
			if img.RevisedPrompt != "" && img.RevisedPrompt != *result.RevisedPrompt {
				return failed("invalid_result"), nil
			}
		}
		result.ActualPrompt = *result.RevisedPrompt
	}
	if result.Prompt == "" {
		result.Prompt = result.ActualPrompt
	}
	for i := range result.Images {
		// Delivery state is platform-owned, never accepted from provider JSON.
		if len(namedFields) == 0 {
			result.Images[i].SourceField = ""
		}
		if !mapTypeEnabled {
			result.Images[i].MapType = ""
		}
		result.Images[i].Layer = nil
		result.Images[i].Stored = false
		result.Images[i].URLExpiresAt = nil
		result.Images[i].AuxiliaryImages = nil
		if result.Images[i].Seed != nil && !validSeed.MatchString(result.Images[i].Seed.String()) {
			return failed("invalid_result"), nil
		}
		if result.ActualPrompt != "" && result.Images[i].RevisedPrompt != "" && result.Images[i].RevisedPrompt != result.ActualPrompt {
			return failed("invalid_result"), nil
		}
		if len(result.Seeds) > 0 {
			if !validSeed.MatchString(result.Seeds[i].String()) {
				return failed("invalid_result"), nil
			}
			if result.Images[i].Seed != nil && *result.Images[i].Seed != result.Seeds[i] {
				return failed("invalid_result"), nil
			}
			result.Images[i].Seed = &result.Seeds[i]
		}

		if result.Images[i].RevisedPrompt == "" {
			result.Images[i].RevisedPrompt = result.ActualPrompt
			if result.Images[i].RevisedPrompt == "" && result.RevisedPrompt == nil {
				result.Images[i].RevisedPrompt = result.Prompt
			}
		}
	}
	var layerMetadata []json.RawMessage
	for _, frozen := range frozenContracts {
		layers, err := registryvalidation.ExtractFrozenLayerMetadata(frozen, rawResult)
		if err != nil {
			return failed("invalid_result"), nil
		}
		if len(layers) == 0 {
			continue
		}
		if len(layers) != len(result.Images) || (layerMetadata != nil && !reflect.DeepEqual(layerMetadata, layers)) {
			return failed("invalid_result"), nil
		}
		layerMetadata = layers
	}
	for i, layer := range layerMetadata {
		result.Images[i].Layer = append([]byte(nil), layer...)
	}
	if result.UsedSeed != nil {
		if result.Seed != nil && result.Seed.String() != result.UsedSeed.String() {
			return failed("invalid_result"), nil
		}
		result.Seed = result.UsedSeed
	}
	var legacySeed *int64
	seedExact := ""
	if result.Seed != nil {
		if !validSeed.MatchString(result.Seed.String()) {
			return failed("invalid_result"), nil
		}
		if value, e := result.Seed.Int64(); e == nil {
			legacySeed = &value
		} else {
			seedExact = result.Seed.String()
		}
	}
	for _, frozen := range frozenContracts {
		assets, err := registryvalidation.ExtractFrozenAuxiliaryImages(frozen, rawResult)
		if err != nil {
			return failed("invalid_result"), nil
		}
		if len(assets) == 0 {
			continue
		}
		if len(result.Images) != 1 {
			return failed("invalid_result"), nil
		}
		if result.Images[0].AuxiliaryImages == nil {
			result.Images[0].AuxiliaryImages = map[string]*model.ImageOutput{}
		}
		for name, rawAsset := range assets {
			if string(bytes.TrimSpace(rawAsset)) == "null" {
				result.Images[0].AuxiliaryImages[name] = nil
				continue
			}
			var asset model.ImageOutput
			if json.Unmarshal(rawAsset, &asset) != nil {
				return failed("invalid_result"), nil
			}
			u, err := url.Parse(asset.URL)
			if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (asset.ContentType != "" && !strings.HasPrefix(asset.ContentType, "image/")) {
				return failed("invalid_result"), nil
			}
			// Project only the source-declared image asset fields. New primary-image
			// metadata must never leak into auxiliary assets by default.
			asset = model.ImageOutput{URL: asset.URL, ContentType: asset.ContentType, Width: asset.Width, Height: asset.Height}
			result.Images[0].AuxiliaryImages[name] = &asset
		}
	}
	return adaptor.ImageTaskResult{Status: "completed", Data: result.Images, Metadata: model.ImageResultMetadata{ProviderMetadata: providerMetadata, NumImages: result.NumImages, Description: result.Description, Seed: legacySeed, SeedExact: seedExact, Prompt: result.Prompt, BillableUnits: resultHeaders.Get("x-fal-billable-units")}}, nil
}

// Synchronous/demo relay must never accidentally send a paid queue request.
func (*Adaptor) GetRequestURL(*meta.Meta, adaptor.Store, *gin.Context) (adaptor.RequestURL, error) {
	return adaptor.RequestURL{}, errors.New("fal requires /v1/images/tasks")
}
func (*Adaptor) ImageAdapterName() string { return "fal-image" }
