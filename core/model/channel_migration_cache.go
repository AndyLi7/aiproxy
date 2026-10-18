package model

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/labring/aiproxy/core/common/config"
	"gorm.io/gorm"
)

// A random boot identity is observation-only. It is not an authenticated fleet
// member until an external trusted runtime binds this exact incarnation.
var migrationCacheBoot, migrationCacheBootError = newAdmissionGeneration()

// ProcessBootIncarnation is the immutable local boot identity. A trusted startup
// adapter may bind this value to its executor membership; reading it is not proof.
func ProcessBootIncarnation() (string, error) { return migrationCacheBoot, migrationCacheBootError }

type MigrationCacheObservationResult struct {
	Version     int                        `json:"version"`
	Source      string                     `json:"source"`
	State       string                     `json:"state"`
	Reason      string                     `json:"reason"`
	OperationID string                     `json:"operationId"`
	Observation *MigrationCacheObservation `json:"observation"`
}
type MigrationCacheObservation struct {
	BootIncarnation   string            `json:"bootIncarnation"`
	CacheVersion      string            `json:"cacheVersion"`
	LoadedAtMs        string            `json:"loadedAtMs"`
	Epoch             string            `json:"epoch"`
	StorageGeneration string            `json:"storageGeneration"`
	SnapshotRevision  string            `json:"snapshotRevision"`
	RequestHash       string            `json:"requestHash"`
	Head              MigrationReadHead `json:"head"`
	CommitRevision    string            `json:"commitRevision"`
	AfterPinHash      string            `json:"afterPinHash"`
}

// Internal only: no credentials, arbitrary configs or runtime authorization are
// exposed. All fields below originate in the same cache read transaction.
type migrationCacheSource struct {
	reason       string
	control      ChannelAdmissionControl
	pinHash      string
	heads        map[string]ChannelAdmissionBarrier
	configValues map[string][2]any
	effective    bool
	routing      map[int]string
}

func readMigrationCacheSource(tx *gorm.DB) (*migrationCacheSource, error) {
	source := &migrationCacheSource{reason: "cache_source_unavailable"}
	if tx.Dialector.Name() != "sqlite" && tx.Dialector.Name() != "postgres" {
		return source, nil
	}
	present, err := channelAdmissionTablesExist(tx)
	if err != nil {
		return nil, err
	}
	if !present {
		source.reason = "cache_source_legacy"
		return source, nil
	}
	if !ChannelAdmissionStorageReady(tx) {
		return source, nil
	}
	// Admission can be installed independently of the migration extension.
	// Its complete absence must not prevent ordinary legacy cache refreshes.
	migrationPresent, err := migrationStoragePresent(tx)
	if err != nil {
		return nil, err
	}
	if !migrationPresent {
		source.reason = "cache_source_legacy"
		return source, nil
	}
	if err = migrationStorageReady(tx); err != nil {
		return nil, err
	}
	if err = tx.First(&source.control, 1).Error; err != nil {
		return nil, err
	}
	c := source.control
	if c.Version != ChannelAdmissionProtocolVersion || c.Epoch == "" || c.StorageGeneration == "" || c.EpochSequence <= 0 || c.ActiveProofID == "" || c.Revision < 0 {
		return source, nil
	}
	if err = migrationReadBoundSource(tx); err != nil {
		if errors.Is(err, ErrChannelAdmissionUnavailable) {
			source.reason = "cache_source_unsupported"
			return source, nil
		}
		return nil, err
	}
	rows, err := migrationConfigRows(tx)
	if err != nil {
		return nil, err
	}
	source.pinHash, err = admissionHash(rows)
	if err != nil {
		return nil, err
	}
	source.configValues = make(map[string][2]any, len(rows))
	for _, row := range rows {
		name, ok := row["model"].(string)
		if !ok || name == "" {
			return source, nil
		}
		if _, exists := source.configValues[name]; exists {
			return source, nil
		}
		var values [2]any
		for i, col := range []string{"config", "plugin"} {
			if row[col] == nil || row[col] == "" {
				values[i] = nil
				continue
			}
			raw, ok := row[col].(string)
			if !ok {
				return source, nil
			}
			value, e := cacheJSONValue([]byte(raw))
			if e != nil {
				return source, nil
			}
			values[i] = value
		}
		source.configValues[name] = values
	}
	var heads []ChannelAdmissionBarrier
	if err = tx.Limit(MigrationInventoryMaxRows + 1).Find(&heads).Error; err != nil {
		return nil, err
	}
	if len(heads) > MigrationInventoryMaxRows {
		source.reason = "cache_source_limit"
		return source, nil
	}
	source.heads = make(map[string]ChannelAdmissionBarrier, len(heads))
	for _, h := range heads {
		source.heads[h.OperationID] = h
	}
	source.reason = ""
	return source, nil
}

