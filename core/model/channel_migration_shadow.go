package model

// A shadow receipt proves only the creation of a disabled resource. It does not
// grant admission, a restricted probe, seal, drain or credential revocation.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type ChannelMigrationShadowProtocol struct {
	ID      int `gorm:"primaryKey"`
	Version int `gorm:"not null"`
}
type ChannelMigrationShadowResource struct {
	OperationID          string `gorm:"primaryKey;size:128"`
	Version              int    `gorm:"not null"`
	RequestJSON          string `gorm:"type:text;not null"`
	RequestHash          string `gorm:"size:64;not null"`
	SourceSnapshotHash   string `gorm:"size:64;not null"`
	SourceFamilyHash     string `gorm:"size:64;not null"`
	SourceEpoch          string `gorm:"size:128;not null"`
	SourceRevision       int64  `gorm:"not null"`
	StorageGeneration    string `gorm:"size:128;not null"`
	FromChannelID        int    `gorm:"not null;index"`
	ShadowChannelID      int    `gorm:"not null;uniqueIndex"`
	ShadowKeyDigest      string `gorm:"size:64;not null"`
	ShadowIdentityHash   string `gorm:"size:64;not null"`
	ShadowProjectionHash string `gorm:"size:64;not null"`
	CreatedRevision      int64  `gorm:"not null"`
	CreatedAtMs          int64  `gorm:"not null"`
	CheckpointHash       string `gorm:"size:64;not null"`
}
type MigrationShadowSource struct {
	SourceSnapshotHash string          `json:"sourceSnapshotHash"`
	Epoch              string          `json:"epoch"`
	Revision           string          `json:"revision"`
	StorageGeneration  string          `json:"storageGeneration"`
	From               ChannelIdentity `json:"from"`
}

// Projection deliberately shares the inventory vocabulary, not provider parameters.
// ConfigsJSON is reviewed with the existing typed inventory rules, never remapped.
type MigrationShadowProjection struct {
	Name                   string            `json:"name"`
	Type                   ChannelType       `json:"type"`
	BaseURL                string            `json:"baseUrl"`
	ProxyURL               string            `json:"proxyUrl"`
	SkipTLSVerify          bool              `json:"skipTlsVerify"`
	Models                 []string          `json:"models"`
	ModelMapping           map[string]string `json:"modelMapping"`
	Sets                   []string          `json:"sets"`
	Priority               int32             `json:"priority"`
	BackupOnly             bool              `json:"backupOnly"`
	EnabledNoPermissionBan bool              `json:"enabledNoPermissionBan"`
	WarnErrorRate          float64           `json:"warnErrorRate"`
	MaxErrorRate           float64           `json:"maxErrorRate"`
	ConfigsJSON            *string           `json:"configsJson"`
}
type MigrationShadowTarget struct {
	CredentialID     string                    `json:"credentialId"`
	CredentialDigest string                    `json:"credentialDigest"`
	Projection       MigrationShadowProjection `json:"projection"`
}
type MigrationShadowRequest struct {
	Version     int                   `json:"version"`
	OperationID string                `json:"operationId"`
	Source      MigrationShadowSource `json:"source"`
	Target      MigrationShadowTarget `json:"target"`
}
type MigrationShadowReceipt struct {
	OperationID          string          `json:"operationId"`
	RequestHash          string          `json:"requestHash"`
	SourceSnapshotHash   string          `json:"sourceSnapshotHash"`
	Epoch                string          `json:"epoch"`
	StorageGeneration    string          `json:"storageGeneration"`
	CreatedRevision      string          `json:"createdRevision"`
	CreatedAtMs          string          `json:"createdAtMs"`
	From                 ChannelIdentity `json:"from"`
	Shadow               ChannelIdentity `json:"shadow"`
	ShadowProjectionHash string          `json:"shadowProjectionHash"`
	CheckpointHash       string          `json:"checkpointHash"`
}
type MigrationShadowResult struct {
	Version           int                     `json:"version"`
	Source            string                  `json:"source"`
	State             string                  `json:"state"`
	Reason            string                  `json:"reason"`
	OperationID       string                  `json:"operationId"`
	Epoch             string                  `json:"epoch"`
	StorageGeneration string                  `json:"storageGeneration"`
	CurrentRevision   string                  `json:"currentRevision"`
	Receipt           *MigrationShadowReceipt `json:"receipt"`
}

