package model

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	"github.com/labring/aiproxy/core/common/imagecompat"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/relay/mode"
)

type OpenAIImageProfile struct {
	Version        int    `json:"version"`
	Enabled        bool   `json:"enabled"`
	ContractSHA256 string `json:"contractSha256"`
	InlineEdit     bool   `json:"inlineEdit"`
}

func ValidOpenAIImageProfile(mc ModelConfig) bool {
	raw, err := json.Marshal(mc.Config[OpenAIImageProfileKey])
	if err != nil {
		return false
	}

	var p OpenAIImageProfile

	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()

	if d.Decode(&p) != nil || p.Version != 1 || !p.Enabled || mc.Type != mode.NativeTasks {
		return false
	}

	identity, ok := PublicCapabilityIdentityFromConfig(mc)
	if !ok ||
		(identity.CapabilityModel != imagecompat.GenerateModel && identity.CapabilityModel != imagecompat.EditModel) {
		return false
	}

	if identity.CapabilityModel == imagecompat.EditModel && !p.InlineEdit {
		return false
	}

	binding, ok := ParseNativeRouteBinding(mc)
	if !ok {
		return false
	}

	sum := sha256.Sum256(binding.Contract)
	if p.ContractSHA256 != hex.EncodeToString(sum[:]) {
		return false
	}

	var contract nativeresult.TaskContract
	if json.Unmarshal(binding.Contract, &contract) != nil ||
		contract.Model != identity.CapabilityModel ||
		len(contract.Artifacts) != 1 ||
		!slices.Equal(contract.Artifacts[0].Path, []string{"images", "*", "url"}) {
		return false
	}

	_, err = nativeresult.CompileTaskContract(binding.Contract)

	return err == nil
}

// Check every accepted spelling against every enabled route, not just routes
// this token can call. An activation must never capture an older image route.
func SelectOpenAIImageProfile(
	configs map[string]ModelConfig,
	requested string,
) (ModelConfig, bool) {
	for _, candidate := range configs {
		identity, ok := PublicCapabilityIdentityFromConfig(candidate)
		if !ok || !identity.Accepts(requested) || !ValidOpenAIImageProfile(candidate) {
			continue
		}

		if OpenAIImageIdentityConflict(configs, candidate) {
			return ModelConfig{}, false
		}

		return candidate, true
	}

	return ModelConfig{}, false
}

// OpenAIImageIdentityConflict checks every spelling against the complete final
// namespace, including legacy public names and disabled route configurations.
func OpenAIImageIdentityConflict(configs map[string]ModelConfig, candidate ModelConfig) bool {
	identity, ok := PublicCapabilityIdentityFromConfig(candidate)
	if !ok {
		return true
	}

	for _, other := range configs {
		if other.Model == candidate.Model {
			continue
		}

		for _, accepted := range identity.Accepted() {
			for _, name := range imageConfigNames(other) {
				if strings.EqualFold(accepted, name) {
					return true
				}
			}
		}
	}

	return false
}

func imageConfigNames(mc ModelConfig) []string {
	names := []string{mc.Model}
	if identity, ok := PublicCapabilityIdentityFromConfig(mc); ok {
		names = append(names, identity.Accepted()...)
	}

	if mc.Type != mode.NativeTasks {
		if public, ok := mc.Config[ModelConfigPublicModelKey].(string); ok {
			names = append(names, public)
		}
	}

	return names
}
