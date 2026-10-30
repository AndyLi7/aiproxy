package model

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/labring/aiproxy/core/common/imagecompat"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/relay/mode"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// This internal option is deliberately absent from optionMap/default initialization
// and YAML overrides. Only validated configuration transactions maintain this
// append-only evidence. Generic option writes must never modify it.
const (
	OpenAIImageIdentitiesOption                = "OpenAIImageIdentities"
	OpenAIImageProfileKey       ModelConfigKey = "x_token_platform_openai_images_v1"
)

type ImageIdentityState string

const (
	ImageIdentityUnloaded    ImageIdentityState = ""
	ImageIdentityMissing     ImageIdentityState = "missing"
	ImageIdentityUnavailable ImageIdentityState = "unavailable"
	ImageIdentityInvalid     ImageIdentityState = "invalid"
	ImageIdentityUnsupported ImageIdentityState = "unsupported"
	ImageIdentityReady       ImageIdentityState = "ready"
)

type imageIdentityDeclaration struct {
	Version      int                 `json:"version"`
	Capabilities map[string][]string `json:"capabilities"`
}

// OpenAIImageIdentitySnapshot is immutable once published with ModelCaches.
// Ownership is acquired only by a successful configuration activation transaction.
type OpenAIImageIdentitySnapshot struct {
	State    ImageIdentityState
	declared map[string]string
	trusted  bool
}

// ImageIdentityEvidence preserves the routing cache pointer on failed rebuilds.
// The separate fault flag closes only this new identity gate; it never acknowledges
// a routing migration or mutates the immutable declaration/maps.
func (c *ModelCaches) ImageIdentityEvidence() OpenAIImageIdentitySnapshot {
	if c == nil {
		return OpenAIImageIdentitySnapshot{}
	}

	evidence := c.OpenAIImageIdentities
	if c.imageIdentityReadFailed != nil && c.imageIdentityReadFailed.Load() {
		evidence.trusted = evidence.Trusted()
		evidence.State = ImageIdentityUnavailable
	}

	return evidence
}

// Ready permits configuration negotiation and new admissions. Missing is a
// verified virgin installation only after the coherent config snapshot check.
func (s OpenAIImageIdentitySnapshot) Ready() bool {
	return s.State == ImageIdentityReady || s.State == ImageIdentityMissing
}

// Trusted classification may survive an unavailable refresh, while new admission
// remains disabled. It is always paired with the last trusted routing snapshot.
func (s OpenAIImageIdentitySnapshot) Trusted() bool { return s.Ready() || s.trusted }

func (s OpenAIImageIdentitySnapshot) Known(id string) bool {
	return s.declared[id] != ""
}

func (s OpenAIImageIdentitySnapshot) Registered(mc ModelConfig) bool {
	identity, ok := openAIImageIdentity(mc)
	if !s.Ready() || !ok {
		return false
	}

	for _, id := range identity.Accepted() {
		if s.declared[id] != identity.CapabilityModel {
			return false
		}
	}

	return true
}

func isOpenAIImageCapability(id string) bool {
	return id == imagecompat.GenerateModel || id == imagecompat.EditModel
}

func openAIImageIdentity(mc ModelConfig) (PublicCapabilityIdentity, bool) {
	identity, ok := PublicCapabilityIdentityFromConfig(mc)
	return identity, ok && mc.Type == mode.NativeTasks &&
		isOpenAIImageCapability(identity.CapabilityModel)
}

var (
	imageIdentityValidator, _ = nativeresult.Compile(
		[]byte(
			`{"type":"object","required":["version","capabilities"],"additionalProperties":false,"properties":{"version":{"type":"integer"},"capabilities":{"type":"object","maxProperties":2,"additionalProperties":{"type":"array","maxItems":1024,"uniqueItems":true,"items":{"type":"string","maxLength":191}}}}}`,
		),
	)
	errImageIdentityWriteConflict = errors.New(
		"image identity declaration changed; retry configuration write",
	)
	errImageIdentityInvalid = errors.New(
		"invalid or unavailable OpenAI image identity declaration",
	)
)

func parseImageIdentityDeclaration(raw string) (imageIdentityDeclaration, ImageIdentityState) {
	var d imageIdentityDeclaration
	if len(raw) > 512<<10 {
		return d, ImageIdentityInvalid
	}

	if _, err := imageIdentityValidator.Validate([]byte(raw)); err != nil {
		return d, ImageIdentityInvalid
	}

	if json.Unmarshal([]byte(raw), &d) != nil {
		return d, ImageIdentityInvalid
	}

	if d.Version != 1 {
		return d, ImageIdentityUnsupported
	}

	owners := map[string]string{}
	for ck, ids := range d.Capabilities {
		if !isOpenAIImageCapability(ck) || !slices.Contains(ids, ck) {
			return d, ImageIdentityInvalid
		}

		for _, id := range ids {
			if !validPublicAlias(id) || (isOpenAIImageCapability(id) && id != ck) {
				return d, ImageIdentityInvalid
			}

			if owner := owners[id]; owner != "" && owner != ck {
				return d, ImageIdentityInvalid
			}

			owners[id] = ck
		}
	}

	return d, ImageIdentityReady
}

