package model

import (
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"

	"github.com/bytedance/sonic"
)

type CapabilityRoutingErrorCode string

const (
	CapabilityModelNotFound     CapabilityRoutingErrorCode = "model_not_found"
	CapabilityNotMatched        CapabilityRoutingErrorCode = "capability_not_matched"
	CapabilityAmbiguous         CapabilityRoutingErrorCode = "capability_ambiguous"
	CapabilityParameterMismatch CapabilityRoutingErrorCode = "capability_parameter_mismatch"
)

type CapabilityRoutingError struct {
	Code    CapabilityRoutingErrorCode
	Message string
}

func (e *CapabilityRoutingError) Error() string {
	return e.Message
}

type capabilityParameterSchema struct {
	Type       string                               `json:"type"`
	Enum       []any                                `json:"enum,omitempty"`
	AnyOf      []capabilityParameterSchema          `json:"anyOf,omitempty"`
	Properties map[string]capabilityParameterSchema `json:"properties,omitempty"`
	Required   []string                             `json:"required,omitempty"`
	Items      *capabilityParameterSchema           `json:"items,omitempty"`
}

type capabilityObjectSchema struct {
	Type                 string                               `json:"type"`
	Properties           map[string]capabilityParameterSchema `json:"properties"`
	Required             []string                             `json:"required"`
	AdditionalProperties bool                                 `json:"additionalProperties"`
}

type CapabilityRoutingMetadata struct {
	ContractVersion       int
	PublicModel           string
	PublicCapabilityModel string
	Capability            string
	ParameterSchema       capabilityObjectSchema
	DefaultParameters     map[string]any
}

type CapabilityResolution struct {
	RequestedModel        string
	PublicModel           string
	PublicCapabilityModel string
	Capability            string
	InternalModel         string
}

func modelConfigString(config map[ModelConfigKey]any, key ModelConfigKey) (string, bool) {
	value, ok := config[key].(string)
	value = strings.TrimSpace(value)
	return value, ok && value != ""
}

func modelConfigPositiveInt(config map[ModelConfigKey]any, key ModelConfigKey) (int, bool) {
	value, ok := config[key]
	if !ok {
		return 0, false
	}

	switch typed := value.(type) {
	case int:
		return typed, typed > 0
	case int64:
		return int(typed), typed > 0 && typed <= math.MaxInt
	case float64:
		return int(typed), typed > 0 && typed <= math.MaxInt && math.Trunc(typed) == typed
	default:
		return 0, false
	}
}

func decodeCapabilityConfigValue[T any](value any) (T, bool) {
	var result T

	raw, err := sonic.Marshal(value)
	if err != nil {
		return result, false
	}

	if err := sonic.Unmarshal(raw, &result); err != nil {
		return result, false
	}

	return result, true
}

func CapabilityRoutingMetadataFromConfig(config ModelConfig) (CapabilityRoutingMetadata, bool) {
	version, ok := modelConfigPositiveInt(config.Config, ModelConfigCapabilityContractVersionKey)
	if !ok {
		return CapabilityRoutingMetadata{}, false
	}

	publicModel, ok := modelConfigString(config.Config, ModelConfigPublicModelKey)
	if !ok {
		return CapabilityRoutingMetadata{}, false
	}

	publicCapabilityModel, ok := modelConfigString(
		config.Config,
		ModelConfigPublicCapabilityModelKey,
	)
	if !ok {
		return CapabilityRoutingMetadata{}, false
	}

	capability, ok := modelConfigString(config.Config, ModelConfigCapabilityKey)
	if !ok {
		return CapabilityRoutingMetadata{}, false
	}

	schema, ok := decodeCapabilityConfigValue[capabilityObjectSchema](
		config.Config[ModelConfigParameterSchemaKey],
	)
	if !ok || schema.Type != "object" || schema.Properties == nil {
		return CapabilityRoutingMetadata{}, false
	}

	defaults, ok := decodeCapabilityConfigValue[map[string]any](
		config.Config[ModelConfigDefaultParametersKey],
	)
	if !ok {
		return CapabilityRoutingMetadata{}, false
	}

	if publicCapabilityModel != publicModel+"/"+capability {
		return CapabilityRoutingMetadata{}, false
	}

	return CapabilityRoutingMetadata{
		ContractVersion:       version,
		PublicModel:           publicModel,
		PublicCapabilityModel: publicCapabilityModel,
		Capability:            capability,
		ParameterSchema:       schema,
		DefaultParameters:     defaults,
	}, true
}

func capabilityPrimitiveMatches(value any, expected string) bool {
	if value == nil {
		return false
	}

	switch expected {
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "integer":
		switch typed := value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return true
		case float64:
			return math.Trunc(typed) == typed
		case float32:
			return float32(math.Trunc(float64(typed))) == typed
		default:
			return false
		}
	case "number":
		switch value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64,
			float32, float64:
			return true
		default:
			return false
		}
	case "null":
		return value == nil
	default:
		return false
	}
}

