package controller

import (
	"encoding/json"
	"github.com/labring/aiproxy/core/common/imagecapabilities"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/labring/aiproxy/core/relay/adaptors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
)

const providerBindingsConfig = "x_token_platform_provider_bindings"

func imageProviderContract(c *gin.Context) []byte {
	encoded, err := json.Marshal(
		middleware.GetModelConfig(c).Config["x_token_platform_capability_contract"],
	)
	if err != nil {
		return nil
	}

	var wrapper struct {
		Contract json.RawMessage `json:"contract"`
	}
	if json.Unmarshal(encoded, &wrapper) != nil {
		return nil
	}

	return wrapper.Contract
}

func channelProviderBinding(
	ch *model.Channel,
	route string,
) (registryvalidation.ProviderBinding, error) {
	raw, err := json.Marshal(ch.Configs[providerBindingsConfig])
	if err != nil {
		return registryvalidation.ProviderBinding{}, err
	}

	var bindings map[string]registryvalidation.ProviderBinding
	if json.Unmarshal(raw, &bindings) != nil {
		return registryvalidation.ProviderBinding{}, registryvalidation.ErrProviderContract
	}

	b, ok := bindings[route]
	if !ok {
		return b, registryvalidation.ErrProviderContract
	}

	return b, nil
}

func mapChannelProviderInput(
	raw []byte,
	ch *model.Channel,
	route string,
	body []byte,
) ([]byte, registryvalidation.ProviderBinding, error) {
	b, err := channelProviderBinding(ch, route)
	if err != nil {
		return nil, b, err
	}

	rawAdapter, exists := adaptors.GetAdaptor(ch.Type)
	executor, supported := rawAdapter.(adaptor.ImageTaskExecutor)
	if !exists || !supported {
		return nil, b, registryvalidation.ErrProviderContract
	}
	capability, supported := imagecapabilities.Lookup(executor.ImageAdapterName())
	if !supported {
		return nil, b, registryvalidation.ErrProviderContract
	}
	adapter, execution := executor.ImageAdapterName(), capability.Execution

	endpoint, _ := meta.GetMappedModelName(route, ch.ModelMapping)
	mapped, err := registryvalidation.MapBoundProviderInput(
		raw,
		b,
		adapter,
		endpoint,
		execution,
		body,
	)

	return mapped, b, err
}

// The predicate applies before health, preference and retry selection, including explicit channel selection.
func imageProviderPredicate(c *gin.Context, route string, m mode.Mode) func(*model.Channel) bool {
	if m != mode.ImagesGenerations && m != mode.ImagesEdits {
		return nil
	}

	raw := imageProviderContract(c)
	if !registryvalidation.HasProviderContracts(raw) {
		return nil
	}
	// Both queue and synchronous upstreams require durable public task reservation.
	if c.Request.URL.Path != "/v1/images/tasks" || c.Request.Method != http.MethodPost {
		common.GetLogger(c).Warn("image channel rejected: task endpoint required")
		return func(*model.Channel) bool { return false }
	}

	body, err := common.GetRequestBodyReusable(c.Request)
	if err != nil {
		return func(*model.Channel) bool { return false }
	}

	return func(ch *model.Channel) bool {
		_, _, err := mapChannelProviderInput(raw, ch, route, body)
		if err != nil {
			common.GetLogger(c).Warnf("image channel compatibility rejected: channel=%d route=%s", ch.ID, route)
		}
		return err == nil
	}
}
