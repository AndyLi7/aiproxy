package model

import (
	"encoding/json"
	"errors"
	"gorm.io/gorm"
	"slices"
	"strings"
	"time"
)

const AgentExecutionKey ModelConfigKey = "x_token_platform_agent_execution_v1"
const AgentVisibilityKey ModelConfigKey = "x_token_platform_agent_visibility_v1"
const AgentInferenceSetsKey ModelConfigKey = "x_token_platform_agent_inference_sets_v1"

// Legacy descriptors were private. Opt-in of an existing public model explicitly
// preserves its public API. Merely carrying an Agent mapping is not visibility.
func IsAgentOnlyModel(c ModelConfig) bool {
	return c.Config[AgentExecutionKey] != nil && c.Config[AgentVisibilityKey] != "shared"
}
func addPrivateAgentInferenceModels(configs []ModelConfig, bySet map[string][]string, configBySet map[string][]ModelConfig, all map[string]ModelConfig) {
	for _, c := range configs {
		if !strings.HasPrefix(c.Model, "agent-inference/") || !IsAgentOnlyModel(c) {
			continue
		}
		raw, _ := json.Marshal(c.Config[AgentInferenceSetsKey])
		var sets []string
		if json.Unmarshal(raw, &sets) != nil || len(sets) == 0 {
			continue
		}
		for _, set := range sets {
			if !slices.Contains(bySet[set], c.Model) {
				bySet[set] = append(bySet[set], c.Model)
				configBySet[set] = append(configBySet[set], c)
			}
		}
		all[c.Model] = c
	}
}

// Create-only inference products cannot overwrite an existing API model.
func CreateAgentInferenceProduct(c ModelConfig) (err error) {
	defer func() {
		if err == nil {
			rebuildModelCachesAfterWrite()
		}
	}()
	return withMigrationConfigWrite([]ModelConfig{c}, nil, func(tx *gorm.DB) error {
		var existing ModelConfig
		err := tx.Where("model = ?", c.Model).First(&existing).Error
		if err == nil {
			a, _ := json.Marshal(c.Config)
			b, _ := json.Marshal(existing.Config)
			pa, _ := json.Marshal(c.Price)
			pb, _ := json.Marshal(existing.Price)
			if string(a) == string(b) && string(pa) == string(pb) && existing.Owner == c.Owner && existing.Type == c.Type {
				return nil
			}
			return errors.New("Agent model already exists")
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return tx.Create(&c).Error
	})
}

// Optimistic whole-row comparison prevents an Agent opt-in from overwriting a
// concurrently changed native route, quote or public configuration.
func AttachAgentMapping(expected ModelConfig, descriptor any) (err error) {
	defer func() {
		if err == nil {
			rebuildModelCachesAfterWrite()
		}
	}()
	return withMigrationConfigWrite([]ModelConfig{expected}, nil, func(tx *gorm.DB) error {
		var current ModelConfig
		if err := tx.Where("model = ?", expected.Model).First(&current).Error; err != nil {
			return err
		}
		timestamp := current.UpdatedAt
		comparable := current
		comparable.CreatedAt, comparable.UpdatedAt = time.Time{}, time.Time{}
		expected.CreatedAt, expected.UpdatedAt = time.Time{}, time.Time{}
		before, _ := json.Marshal(expected)
		actual, _ := json.Marshal(comparable)
		if string(before) != string(actual) {
			return errors.New("model changed; refresh selection")
		}
		if current.Config == nil {
			current.Config = map[ModelConfigKey]any{}
		}
		if current.Config[AgentExecutionKey] == nil {
			current.Config[AgentVisibilityKey] = "shared"
		}
		current.Config[AgentExecutionKey] = descriptor
		result := tx.Model(&current).Where("updated_at = ?", timestamp).Select("Config", "UpdatedAt").Updates(&current)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("model changed; refresh selection")
		}
		return nil
	})
}