// ReadOpenAIImageIdentities reads configuration storage, never native task storage.
// A missing record is a normal virgin state; unreadable/invalid history is not empty.
func ReadOpenAIImageIdentities(db *gorm.DB) OpenAIImageIdentitySnapshot {
	s := OpenAIImageIdentitySnapshot{State: ImageIdentityUnavailable}
	if db == nil || !db.Migrator().HasTable(&Option{}) {
		return s
	}

	var row Option

	err := db.Where("key = ?", OpenAIImageIdentitiesOption).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		s.State = ImageIdentityMissing
		return s
	}

	if err != nil {
		return s
	}

	d, state := parseImageIdentityDeclaration(row.Value)

	s.State = state
	if state == ImageIdentityReady {
		s.declared = map[string]string{}
		for ck, ids := range d.Capabilities {
			for _, id := range ids {
				s.declared[id] = ck
			}
		}
	}

	return s
}

// A valid opt-in config without durable registration is not a supported virgin
// state (old adapter, partial restore or a bypass). Do not infer empty history.
func (s OpenAIImageIdentitySnapshot) withCurrent(
	configs []ModelConfig,
) OpenAIImageIdentitySnapshot {
	if s.Ready() {
		for _, mc := range configs {
			if ValidOpenAIImageProfile(mc) && !s.Registered(mc) {
				s.State = ImageIdentityInvalid
				break
			}
		}
	}

	return s
}

func (s OpenAIImageIdentitySnapshot) retains(previous OpenAIImageIdentitySnapshot) bool {
	for id, ck := range previous.declared {
		if s.declared[id] != ck {
			return false
		}
	}

	return true
}

// mergeImageIdentityDeclaration is a database compare-and-swap. It never changes
// memory, never removes an ID, and never assigns an ID to a different capability.
// Inside a model-config transaction, failure rolls back both config and evidence.
func mergeImageIdentityDeclaration(
	db *gorm.DB,
	addition imageIdentityDeclaration,
) error {
	if db == nil {
		return errImageIdentityInvalid
	}

	var row Option

	err := db.Where("key = ?", OpenAIImageIdentitiesOption).First(&row).Error
	if err != nil {
		return errImageIdentityInvalid
	}

	stored, state := parseImageIdentityDeclaration(row.Value)
	if state != ImageIdentityReady {
		return errImageIdentityInvalid
	}

	for ck, ids := range addition.Capabilities {
		for _, id := range ids {
			if !slices.Contains(stored.Capabilities[ck], id) {
				stored.Capabilities[ck] = append(stored.Capabilities[ck], id)
			}
		}

		slices.Sort(stored.Capabilities[ck])
	}

	raw, err := json.Marshal(stored)
	if err != nil {
		return err
	}

	if _, state := parseImageIdentityDeclaration(string(raw)); state != ImageIdentityReady {
		return errImageIdentityInvalid
	}

	if row.Value == string(raw) {
		return nil
	}

	result := db.Model(&Option{}).
		Where("key = ? AND value = ?", OpenAIImageIdentitiesOption, row.Value).
		Update("value", string(raw))
	if result.Error != nil {
		return result.Error
	}

	if result.RowsAffected != 1 {
		return errImageIdentityWriteConflict
	}

	return nil
}

// lockImageIdentityNamespace serializes both first claims and ordinary model
// writes. The no-op UPDATE is a row write lock on PostgreSQL/MySQL and obtains
// SQLite's writer lock. Read the final namespace only after obtaining this lock.
// The insertion itself is transactional; a failed candidate leaves no marker.
func lockImageIdentityNamespace(tx *gorm.DB) (bool, error) {
	if !tx.Migrator().HasTable(&Option{}) {
		return false, nil // older/minimal stores cannot activate this feature
	}

	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&Option{
		Key: OpenAIImageIdentitiesOption, Value: `{"version":1,"capabilities":{}}`,
	}).Error; err != nil {
		return false, err
	}

	result := tx.Model(&Option{}).Where("key = ?", OpenAIImageIdentitiesOption).
		UpdateColumn("value", gorm.Expr("value"))
	if result.Error != nil {
		return false, result.Error
	}

	return true, nil
}

