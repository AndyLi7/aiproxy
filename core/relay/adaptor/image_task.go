package adaptor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/labring/aiproxy/core/common/failover"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/meta"
)

type ImageTaskResult struct {
	Metadata model.ImageResultMetadata
	Status   string
	Data     []model.ImageOutput
	Error    *model.ImageTaskError
}

type ImageTaskExecutor interface{ ImageAdapterName() string }

// SyncImageTaskAdapter runs once after durable reservation. Only the saved result
// enters the polling/settlement worker; this interface has no resubmission method.
type SyncImageTaskAdapter interface {
	ImageTaskExecutor
	GenerateImage(ctx context.Context, m *meta.Meta, body, frozen []byte) (ImageTaskResult, error)
}

// ImageTaskAdapter normalizes provider queue protocols; the durable worker is model agnostic.
type ImageTaskAdapter interface {
	ImageAdapterName() string
	SubmitImage(ctx context.Context, m *meta.Meta, body []byte) (string, error)
	PollImage(
		ctx context.Context,
		channel *model.Channel,
		info *model.AsyncUsageInfo,
		task *model.ImageTask,
	) (ImageTaskResult, error)
}

// ImageTaskCanceller is optional on an ImageTaskAdapter whose provider queue
// can stop an accepted request (fal). The worker calls it once, best effort,
// after a task outlived model.AsyncGenerationDeadline and was failed and
// refunded; the outcome is only logged. It receives the same channel and
// AsyncUsageInfo base URL that passed the poll's identity checks, so it never
// uses another channel's credential. It is not a public cancel API.
type ImageTaskCanceller interface {
	CancelImage(
		ctx context.Context,
		channel *model.Channel,
		info *model.AsyncUsageInfo,
		task *model.ImageTask,
	) (string, error)
}

var ErrImageSubmissionRejected = &ImageSubmissionFailure{Failure: failover.Failure{Acceptance: failover.NotAccepted, Class: failover.InvalidRequest, Evidence: "invalid_submission"}}

// MapImageProviderInput executes the server-owned contract, independent of model names.
func MapImageProviderInput(
	config map[model.ModelConfigKey]any,
	adapterName string,
	body []byte,
) ([]byte, error) {
	var wrapper struct {
		Contract struct {
			Providers map[string]struct {
				Adapter     string            `json:"adapter"`
				Mapping     map[string]string `json:"parameterMapping"`
				Fixed       map[string]any    `json:"fixedParameters"`
				Passthrough []string          `json:"allowedPassthroughParameters"`
			} `json:"providers"`
		} `json:"contract"`
	}

	raw, err := json.Marshal(config["x_token_platform_capability_contract"])
	if err != nil {
		return nil, err
	}

	if err = json.Unmarshal(raw, &wrapper); err != nil {
		return nil, err
	}

	var input map[string]any
	if err = json.Unmarshal(body, &input); err != nil {
		return nil, err
	}

	delete(input, "model")

	var result []byte
	for _, provider := range wrapper.Contract.Providers {
		if provider.Adapter != adapterName {
			continue
		}

		mapped := map[string]any{}
		for key, value := range input {
			if fixed, ok := provider.Fixed[key]; ok {
				if !reflect.DeepEqual(value, fixed) {
					return nil, errors.New("fixed provider parameter override")
				}
				continue
			}

			target, ok := provider.Mapping[key]
			if !ok {
				if !slices.Contains(provider.Passthrough, key) {
					return nil, fmt.Errorf("unmapped image parameter %s", key)
				}

				target = key
			}

			if target == "" || target == "model" {
				return nil, errors.New("invalid image parameter mapping")
			}

			if _, exists := mapped[target]; exists {
				return nil, errors.New("colliding image parameter mapping")
			}

			mapped[target] = value
		}

		for key, value := range provider.Fixed {
			if supplied, ok := mapped[key]; ok && !reflect.DeepEqual(value, supplied) {
				return nil, errors.New("fixed provider parameter collision")
			}

			mapped[key] = value
		}

		encoded, err := json.Marshal(mapped)
		if err != nil {
			return nil, err
		}

		if result != nil && !bytes.Equal(result, encoded) {
			return nil, errors.New("ambiguous image provider mapping")
		}

		result = encoded
	}

	if result == nil {
		return nil, errors.New("image provider contract unavailable")
	}

	return result, nil
}