// Normalize numbers exactly, not through float64. This is a comparison of the
// effective cached values, never the authoritative raw DB pin hash. Existing
// serializers that lose precision make this observation unsupported.
func cacheJSONValue(raw []byte) (any, error) {
	v, ok := inventoryDecode(raw)
	if !ok {
		return nil, ErrChannelAdmissionUnavailable
	}
	invalidNumber := false
	var normalize func(any) any
	normalize = func(v any) any {
		switch x := v.(type) {
		case json.Number:
			// JSON byte/depth limits do not bound exponent expansion. A cache
			// observation must not allocate an attacker-sized rational number.
			text := string(x)
			if len(text) > 4096 {
				invalidNumber = true
				return nil
			}
			if pos := strings.IndexAny(text, "eE"); pos >= 0 {
				exponent, err := strconv.ParseInt(text[pos+1:], 10, 32)
				if err != nil || exponent < -4096 || exponent > 4096 {
					invalidNumber = true
					return nil
				}
			}
			r, ok := new(big.Rat).SetString(string(x))
			if !ok {
				return x
			}
			return struct{ Number string }{r.RatString()}
		case []any:
			for i := range x {
				x[i] = normalize(x[i])
			}
			return x
		case map[string]any:
			for k := range x {
				x[k] = normalize(x[k])
			}
			return x
		default:
			return v
		}
	}
	normalized := normalize(v)
	if invalidNumber {
		return nil, ErrChannelAdmissionUnavailable
	}
	return normalized, nil
}
func finishMigrationCacheSource(snapshot cacheSnapshot, caches *ModelCaches) *migrationCacheSource {
	s := snapshot.migrationSource
	if s == nil || s.reason != "" {
		return s
	}
	if config.DisableModelConfig {
		s.reason = "effective_cache_projection_unsupported"
		return s
	}
	// Reject any actual wrapper/extra YAML channel, including empty config
	// overrides. No second LoadYAMLConfig call could race the constructed cache.
	if _, ok := caches.ModelConfig.(*modelConfigMapCache); !ok {
		s.reason = "effective_cache_projection_unsupported"
		return s
	}
	if len(caches.ChannelsByID) != len(snapshot.enabledChannels)+len(snapshot.disabledChannels) {
		s.reason = "effective_cache_projection_unsupported"
		return s
	}
	if !migrationCacheConfigsMatch(s, caches) {
		s.reason = "effective_cache_projection_unsupported"
		return s
	}
	s.effective = true
	return s
}