// withImageIdentityConfigWrite runs inside the existing migration transaction.
// It validates the batch's final namespace, then atomically commits its configs
// and ownership. No controller/cache/network callbacks run inside this work.
func withImageIdentityConfigWrite(
	tx *gorm.DB,
	configs []ModelConfig,
	deleted []string,
	write func(*gorm.DB) error,
) error {
	locked, err := lockImageIdentityNamespace(tx)
	if err != nil {
		return err
	}

	if !locked {
		if slices.ContainsFunc(configs, ValidOpenAIImageProfile) {
			return errImageIdentityInvalid
		}

		if len(LoadModelCaches().OpenAIImageIdentities.declared) != 0 {
			return errImageIdentityInvalid
		}

		return write(tx)
	}

	evidence := ReadOpenAIImageIdentities(tx)
	if !evidence.Ready() || !evidence.retains(LoadModelCaches().OpenAIImageIdentities) {
		return errImageIdentityInvalid
	}

	var existing []ModelConfig
	if err := tx.Find(&existing).Error; err != nil {
		return err
	}

	if !evidence.withCurrent(existing).Ready() {
		return errImageIdentityInvalid
	}

	final := make(map[string]ModelConfig, len(existing)+len(configs))
	for _, mc := range existing {
		final[mc.Model] = mc
	}

	for _, id := range deleted {
		delete(final, id)
	}

	for _, mc := range configs {
		final[mc.Model] = mc
	}
	// YAML routes are part of the namespace but cannot activate compatibility;
	// activation requires a durable model configuration transaction.
	namespace := imageIdentityEffectiveNamespace(final)

	changed := map[string]ModelConfig{}
	for _, mc := range configs {
		changed[mc.Model] = final[mc.Model]
	}

	addition, err := imageIdentityClaims(changed, namespace)
	if err != nil {
		return err
	}

	if err := validateImageIdentityOwnership(evidence, addition, namespace); err != nil {
		return err
	}

	if err := write(tx); err != nil {
		return err
	}

	return mergeImageIdentityDeclaration(tx, addition)
}

func imageIdentityEffectiveNamespace(final map[string]ModelConfig) map[string]ModelConfig {
	result := make(map[string]ModelConfig, len(final))

	rows := make([]ModelConfig, 0, len(final))
	for id, mc := range final {
		result[id] = mc
		rows = append(rows, mc)
	}

	cache := applyYAMLConfigToModelConfigCache(newModelConfigCache(rows))
	if yaml := LoadYAMLConfig(); yaml != nil {
		for _, item := range yaml.ModelConfigs {
			if mc, ok := cache.GetModelConfig(item.Model); ok {
				result[item.Model] = mc
			}
		}
	}

	return result
}

func imageIdentityClaims(
	changed, namespace map[string]ModelConfig,
) (imageIdentityDeclaration, error) {
	addition := imageIdentityDeclaration{Version: 1, Capabilities: map[string][]string{}}
	for _, mc := range changed {
		if !ValidOpenAIImageProfile(mc) {
			continue
		}

		effective := namespace[mc.Model]
		if !ValidOpenAIImageProfile(effective) ||
			OpenAIImageIdentityConflict(namespace, effective) {
			return addition, errors.New(
				"OpenAI image activation conflicts with the final model namespace",
			)
		}

		identity, _ := openAIImageIdentity(mc)

		effectiveIdentity, _ := openAIImageIdentity(effective)
		if !slices.Equal(identity.Accepted(), effectiveIdentity.Accepted()) {
			return addition, errImageIdentityInvalid
		}

		if fault := LoadModelCaches().imageIdentityReadFailed; fault != nil && fault.Load() {
			return addition, errImageIdentityInvalid
		}

		addition.Capabilities[identity.CapabilityModel] = append(
			addition.Capabilities[identity.CapabilityModel],
			identity.Accepted()...,
		)
	}

	return addition, nil
}

func validateImageIdentityOwnership(
	evidence OpenAIImageIdentitySnapshot,
	addition imageIdentityDeclaration,
	namespace map[string]ModelConfig,
) error {
	// All routes, including non-opt-in writes, respect acquired ownership. A
	// transaction that removes/renames a conflicting old route is checked as one.
	owners := map[string]string{}
	maps.Copy(owners, evidence.declared)

	for ck, ids := range addition.Capabilities {
		for _, id := range ids {
			owners[id] = ck
		}
	}

	for _, mc := range namespace {
		identity, native := openAIImageIdentity(mc)
		for _, name := range imageConfigNames(mc) {
			for reserved, owner := range owners {
				if strings.EqualFold(name, reserved) &&
					(!native || identity.CapabilityModel != owner || !identity.Accepts(reserved)) {
					return errors.New(
						"model identity is reserved for historical OpenAI image operations",
					)
				}
			}
		}
	}

	return nil
}