func shadowResult(op, state, reason string) MigrationShadowResult {
	return MigrationShadowResult{Version: 1, Source: "gateway-migration-shadow-v1", State: state, Reason: reason, OperationID: op}
}
func shadowStoragePresent(tx *gorm.DB) (bool, error) {
	var n int64
	names := []string{"channel_migration_shadow_protocols", "channel_migration_shadow_resources"}
	var e error
	switch tx.Dialector.Name() {
	case "sqlite":
		e = tx.Raw("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ?", names).Scan(&n).Error
	case "postgres":
		e = tx.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name IN ?", names).Scan(&n).Error
	case "mysql":
		e = tx.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN ?", names).Scan(&n).Error
	default:
		return false, ErrChannelAdmissionUnavailable
	}
	return n > 0, e
}
func shadowStorageReady(tx *gorm.DB) error {
	for _, m := range []any{&ChannelMigrationShadowProtocol{}, &ChannelMigrationShadowResource{}} {
		st := &gorm.Statement{DB: tx}
		if st.Parse(m) != nil {
			return ErrChannelAdmissionUnavailable
		}
		rows, e := tx.Model(m).Select(st.Schema.DBNames).Limit(0).Rows()
		if e != nil {
			return ErrChannelAdmissionUnavailable
		}
		e = rows.Err()
		ce := rows.Close()
		if e != nil || ce != nil {
			return ErrChannelAdmissionUnavailable
		}
	}
	var p ChannelMigrationShadowProtocol
	if tx.First(&p, 1).Error != nil || p.Version != 1 {
		return ErrChannelAdmissionUnavailable
	}
	var bad int64
	if tx.Model(&ChannelMigrationShadowResource{}).Where("version IS NULL OR version <> 1 OR shadow_channel_id IS NULL OR shadow_channel_id <= 0 OR from_channel_id IS NULL OR from_channel_id <= 0 OR created_revision IS NULL OR source_revision IS NULL OR created_revision <= source_revision OR created_at_ms IS NULL OR created_at_ms <= 0").Count(&bad).Error != nil || bad != 0 {
		return ErrChannelAdmissionUnavailable
	}
	return nil
}
func optionalShadowStorageReady(tx *gorm.DB) error {
	present, e := shadowStoragePresent(tx)
	if e != nil {
		return e
	}
	if !present {
		return nil
	}
	return shadowStorageReady(tx)
}
func guardOwnedShadowMutation(tx *gorm.DB, ids []int) error {
	present, e := shadowStoragePresent(tx)
	if e != nil {
		return e
	}
	if !present {
		return nil
	}
	if e = shadowStorageReady(tx); e != nil {
		return e
	}
	var n int64
	if e = tx.Model(&ChannelMigrationShadowResource{}).Where("shadow_channel_id IN ?", ids).Count(&n).Error; e != nil {
		return e
	}
	if n != 0 {
		return ErrChannelAdmissionConflict
	}
	return nil
}
func shadowRevision(text string) (int64, bool) {
	n, e := strconv.ParseInt(text, 10, 64)
	return n, e == nil && n >= 0 && strconv.FormatInt(n, 10) == text
}
func shadowDigest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
func validShadowRequest(r MigrationShadowRequest) bool {
	_, ok := shadowRevision(r.Source.Revision)
	if r.Version != 1 || !ValidMigrationReadOperationID(r.OperationID) || !ok || r.Source.Epoch == "" || len(r.Source.Epoch) > 128 || r.Source.StorageGeneration == "" || len(r.Source.StorageGeneration) > 128 || !fullIdentityDigest(r.Source.SourceSnapshotHash) || r.Source.From.ChannelID <= 0 || !fullIdentityDigest(r.Source.From.KeyDigest) || !fullIdentityDigest(r.Source.From.IdentityHash) || r.Target.CredentialID == "" || len(r.Target.CredentialID) > 128 || !fullIdentityDigest(r.Target.CredentialDigest) {
		return false
	}
	p := r.Target.Projection
	if p.Name == "" || len(p.Name) > 64 || p.Type <= 0 || len(p.BaseURL) > 128 || len(p.ProxyURL) > 255 || len(p.Models) == 0 || len(p.Models) > 1024 || len(p.ModelMapping) > 1024 || len(p.Sets) > 128 || p.Models == nil || p.Sets == nil || p.ModelMapping == nil || p.Priority < 0 || p.WarnErrorRate < 0 || p.MaxErrorRate < 0 {
		return false
	}
	for _, list := range [][]string{p.Models, p.Sets} {
		seen := map[string]bool{}
		for _, v := range list {
			if v == "" || len(v) > 1024 || seen[v] {
				return false
			}
			seen[v] = true
		}
	}
	for k, v := range p.ModelMapping {
		if k == "" || v == "" || len(k) > 1024 || len(v) > 1024 {
			return false
		}
	}
	if p.ConfigsJSON != nil && (len(*p.ConfigsJSON) > 256*1024 || !inventoryReviewText(*p.ConfigsJSON, "channel")) {
		return false
	}
	raw, e := json.Marshal(r)
	return e == nil && len(raw) <= 512*1024 && inventorySafeJSON(raw, nil, 0)
}
func shadowChannel(r MigrationShadowRequest, key string) (*Channel, error) {
	p := r.Target.Projection
	ch := &Channel{Name: p.Name, Key: key, Type: p.Type, Status: ChannelStatusDisabled, BaseURL: p.BaseURL, ProxyURL: p.ProxyURL, SkipTLSVerify: p.SkipTLSVerify, Models: p.Models, ModelMapping: p.ModelMapping, Sets: p.Sets, Priority: p.Priority, BackupOnly: p.BackupOnly, EnabledNoPermissionBan: p.EnabledNoPermissionBan, WarnErrorRate: p.WarnErrorRate, MaxErrorRate: p.MaxErrorRate}
	if p.ConfigsJSON != nil {
		d := json.NewDecoder(strings.NewReader(*p.ConfigsJSON))
		d.UseNumber()
		if d.Decode(&ch.Configs) != nil {
			return nil, ErrChannelAdmissionConflict
		}
	}
	return ch, nil
}
func shadowIdentity(r *ChannelMigrationShadowResource) ChannelIdentity {
	return ChannelIdentity{ChannelID: r.ShadowChannelID, KeyDigest: r.ShadowKeyDigest, IdentityHash: r.ShadowIdentityHash}
}
func shadowCheckpoint(r *ChannelMigrationShadowResource) (string, error) {
	return admissionHash(struct {
		Operation, Request, Source, Family, Epoch, Storage string
		From, Shadow                                       int
		Key, Identity, Projection                          string
		Revision, Time                                     int64
	}{r.OperationID, r.RequestHash, r.SourceSnapshotHash, r.SourceFamilyHash, r.SourceEpoch, r.StorageGeneration, r.FromChannelID, r.ShadowChannelID, r.ShadowKeyDigest, r.ShadowIdentityHash, r.ShadowProjectionHash, r.CreatedRevision, r.CreatedAtMs})
}
func shadowResourceRequest(row *ChannelMigrationShadowResource) (MigrationShadowRequest, error) {
	var req MigrationShadowRequest
	if len(row.RequestJSON) > 512*1024 {
		return req, ErrChannelAdmissionConflict
	}
	if _, ok := inventoryDecode([]byte(row.RequestJSON)); !ok {
		return req, ErrChannelAdmissionConflict
	}
	d := json.NewDecoder(strings.NewReader(row.RequestJSON))
	d.DisallowUnknownFields()
	if d.Decode(&req) != nil || !validShadowRequest(req) {
		return req, ErrChannelAdmissionConflict
	}
	hash, e := shadowRequestHash(req, row.SourceFamilyHash)
	revision, _ := shadowRevision(req.Source.Revision)
	check, ce := shadowCheckpoint(row)
	if e != nil || ce != nil || !fullIdentityDigest(row.SourceFamilyHash) || row.Version != 1 || row.OperationID != req.OperationID || row.RequestHash != hash || row.SourceRevision != revision || row.SourceSnapshotHash != req.Source.SourceSnapshotHash || row.SourceEpoch != req.Source.Epoch || row.StorageGeneration != req.Source.StorageGeneration || row.FromChannelID != req.Source.From.ChannelID || row.ShadowChannelID <= 0 || row.ShadowChannelID == row.FromChannelID || row.ShadowKeyDigest != req.Target.CredentialDigest || !fullIdentityDigest(row.ShadowIdentityHash) || !fullIdentityDigest(row.ShadowProjectionHash) || row.CreatedRevision != row.SourceRevision+1 || row.CreatedAtMs <= 0 || check != row.CheckpointHash {
		return req, ErrChannelAdmissionConflict
	}
	return req, nil
}
func shadowNoOperationEffects(tx *gorm.DB, op string) error {
	for _, q := range []*gorm.DB{tx.Model(&ChannelMigrationPrepared{}).Where("operation_id = ?", op), tx.Model(&ChannelAdmissionBarrier{}).Where("operation_id = ?", op), tx.Model(&ChannelAdmissionFamily{}).Where("operation_id = ?", op)} {
		var n int64
		if e := q.Count(&n).Error; e != nil {
			return e
		}
		if n != 0 {
			return ErrChannelAdmissionConflict
		}
	}
	return nil
}
func shadowObserve(tx *gorm.DB, c *ChannelAdmissionControl, row *ChannelMigrationShadowResource, features []string) (MigrationShadowReceipt, error) {
	var empty MigrationShadowReceipt
	var bindings int64
	if e := tx.Model(&ChannelMigrationShadowResource{}).Where("operation_id = ? OR shadow_channel_id = ?", row.OperationID, row.ShadowChannelID).Count(&bindings).Error; e != nil {
		return empty, e
	}
	if bindings != 1 {
		return empty, ErrChannelAdmissionConflict
	}
	req, e := shadowResourceRequest(row)
	if e != nil {
		return empty, e
	}
	if c.Epoch != row.SourceEpoch || c.StorageGeneration != row.StorageGeneration || c.Revision < row.CreatedRevision {
		return empty, ErrChannelAdmissionConflict
	}
	family, e := migrationFamilyHash(tx, req.Source.From.KeyDigest, req.Target.CredentialDigest)
	if e != nil || family != row.SourceFamilyHash {
		return empty, ErrChannelAdmissionConflict
	}
	inv, e := migrationInventoryTx(tx, c, features)
	if e != nil || inv.State != "ready" || !inv.Complete {
		return empty, ErrChannelAdmissionUnavailable
	}
	index := -1
	for i, ch := range inv.Channels {
		if ch.Identity.ChannelID == row.ShadowChannelID {
			if index != -1 || ch.Identity != shadowIdentity(row) || ch.ProjectionHash != row.ShadowProjectionHash || ch.Routing.Status != ChannelStatusDisabled || ch.Routing.DeletedAt != nil {
				return empty, ErrChannelAdmissionConflict
			}
			index = i
		}
	}
	if index < 0 {
		return empty, ErrChannelAdmissionConflict
	}
	// The only omitted member is the exact immutable, uniquely bound resource just
	// verified above. Preserve every original row/config/feature/epoch/storage fact.
	inv.Channels = append(inv.Channels[:index:index], inv.Channels[index+1:]...)
	inv.Revision = strconv.FormatInt(row.SourceRevision, 10)
	inv.SourceSnapshotHash = ""
	before, e := admissionHash(inv)
	if e != nil || before != row.SourceSnapshotHash {
		return empty, ErrChannelAdmissionConflict
	}
	return MigrationShadowReceipt{row.OperationID, row.RequestHash, row.SourceSnapshotHash, row.SourceEpoch, row.StorageGeneration, strconv.FormatInt(row.CreatedRevision, 10), strconv.FormatInt(row.CreatedAtMs, 10), req.Source.From, shadowIdentity(row), row.ShadowProjectionHash, row.CheckpointHash}, nil
}
func shadowSuccess(op string, c *ChannelAdmissionControl, receipt *MigrationShadowReceipt) MigrationShadowResult {
	out := shadowResult(op, "ready", "")
	out.Epoch = c.Epoch
	out.StorageGeneration = c.StorageGeneration
	out.CurrentRevision = strconv.FormatInt(c.Revision, 10)
	out.Receipt = receipt
	return out
}
func shadowFailure(op string, e error) MigrationShadowResult {
	if errors.Is(e, ErrChannelAdmissionConflict) {
		return shadowResult(op, "conflict", "shadow_source_or_resource_conflict")
	}
	if errors.Is(e, ErrChannelAdmissionUnavailable) {
		return shadowResult(op, "unsupported", "shadow_storage_or_source_unavailable")
	}
	return shadowResult(op, "unavailable", "shadow_request_unavailable")
}

