package model

import "github.com/labring/aiproxy/core/relay/mode"

// ResolveNativeCapabilityRoute maps a native capability's public id (for example
// vendor/model/text-to-image) to the internal route key of the one enabled native
// model config that published it. Unknown or ambiguous ids are returned unchanged.
func ResolveNativeCapabilityRoute(publicID string, configs map[string]ModelConfig) string {
	route := ""

	for name, config := range configs {
		if config.Type != mode.NativeTasks {
			continue
		}

		published, _ := config.Config[ModelConfigPublicCapabilityModelKey].(string)
		if published != publicID {
			continue
		}

		if route != "" {
			return publicID
		}

		route = name
	}

	if route == "" {
		return publicID
	}

	return route
}
