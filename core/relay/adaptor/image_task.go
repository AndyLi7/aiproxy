package adaptor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/meta"
)

type ImageTaskResult struct {
	Status string
	Data   []model.ImageOutput
	Error  *model.ImageTaskError
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

var ErrImageSubmissionRejected = errors.New("upstream rejected image submission")

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