// CreateMigrationShadow is the sole operation-resource creator. key is not part
// of any persistent request/receipt, and no caller-provided evidence is accepted.
func (s *ChannelAdmissionStore) CreateMigrationShadow(ctx context.Context, req MigrationShadowRequest, key string, features []string) MigrationShadowResult {
	if !validShadowRequest(req) || strings.TrimSpace(key) == "" || len(key) > 64*1024 || shadowDigest(key) != req.Target.CredentialDigest {
		return shadowFailure(req.OperationID, ErrChannelAdmissionConflict)
	}
	raw, _ := json.Marshal(req)
	if !inventorySafeJSON(raw, []string{key}, 0) {
		return shadowFailure(req.OperationID, ErrChannelAdmissionConflict)
	}
	if s == nil || s.DB == nil {
		return shadowFailure(req.OperationID, ErrChannelAdmissionUnavailable)
	}
	safe := *s
	safe.DB = s.DB.Session(&gorm.Session{Logger: logger.Discard})
	if s.DB == s.LogDB {
		safe.LogDB = safe.DB
	}
	out := shadowResult(req.OperationID, "unsupported", "shadow_unavailable")
	err := safe.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		if e := migrationStorageReady(tx); e != nil {
			return e
		}
		if e := shadowStorageReady(tx); e != nil {
			return e
		}
		var row ChannelMigrationShadowResource
		e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "operation_id = ?", req.OperationID).Error
		hash, _ := shadowRequestHash(req, row.SourceFamilyHash)
		if e == nil {
			if row.RequestHash != hash {
				return ErrChannelAdmissionConflict
			}
			receipt, e := shadowObserve(tx, c, &row, features)
			if e != nil {
				return e
			}
			out = shadowSuccess(req.OperationID, c, &receipt)
			return nil
		}
		if !errors.Is(e, gorm.ErrRecordNotFound) {
			return e
		}
		if e = shadowNoOperationEffects(tx, req.OperationID); e != nil {
			return e
		}
		// Current management writers share control. These row locks additionally
		// observe preceding external edits before accepting the exact source.
		var ids []int
		if e = tx.Unscoped().Model(&Channel{}).Clauses(clause.Locking{Strength: "UPDATE"}).Order("id").Limit(MigrationInventoryMaxRows+1).Pluck("id", &ids).Error; e != nil {
			return e
		}
		var models []string
		if e = tx.Model(&ModelConfig{}).Clauses(clause.Locking{Strength: "UPDATE"}).Order("model").Limit(MigrationInventoryMaxRows+1).Pluck("model", &models).Error; e != nil {
			return e
		}
		if len(ids) > MigrationInventoryMaxRows || len(models) > MigrationInventoryMaxRows {
			return ErrChannelAdmissionUnavailable
		}
		inv, e := migrationInventoryTx(tx, c, features)
		if e != nil || inv.State != "ready" || !inv.Complete {
			return ErrChannelAdmissionUnavailable
		}
		if inv.SourceSnapshotHash != req.Source.SourceSnapshotHash || inv.Epoch != req.Source.Epoch || inv.Revision != req.Source.Revision || inv.StorageGeneration != req.Source.StorageGeneration {
			return ErrChannelAdmissionConflict
		}
		found := false
		for _, ch := range inv.Channels {
			if ch.Identity == req.Source.From && ch.Routing.Status == ChannelStatusEnabled && ch.Routing.DeletedAt == nil {
				found = true
			}
		}
		if !found {
			return ErrChannelAdmissionConflict
		}
		var sourceKeys []string
		if e = tx.Unscoped().Model(&Channel{}).Pluck("key", &sourceKeys).Error; e != nil {
			return e
		}
		if !inventorySafeJSON(raw, sourceKeys, 0) {
			return ErrChannelAdmissionConflict
		}
		ch, e := shadowChannel(req, key)
		if e != nil {
			return e
		}
		// Resolve models under this same source transaction, not through global DB.
		if !shadowModelsExist(models, ch.Models) {
			return ErrChannelAdmissionConflict
		}
		if e = tx.Create(ch).Error; e != nil {
			return e
		}
		var persisted Channel
		if e = tx.First(&persisted, ch.ID).Error; e != nil {
			return e
		}
		if persisted.Key != key || !shadowTargetMatches(req.Target.Projection, &persisted) {
			return ErrChannelAdmissionConflict
		}
		identity, e := ChannelIdentityOf(&persisted)
		if e != nil {
			return e
		}
		c.Revision++
		res := tx.Model(&ChannelAdmissionControl{}).Where("id=1 AND revision = ?", c.Revision-1).UpdateColumn("revision", c.Revision)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return ErrChannelAdmissionConflict
		}
		after, e := migrationInventoryTx(tx, c, features)
		if e != nil || after.State != "ready" {
			return ErrChannelAdmissionUnavailable
		}
		projection := ""
		for _, item := range after.Channels {
			if item.Identity == identity {
				projection = item.ProjectionHash
			}
		}
		if projection == "" {
			return ErrChannelAdmissionConflict
		}
		// Rebuild expected channel through the exact serializer. The inventory still
		// hashes raw configs; an unequal persisted value cannot be silently accepted.
		if persisted.Status != ChannelStatusDisabled {
			return ErrChannelAdmissionConflict
		}
		if req.Target.Projection.ConfigsJSON != nil {
			var stored string
			if e = tx.Table("channels").Select("configs").Where("id=?", ch.ID).Scan(&stored).Error; e != nil {
				return e
			}
			if !shadowJSONEqual(*req.Target.Projection.ConfigsJSON, stored) {
				return ErrChannelAdmissionConflict
			}
		}
		now, e := channelAdmissionTime(tx)
		if e != nil {
			return e
		}
		sourceRevision, _ := shadowRevision(req.Source.Revision)
		familyHash, e := migrationFamilyHash(tx, req.Source.From.KeyDigest, req.Target.CredentialDigest)
		if e != nil {
			return e
		}
		hash, e = shadowRequestHash(req, familyHash)
		if e != nil {
			return e
		}
		row = ChannelMigrationShadowResource{OperationID: req.OperationID, Version: 1, RequestJSON: string(raw), RequestHash: hash, SourceFamilyHash: familyHash, SourceSnapshotHash: req.Source.SourceSnapshotHash, SourceEpoch: c.Epoch, SourceRevision: sourceRevision, StorageGeneration: c.StorageGeneration, FromChannelID: req.Source.From.ChannelID, ShadowChannelID: ch.ID, ShadowKeyDigest: identity.KeyDigest, ShadowIdentityHash: identity.IdentityHash, ShadowProjectionHash: projection, CreatedRevision: c.Revision, CreatedAtMs: now.UnixMilli()}
		row.CheckpointHash, e = shadowCheckpoint(&row)
		if e != nil {
			return e
		}
		if e = tx.Create(&row).Error; e != nil {
			return e
		}
		receipt, e := shadowObserve(tx, c, &row, features)
		if e != nil {
			return e
		}
		out = shadowSuccess(req.OperationID, c, &receipt)
		return nil
	})
	if err != nil {
		return shadowFailure(req.OperationID, err)
	}
	// Disabled creation has no serving-route change and does not rebuild global caches.
	return out
}
func shadowModelsExist(all, wanted []string) bool {
	sort.Strings(all)
	for _, m := range wanted {
		if i := sort.SearchStrings(all, m); i == len(all) || all[i] != m {
			return false
		}
	}
	return true
}
func shadowJSONEqual(a, b string) bool {
	av, ao := inventoryDecode([]byte(a))
	bv, bo := inventoryDecode([]byte(b))
	if !ao || !bo {
		return false
	}
	aa, e := json.Marshal(av)
	bb, f := json.Marshal(bv)
	return e == nil && f == nil && bytes.Equal(aa, bb)
}

