package model

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const (
	ModelCapabilityContractVersion = 1
	modelCapabilityKeySeparator    = "::"
)

type ModelCapability string

const (
	ModelCapabilityTextToVideo  ModelCapability = "text-to-video"
	ModelCapabilityImageToVideo ModelCapability = "image-to-video"
)

func (c ModelCapability) Valid() bool {
	switch c {
	case ModelCapabilityTextToVideo, ModelCapabilityImageToVideo:
		return true
	default:
		return false
	}
}

func BuildModelCapabilityKey(modelName string, capability ModelCapability) (string, error) {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" || strings.Contains(modelName, modelCapabilityKeySeparator) {
		return "", fmt.Errorf("invalid public model %q", modelName)
	}
	if !capability.Valid() || strings.Contains(string(capability), modelCapabilityKeySeparator) {
		return "", fmt.Errorf("unsupported model capability %q", capability)
	}

	return modelName + modelCapabilityKeySeparator + string(capability), nil
}

func ParseModelCapabilityKey(key string) (string, ModelCapability, bool) {
	modelName, rawCapability, ok := strings.Cut(key, modelCapabilityKeySeparator)
	if !ok || strings.Contains(rawCapability, modelCapabilityKeySeparator) {
		return "", "", false
	}

	capability := ModelCapability(rawCapability)
	if strings.TrimSpace(modelName) == "" || !capability.Valid() {
		return "", "", false
	}

	return modelName, capability, true
}

func ValidateModelCapabilityConfig(
	config map[ModelConfigKey]any,
	publicModel string,
	capability ModelCapability,
) error {
	version, ok := modelCapabilityContractVersion(
		config[ModelConfigKey("capability_contract_version")],
	)
	if !ok || version != ModelCapabilityContractVersion {
		return fmt.Errorf("unsupported model capability contract version")
	}

	configuredModel, ok := config[ModelConfigKey("public_model")].(string)
	if !ok || configuredModel != publicModel {
		return fmt.Errorf("model capability public model does not match route key")
	}

	configuredCapability, ok := config[ModelConfigKey("capability")].(string)
	if !ok || ModelCapability(configuredCapability) != capability || !capability.Valid() {
		return fmt.Errorf("model capability does not match route key")
	}

	return nil
}

func modelCapabilityContractVersion(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), int64(int(typed)) == typed
	case float64:
		return int(typed), float64(int(typed)) == typed
	case json.Number:
		parsed, err := strconv.Atoi(typed.String())
		return parsed, err == nil
	default:
		return 0, false
	}
}