func capabilityEnumMatches(value any, allowed []any) bool {
	if len(allowed) == 0 {
		return true
	}

	for _, candidate := range allowed {
		if reflect.DeepEqual(candidate, value) {
			return true
		}

		candidateNumber, candidateIsNumber := candidate.(float64)

		valueNumber, valueIsNumber := value.(float64)
		if candidateIsNumber && valueIsNumber && candidateNumber == valueNumber {
			return true
		}
	}

	return false
}

func capabilityParameterMatches(value any, schema capabilityParameterSchema) bool {
	if len(schema.AnyOf) != 0 {
		for _, candidate := range schema.AnyOf {
			if capabilityParameterMatches(value, candidate) {
				return true
			}
		}

		return false
	}

	if !capabilityEnumMatches(value, schema.Enum) {
		return false
	}

	switch schema.Type {
	case "object":
		fields, ok := value.(map[string]any)
		if !ok {
			return false
		}

		for _, required := range schema.Required {
			child, described := schema.Properties[required]

			field, exists := fields[required]
			if !exists || !described || !capabilityParameterMatches(field, child) {
				return false
			}
		}

		for name, field := range fields {
			child, described := schema.Properties[name]
			if described && !capabilityParameterMatches(field, child) {
				return false
			}
		}

		return true
	case "array":
		items, ok := value.([]any)
		if !ok || schema.Items == nil {
			return false
		}

		for _, item := range items {
			if !capabilityParameterMatches(item, *schema.Items) {
				return false
			}
		}

		return true
	default:
		return capabilityPrimitiveMatches(value, schema.Type)
	}
}

func capabilitySchemaMatches(schema capabilityObjectSchema, fields map[string]any) bool {
	for _, required := range schema.Required {
		value, exists := fields[required]

		parameter, described := schema.Properties[required]
		if !exists || !described || !capabilityParameterMatches(value, parameter) {
			return false
		}
	}

	for name, parameter := range schema.Properties {
		value, exists := fields[name]
		if !exists {
			continue
		}

		if !capabilityParameterMatches(value, parameter) {
			return false
		}
	}

	return true
}

type capabilityCandidate struct {
	config   ModelConfig
	metadata CapabilityRoutingMetadata
}

func capabilityCandidates(configs []ModelConfig) []capabilityCandidate {
	candidates := make([]capabilityCandidate, 0, len(configs))
	for _, config := range configs {
		metadata, ok := CapabilityRoutingMetadataFromConfig(config)
		if !ok {
			continue
		}

		candidates = append(candidates, capabilityCandidate{config: config, metadata: metadata})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].metadata.PublicCapabilityModel < candidates[j].metadata.PublicCapabilityModel
	})

	return candidates
}

func resolutionFromCandidate(requested string, candidate capabilityCandidate) CapabilityResolution {
	return CapabilityResolution{
		RequestedModel:        requested,
		PublicModel:           candidate.metadata.PublicModel,
		PublicCapabilityModel: candidate.metadata.PublicCapabilityModel,
		Capability:            candidate.metadata.Capability,
		InternalModel:         candidate.config.Model,
	}
}

func ResolveCapabilityModel(
	requested string,
	fields map[string]any,
	configs []ModelConfig,
) (CapabilityResolution, error) {
	requested = strings.TrimSpace(requested)

	candidates := capabilityCandidates(configs)
	for _, candidate := range candidates {
		if candidate.metadata.PublicCapabilityModel != requested {
			continue
		}

		if !capabilitySchemaMatches(candidate.metadata.ParameterSchema, fields) {
			return CapabilityResolution{}, &CapabilityRoutingError{
				Code:    CapabilityParameterMismatch,
				Message: "request parameters do not match the selected capability",
			}
		}

		return resolutionFromCandidate(requested, candidate), nil
	}

	baseCandidates := make([]capabilityCandidate, 0)
	for _, candidate := range candidates {
		if candidate.metadata.PublicModel == requested {
			baseCandidates = append(baseCandidates, candidate)
		}
	}

	if len(baseCandidates) == 0 {
		return CapabilityResolution{}, &CapabilityRoutingError{
			Code:    CapabilityModelNotFound,
			Message: fmt.Sprintf("the model %q does not exist or is not available", requested),
		}
	}

	matched := make([]capabilityCandidate, 0, len(baseCandidates))

	bestSpecificity := -1
	for _, candidate := range baseCandidates {
		if !capabilitySchemaMatches(candidate.metadata.ParameterSchema, fields) {
			continue
		}

		specificity := len(candidate.metadata.ParameterSchema.Required)
		switch {
		case specificity > bestSpecificity:
			matched = append(matched[:0], candidate)
			bestSpecificity = specificity
		case specificity == bestSpecificity:
			matched = append(matched, candidate)
		}
	}

	if len(matched) == 0 {
		return CapabilityResolution{}, &CapabilityRoutingError{
			Code:    CapabilityNotMatched,
			Message: "request parameters do not match any available capability",
		}
	}

	if len(matched) > 1 {
		return CapabilityResolution{}, &CapabilityRoutingError{
			Code:    CapabilityAmbiguous,
			Message: "request matches multiple capabilities; use a full capability model id",
		}
	}

	return resolutionFromCandidate(requested, matched[0]), nil
}