// ReadMigrationShadow is an observation only: never a revision self-update,
// create, automatic repair or fallback to a name lookup.
func (s *ChannelAdmissionStore) ReadMigrationShadow(ctx context.Context, op string, features []string) MigrationShadowResult {
	if !ValidMigrationReadOperationID(op) {
		return shadowFailure(op, ErrChannelAdmissionConflict)
	}
	out := shadowResult(op, "unsupported", "shadow_unavailable")
	observed := s.migrationReadTransaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) (MigrationReadResult, error) {
		marker := migrationReadState("ready", "")
		if e := shadowStorageReady(tx); e != nil {
			return marker, e
		}
		var row ChannelMigrationShadowResource
		e := tx.First(&row, "operation_id = ?", op).Error
		if errors.Is(e, gorm.ErrRecordNotFound) {
			if e = shadowNoOperationEffects(tx, op); e != nil {
				return marker, e
			}
			inv, e := migrationInventoryTx(tx, c, features)
			if e != nil || inv.State != "ready" || !inv.Complete {
				return marker, ErrChannelAdmissionUnavailable
			}
			out = shadowSuccess(op, c, nil)
			out.State = "absent"
			return marker, nil
		}
		if e != nil {
			return marker, e
		}
		receipt, e := shadowObserve(tx, c, &row, features)
		if e != nil {
			return marker, e
		}
		out = shadowSuccess(op, c, &receipt)
		return marker, nil
	})
	if observed.State != "ready" {
		out = shadowResult(op, observed.State, "shadow_read_unavailable")
		if observed.State == "conflict" {
			out.Reason = "shadow_source_or_resource_conflict"
		}
	}
	return out
}

