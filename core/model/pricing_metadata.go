package model

import "strings"

const retailPricingMetadataConfigKey ModelConfigKey = "x_token_platform_pricing"

// RetailPricingMetadata returns the immutable currency and release version
// published with a model config. The metadata lives under Config so older
// AIProxy versions preserve it even when their Price struct has no such fields.
func (c ModelConfig) RetailPricingMetadata() (currency, version string, ok bool) {
	raw, exists := c.Config[retailPricingMetadataConfigKey]
	if !exists {
		return "", "", false
	}

	var currencyValue, versionValue any
	switch metadata := raw.(type) {
	case map[string]any:
		currencyValue = metadata["currency"]
		versionValue = metadata["pricing_version"]
	case map[ModelConfigKey]any:
		currencyValue = metadata[ModelConfigKey("currency")]
		versionValue = metadata[ModelConfigKey("pricing_version")]
	default:
		return "", "", false
	}

	currency, currencyOK := currencyValue.(string)
	version, versionOK := versionValue.(string)
	currency = strings.ToUpper(strings.TrimSpace(currency))
	version = strings.TrimSpace(version)
	if !currencyOK || !versionOK || currency == "" || version == "" {
		return "", "", false
	}

	return currency, version, true
}
