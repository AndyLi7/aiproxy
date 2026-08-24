package openai

import (
	"time"

	"github.com/labring/aiproxy/core/relay/meta"
)

const defaultResponsesFirstEventTimeoutSeconds uint32 = 2

type Config struct {
	MapReasoningToReasoningContent bool   `json:"map_reasoning_to_reasoning_content"`
	ResponsesFirstEventTimeout     uint32 `json:"responses_first_event_timeout"`
}

// DoResponseOptions contains the response handling options needed by the OpenAI Responses path.
type DoResponseOptions struct {
	ResponsesFirstEventTimeout time.Duration
}

func defaultConfig() Config {
	return Config{
		ResponsesFirstEventTimeout: defaultResponsesFirstEventTimeoutSeconds,
	}
}

// ConfigSchema returns the channel configuration schema shared by OpenAI-compatible adaptors.
func ConfigSchema() map[string]any {
	return configSchema()
}

func (c Config) responsesFirstEventTimeout() time.Duration {
	return time.Duration(c.ResponsesFirstEventTimeout) * time.Second
}

func (c Config) doResponseOptions() DoResponseOptions {
	return DoResponseOptions{
		ResponsesFirstEventTimeout: c.responsesFirstEventTimeout(),
	}
}

// LoadDoResponseOptions loads OpenAI response handling options from channel config.
func LoadDoResponseOptions(meta *meta.Meta) (DoResponseOptions, error) {
	cfg := defaultConfig()
	if meta == nil {
		return cfg.doResponseOptions(), nil
	}

	if err := meta.ChannelConfigs.LoadConfig(&cfg); err != nil {
		return DoResponseOptions{}, err
	}

	return cfg.doResponseOptions(), nil
}

func (a *Adaptor) loadConfig(meta *meta.Meta) (Config, error) {
	cfg := defaultConfig()
	return a.configCache.Load(meta, cfg)
}

func configSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"responses_first_event_timeout": map[string]any{
				"type":        "integer",
				"title":       "Responses first event timeout",
				"description": "Maximum seconds to buffer initial Responses API lifecycle events while waiting for the first output or error event. Increase this value to allow late upstream errors to trigger channel retries.",
				"default":     defaultResponsesFirstEventTimeoutSeconds,
				"minimum":     0,
			},
			"map_reasoning_to_reasoning_content": map[string]any{
				"type":        "boolean",
				"title":       "Map reasoning To reasoning_content",
				"description": "Rewrite upstream chat completion `reasoning` fields to `reasoning_content` in both streaming and non-streaming responses.",
			},
		},
	}
}

func getChatCompletionResponsePreHandlers(
	meta *meta.Meta,
) (streamPreHandler, handlerPreHandler PreHandler, err error) {
	return (&Adaptor{}).getChatCompletionResponsePreHandlers(meta)
}

func (a *Adaptor) getChatCompletionResponsePreHandlers(
	meta *meta.Meta,
) (streamPreHandler, handlerPreHandler PreHandler, err error) {
	cfg, err := a.loadConfig(meta)
	if err != nil {
		return nil, nil, err
	}

	if !cfg.MapReasoningToReasoningContent {
		return nil, nil, nil
	}

	return StreamReasoningToReasoningContentPreHandler,
		ReasoningToReasoningContentPreHandler,
		nil
}