// ReadMigrationCacheObservation observes this process only. It neither loads
// an issuer nor produces fleet confirmation/drain/disable/revoke authority.
func (s *ChannelAdmissionStore) ReadMigrationCacheObservation(ctx context.Context, op string) MigrationCacheObservationResult {
	out := MigrationCacheObservationResult{Version: 1, Source: "gateway-local-cache-observation-v1", State: "unavailable", OperationID: op}
	if s == nil || s.DB != DB {
		out.Reason = "cache_process_storage_mismatch"
		return out
	}
	if !ValidMigrationReadOperationID(op) {
		out.State = "conflict"
		out.Reason = "invalid_operation"
		return out
	}
	caches := LoadModelCaches() // One pointer: routing and provenance cannot tear.
	seen := s.migrationReadTransaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) (MigrationReadResult, error) {
		original, err := readPreparedMigrationFactsTx(tx, c, op)
		if err != nil {
			return original, err
		}
		if original.State != "ready" {
			return original, nil
		}
		r := original.Readback
		if r == nil || r.Receipt.State != "committed" || r.CurrentHead == nil {
			out.State = "pending"
			out.Reason = "operation_not_committed"
			return original, nil
		}
		if migrationCacheBootError != nil || caches == nil || caches.Version == 0 || caches.migrationSource == nil {
			out.State = "pending"
			out.Reason = "cache_not_loaded"
			return original, nil
		}
		source := caches.migrationSource
		if source.reason != "" || !source.effective {
			out.State = "unsupported"
			out.Reason = source.reason
			return original, nil
		}
		if source.control.Epoch != c.Epoch || source.control.StorageGeneration != c.StorageGeneration || source.control.Revision > c.Revision {
			out.State = "conflict"
			out.Reason = "cache_storage_changed"
			return original, nil
		}
		committed, err := strconv.ParseInt(r.Receipt.CommitRevision, 10, 64)
		if err != nil {
			return original, ErrChannelAdmissionConflict
		}
		h, ok := source.heads[op]
		expected := *r.CurrentHead
		actual := MigrationReadHead{h.Version, h.OperationID, h.Epoch, migrationDecimal(h.Revision), h.FromIdentityHash, h.ToIdentityHash, migrationDecimal(h.RecordedAtMs)}
		if source.control.Revision < committed || !ok || actual != expected || source.pinHash != r.Receipt.AfterPinHash {
			out.State = "pending"
			out.Reason = "cache_before_current_commit"
			return original, nil
		}
		for _, want := range []ChannelIdentity{r.From, r.To} {
			ch := caches.ChannelsByID[want.ChannelID]
			identity, e := ChannelIdentityOf(ch)
			if e != nil || identity != want || (ch.Status == ChannelStatusEnabled) != map[int]bool{r.From.ChannelID: r.FromEnabled, r.To.ChannelID: r.ToEnabled}[want.ChannelID] {
				out.State = "unsupported"
				out.Reason = "effective_cache_identity_mismatch"
				return original, nil
			}
		}
		// Full model config values were checked before this pointer's publication.
		// Routing must actually include the target for every model it advertises.
		target := caches.ChannelsByID[r.To.ChannelID]
		if len(target.Models) == 0 {
			out.State = "unsupported"
			out.Reason = "effective_cache_route_missing"
			return original, nil
		}
		for _, id := range []int{r.From.ChannelID, r.To.ChannelID} {
			h, e := migrationCacheRoutingHash(caches.ChannelsByID[id])
			if e != nil || h != source.routing[id] {
				out.State = "unsupported"
				out.Reason = "effective_cache_projection_unsupported"
				return original, nil
			}
		}
		for _, set := range target.GetSets() {
			for _, name := range target.Models {
				found := false
				for _, ch := range caches.EnabledModel2ChannelsBySet[set][name] {
					if ch.ID == target.ID {
						found = true
						break
					}
				}
				if !found {
					out.State = "unsupported"
					out.Reason = "effective_cache_route_missing"
					return original, nil
				}
			}
		}
		if !migrationCacheConfigsMatch(source, caches) {
			out.State = "unsupported"
			out.Reason = "effective_cache_projection_unsupported"
			return original, nil
		}
		out.State = "loaded"
		out.Observation = &MigrationCacheObservation{migrationCacheBoot, strconv.FormatUint(caches.Version, 10), migrationDecimal(caches.BuiltAt.UnixMilli()), c.Epoch, c.StorageGeneration, migrationDecimal(source.control.Revision), r.Receipt.RequestHash, expected, r.Receipt.CommitRevision, r.Receipt.AfterPinHash}
		return original, nil
	})
	if seen.State != "ready" {
		out.State = seen.State
		out.Reason = seen.Reason
		out.Observation = nil
	}
	return out
}

func migrationCacheRoutingHash(ch *Channel) (string, error) {
	if ch == nil {
		return "", ErrChannelAdmissionUnavailable
	}
	models := append([]string{}, ch.Models...)
	slices.Sort(models)
	models = slices.Compact(models)
	sets := append([]string{}, ch.GetSets()...)
	slices.Sort(sets)
	sets = slices.Compact(sets)
	mapping := ch.ModelMapping
	if mapping == nil {
		mapping = map[string]string{}
	}
	return admissionHash([]any{models, sets, mapping, ch.Configs, ch.GetPriority(), ch.BackupOnly})
}
func captureMigrationCacheRouting(snapshot *cacheSnapshot) {
	s := snapshot.migrationSource
	if s == nil || s.reason != "" {
		return
	}
	s.routing = map[int]string{}
	for _, channels := range [][]*Channel{snapshot.enabledChannels, snapshot.disabledChannels} {
		for _, ch := range channels {
			h, e := migrationCacheRoutingHash(ch)
			if e != nil {
				s.reason = "effective_cache_projection_unsupported"
				return
			}
			s.routing[ch.ID] = h
		}
	}
}

func migrationCacheConfigsMatch(s *migrationCacheSource, caches *ModelCaches) bool {
	for name, values := range s.configValues {
		actual, ok := caches.ModelConfig.GetModelConfig(name)
		if !ok {
			return false
		}
		for i, v := range []any{actual.Config, actual.Plugin} {
			raw, err := json.Marshal(v)
			if err != nil {
				return false
			}
			got, err := cacheJSONValue(raw)
			if err != nil || !reflect.DeepEqual(got, values[i]) {
				return false
			}
		}
	}

	return true
}
