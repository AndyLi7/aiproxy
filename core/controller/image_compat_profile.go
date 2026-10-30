package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	"github.com/labring/aiproxy/core/common/imagecompat"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
)

const ImageCompatConfigKey model.ModelConfigKey = "x_token_platform_openai_images_v1"

// Opt-in evidence, not another route or quote. Nothing installs this profile by
// default. Native publication and pricing remain the authoritative binding.
type imageCompatProfile struct {
	Version        int    `json:"version"`
	Enabled        bool   `json:"enabled"`
	ContractSHA256 string `json:"contractSha256"`
	InlineEdit     bool   `json:"inlineEdit"`
}

func validImageCompatProfile(mc model.ModelConfig) bool {
	raw, err := json.Marshal(mc.Config[ImageCompatConfigKey])
	if err != nil {
		return false
	}

	var p imageCompatProfile

	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()

	if d.Decode(&p) != nil || p.Version != 1 || !p.Enabled || mc.Type != mode.NativeTasks {
		return false
	}

	identity, ok := model.PublicCapabilityIdentityFromConfig(mc)
	if !ok ||
		(identity.CapabilityModel != imagecompat.GenerateModel && identity.CapabilityModel != imagecompat.EditModel) {
		return false
	}

	if identity.CapabilityModel == imagecompat.EditModel && !p.InlineEdit {
		return false
	}

	binding, ok := parseNativeRouteBinding(mc)
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
func selectImageCompatProfile(
	configs map[string]model.ModelConfig,
	requested string,
) (model.ModelConfig, bool) {
	for _, candidate := range configs {
		identity, ok := model.PublicCapabilityIdentityFromConfig(candidate)
		if !ok || !identity.Accepts(requested) || !validImageCompatProfile(candidate) {
			continue
		}

		for _, other := range configs {
			if other.Model == candidate.Model {
				continue
			}

			otherID, hasID := model.PublicCapabilityIdentityFromConfig(other)
			for _, accepted := range identity.Accepted() {
				if strings.EqualFold(accepted, other.Model) ||
					(hasID && (otherID.Accepts(accepted) || (other.Type != mode.NativeTasks && strings.EqualFold(otherID.PublicModel, accepted)))) {
					return model.ModelConfig{}, false
				}
			}
		}

		return candidate, true
	}

	return model.ModelConfig{}, false
}
