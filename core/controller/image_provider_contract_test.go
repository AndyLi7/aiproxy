package controller

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

func TestVersionedImageForcedChannelCannotBypassCompatibility(t *testing.T) {
	raw, err := os.ReadFile("../common/registryvalidation/testdata/provider.json")
	require.NoError(t, err)
	var contract any
	require.NoError(t, json.Unmarshal(raw, &contract))
	binding := map[string]any{"provider": "small", "id": "small", "revision": "1", "contractHash": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	channel := &model.Channel{ID: 42, Status: model.ChannelStatusEnabled, Type: model.ChannelTypeFal, Models: []string{"image"}, ModelMapping: map[string]string{"image": "fal-ai/test"}, Configs: map[string]any{providerBindingsConfig: map[string]any{"image": binding}}}
	for _, test := range []struct {
		name, body, path string
		pinned, fail     bool
	}{
		{"allowed header", `{"model":"image","prompt":"hi","n":1}`, "/v1/images/tasks", false, false},
		{"incompatible header", `{"model":"image","prompt":"hi","n":2}`, "/v1/images/tasks", false, true},
		{"allowed pinned", `{"model":"image","prompt":"hi","n":1}`, "/v1/images/tasks", true, false},
		{"incompatible pinned", `{"model":"image","prompt":"hi","n":2}`, "/v1/images/tasks", true, true},
		{"no implicit sync bridge", `{"model":"image","prompt":"hi","n":1}`, "/v1/images/generations", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", test.path, bytes.NewBufferString(test.body))
			c.Set(middleware.Group, model.GroupCache{Status: model.GroupStatusInternal})
			c.Set(middleware.ModelConfig, model.ModelConfig{Config: map[model.ModelConfigKey]any{"x_token_platform_capability_contract": map[string]any{"contract": contract}}})
			c.Set(middleware.ModelCaches, &model.ModelCaches{ChannelsByID: map[int]*model.Channel{42: channel}, EnabledModel2ChannelsBySet: map[string]map[string][]*model.Channel{"default": {"image": {channel}}}})
			if test.pinned {
				c.Set(middleware.ChannelID, 42)
			} else {
				c.Request.Header.Set(AIProxyChannelHeader, "42")
			}
			got, err := getInitialChannel(c, "image", mode.ImagesGenerations)
			if test.fail {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, 42, got.channel.ID)
			}
		})
	}
}