// Ordinary insertions must serialize with whole-source CAS. Absence of the
// extension preserves legacy creation; partial installation cannot bypass it.
func withMigrationChannelCreate(write func(*gorm.DB) error) error {
	exists, e := migrationStoragePresent(DB)
	if e != nil {
		return e
	}
	if !exists {
		return DB.Transaction(write)
	}
	store := &ChannelAdmissionStore{DB: DB, LogDB: LogDB}
	return store.transaction(context.Background(), func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		if e := migrationStorageReady(tx); e != nil {
			return e
		}
		return write(tx)
	}, true)
}

func shadowOrdinaryWriteReady(db *gorm.DB) error {
	present, e := shadowStoragePresent(db)
	if e != nil {
		return e
	}
	if !present {
		return nil
	}
	if !ChannelAdmissionStorageReady(db) {
		return ErrChannelAdmissionUnavailable
	}
	return migrationStorageReady(db)
}

func shadowRequestHash(req MigrationShadowRequest, family string) (string, error) {
	return admissionHash(struct {
		Request          MigrationShadowRequest
		SourceFamilyHash string
	}{req, family})
}

func shadowTargetMatches(p MigrationShadowProjection, ch *Channel) bool {
	if p.ConfigsJSON == nil && len(ch.Configs) != 0 {
		return false
	}
	expected := p
	expected.ConfigsJSON = nil
	actual := MigrationShadowProjection{Name: ch.Name, Type: ch.Type, BaseURL: ch.BaseURL, ProxyURL: ch.ProxyURL, SkipTLSVerify: ch.SkipTLSVerify, Models: ch.Models, ModelMapping: ch.ModelMapping, Sets: ch.Sets, Priority: ch.Priority, BackupOnly: ch.BackupOnly, EnabledNoPermissionBan: ch.EnabledNoPermissionBan, WarnErrorRate: ch.WarnErrorRate, MaxErrorRate: ch.MaxErrorRate}
	a, e := json.Marshal(expected)
	b, f := json.Marshal(actual)
	return e == nil && f == nil && bytes.Equal(a, b) && ch.Status == ChannelStatusDisabled && !ch.DeletedAt.Valid
}
