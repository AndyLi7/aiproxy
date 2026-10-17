package model

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Protocol 9 is additive. Inner relay return, outer handoff and consumer exit
// are separate facts. None certifies upstream termination or closes a use.
type ChannelAdmissionMeasuredProducer struct {
	ID                string `gorm:"primaryKey;size:128"`
	ExecutionID       string `gorm:"not null;size:128;uniqueIndex"`
	ExecutionAuth     string `gorm:"not null;size:128"`
	UseID             string `gorm:"not null;size:128;uniqueIndex"`
	UseHash           string `gorm:"not null;size:128"`
	StorageGeneration string `gorm:"not null;size:128"`
	ScopeJSON         string `gorm:"not null"`
	PriceJSON         string `gorm:"not null"`
	RequestAtMs       int64  `gorm:"not null"`
	DisableFuzzy      bool   `gorm:"not null"`
	AuthorizationHash string `gorm:"not null;size:128"`
	State             string `gorm:"not null;size:32"`
	StartedAtMs       int64  `gorm:"not null"`
	ReturnedAtMs      int64  `gorm:"not null"`
	HandoffKind       string `gorm:"not null;size:32"`
	UsageID           int    `gorm:"not null;index"`
	LogID             int    `gorm:"not null"`
	UsageHash         string `gorm:"not null;size:128"`
	FrozenHash        string `gorm:"not null;size:128"`
	LogIdentityHash   string `gorm:"not null;size:128"`
}
type ChannelAdmissionMeasuredClaim struct {
	ID                string `gorm:"primaryKey;size:128"`
	SourceID          string `gorm:"not null;size:128;index"`
	UsageID           int    `gorm:"not null;index"`
	OwnerHash         string `gorm:"not null;size:128"`
	UntilMicros       int64  `gorm:"not null"`
	Epoch             string `gorm:"not null;size:128"`
	StorageGeneration string `gorm:"not null;size:128"`
	ServiceID         string `gorm:"not null;size:128"`
	InstanceID        string `gorm:"not null;size:128"`
	BuildSHA          string `gorm:"not null;size:128"`
	ProofJSON         string `gorm:"not null"`
	AuthorizationHash string `gorm:"not null;size:128"`
	State             string `gorm:"not null;size:32"`
	OfferedAtMs       int64  `gorm:"not null"`
	AcceptedAtMs      int64  `gorm:"not null"`
	ExitedAtMs        int64  `gorm:"not null"`
	ReleaseJSON       string `gorm:"not null"`
	ReleaseHash       string `gorm:"not null;size:128"`
}

// Created by the outer server entry, consumed once by real admission. A request
// string, database row or JSON value cannot reconstruct its private capability.
type MeasuredRelayIntent struct {
	startMu   sync.Mutex
	price     Price
	at        time.Time
	fuzzy     bool
	used      bool
	execution *RelayExecutionHandle
	row       ChannelAdmissionMeasuredProducer
	nonce     string
}

func NewMeasuredRelayIntent(price Price, at time.Time, disableFuzzy bool) *MeasuredRelayIntent {
	if !price.HasImageBilling() {
		return nil
	}
	raw, e := json.Marshal(price)
	if e != nil {
		return nil
	}
	var frozen Price
	if json.Unmarshal(raw, &frozen) != nil {
		return nil
	}
	return &MeasuredRelayIntent{price: frozen, at: at, fuzzy: disableFuzzy}
}
func measuredHash(v any) string { h, _ := admissionHash(v); return h }
func measuredProducerAuth(r ChannelAdmissionMeasuredProducer, nonce string) string {
	r.AuthorizationHash = ""
	r.State = "active"
	r.ReturnedAtMs = 0
	r.HandoffKind = ""
	r.UsageID = 0
	r.LogID = 0
	r.UsageHash = ""
	r.FrozenHash = ""
	r.LogIdentityHash = ""
	return measuredHash([]any{"measured-producer-v1", r, nonce})
}
func measuredLogIdentity(l *Log) string {
	return measuredHash([]any{l.ID, string(l.RequestID), string(l.BillingOperationID), l.GroupID, l.TokenID, l.TokenName, l.Mode, l.ChannelID, l.Model, l.Capability, l.RequestAt.UnixMilli(), l.Currency, l.PricingVersion, string(l.UpstreamID)})
}
func validateMeasuredLog(tx *gorm.DB, r *ChannelAdmissionMeasuredProducer) error {
	if r.HandoffKind != "final" {
		if r.LogID != 0 || r.LogIdentityHash != "" {
			return ErrChannelAdmissionConflict
		}
		return nil
	}
	var l Log
	if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&l, "id=?", r.LogID).Error; e != nil {
		return e
	}
	if measuredLogIdentity(&l) != r.LogIdentityHash {
		return ErrChannelAdmissionConflict
	}
	return nil
}
func measuredFrozen(u *AsyncUsageInfo) string {
	return measuredHash([]any{u.MeasuredImage, u.Price, u.Usage, u.UsageContext, u.Amount, u.DisableResolutionFuzzyMatch})
}
func measuredStoragePresent(tx *gorm.DB) (bool, error) {
	var count int64
	names := []string{"channel_admission_measured_producers", "channel_admission_measured_claims"}
	var e error
	switch tx.Dialector.Name() {
	case "sqlite":
		e = tx.Raw("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ?", names).Scan(&count).Error
	case "postgres":
		e = tx.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name IN ?", names).Scan(&count).Error
	case "mysql":
		e = tx.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN ?", names).Scan(&count).Error
		if e != nil || count != 0 {
			return false, ErrChannelAdmissionUnavailable
		}
		var p int64
		if e = tx.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='channel_admission_execution_protocols'").Scan(&p).Error; e != nil {
			return false, e
		}
		if p != 0 {
			if e = tx.Model(&ChannelAdmissionExecutionProtocol{}).Where("id=9").Count(&p).Error; e != nil {
				return false, e
			}
			if p != 0 {
				return false, ErrChannelAdmissionUnavailable
			}
		}
		return false, nil
	default:
		return false, ErrChannelAdmissionUnavailable
	}
	if e != nil {
		return false, e
	}
	base, e := channelExecutionStoragePresent(tx)
	if e != nil {
		return false, e
	}
	var p []ChannelAdmissionExecutionProtocol
	if base {
		if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=9").Find(&p).Error; e != nil {
			return false, e
		}
	}
	if count == 0 && len(p) == 0 {
		return false, nil
	}
	if !base || count != 2 || len(p) != 1 || p[0].Version != 1 {
		return false, ErrChannelAdmissionUnavailable
	}
	for _, m := range []any{&ChannelAdmissionMeasuredProducer{}, &ChannelAdmissionMeasuredClaim{}} {
		s := &gorm.Statement{DB: tx}
		if e = s.Parse(m); e != nil {
			return false, e
		}
		columns := make([]clause.Column, 0, len(s.Schema.DBNames))
		for _, name := range s.Schema.DBNames {
			columns = append(columns, clause.Column{Table: clause.CurrentTable, Name: name})
		}
		rows, e := tx.Model(m).Clauses(clause.Select{Columns: columns}).Limit(0).Rows()
		if e != nil {
			return false, e
		}
		re := rows.Err()
		ce := rows.Close()
		if re != nil {
			return false, re
		}
		if ce != nil {
			return false, ce
		}
	}
	return true, nil
}
func (i *MeasuredRelayIntent) start(tx *gorm.DB, c *ChannelAdmissionControl, execution *RelayExecutionHandle, use *ChannelAdmissionUse, scope RelayAdmissionScope) error {
	i.startMu.Lock()
	defer i.startMu.Unlock()
	if i.used {
		return ErrChannelAdmissionConflict
	}
	i.used = true
	present, e := measuredStoragePresent(tx)
	if e != nil || !present {
		return e
	}
	if execution == nil {
		return ErrChannelAdmissionUnavailable
	}
	nonce, e := newAdmissionGeneration()
	if e != nil {
		return e
	}
	at, e := channelAdmissionTime(tx)
	if e != nil {
		return e
	}
	raw, e := json.Marshal(i.price)
	if e != nil {
		return e
	}
	sc, e := json.Marshal(scope)
	if e != nil {
		return e
	}
	r := ChannelAdmissionMeasuredProducer{ID: measuredHash([]any{"measured-source", execution.id, nonce}), ExecutionID: execution.id, UseID: use.ID, UseHash: executionUseHash(use), StorageGeneration: c.StorageGeneration, ScopeJSON: string(sc), PriceJSON: string(raw), RequestAtMs: i.at.UnixMilli(), DisableFuzzy: i.fuzzy, State: "active", StartedAtMs: at.UnixMilli()}
	var ex ChannelAdmissionExecution
	if e = tx.First(&ex, "id=?", execution.id).Error; e != nil {
		return e
	}
	r.ExecutionAuth = ex.AuthorizationHash
	r.AuthorizationHash = measuredProducerAuth(r, nonce)
	if e = tx.Create(&r).Error; e != nil {
		return e
	}
	i.execution = execution
	i.row = r
	i.nonce = nonce
	return nil
}

// Shared lock order: control -> original execution -> use -> source -> usage
// -> consumer journal -> log. No network request occurs inside these locks.
func lockMeasuredSource(tx *gorm.DB, c *ChannelAdmissionControl, id string) (*ChannelAdmissionMeasuredProducer, error) {
	var hint ChannelAdmissionMeasuredProducer
	if e := tx.First(&hint, "id=?", id).Error; e != nil {
		return nil, e
	}
	var ex ChannelAdmissionExecution
	if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&ex, "id=?", hint.ExecutionID).Error; e != nil {
		return nil, e
	}
	var use ChannelAdmissionUse
	if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&use, "id=?", hint.UseID).Error; e != nil {
		return nil, e
	}
	var r ChannelAdmissionMeasuredProducer
	if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&r, "id=?", id).Error; e != nil {
		return nil, e
	}
	if r.ExecutionAuth != ex.AuthorizationHash || r.ExecutionID != ex.ID || r.UseID != use.ID || r.UseHash != executionUseHash(&use) || ex.UseHash != r.UseHash || ex.UseID != r.UseID || ex.StorageGeneration != c.StorageGeneration || r.StorageGeneration != c.StorageGeneration || use.State != "open" || use.ClosedAtMs != 0 || use.ClosureHash != "" || ex.ContinuationState != "unproven" || ex.Version != 1 || ex.Kind != "relay_call" || !fullIdentityDigest(ex.AuthorizationHash) || r.StartedAtMs <= 0 || !fullIdentityDigest(r.AuthorizationHash) {
		return nil, ErrChannelAdmissionConflict
	}
	if (ex.State == "active" && ex.ReturnedAtMs != 0) || (ex.State != "active" && ex.State != "returned" && ex.State != "error_returned" && ex.State != "async_returned") {
		return nil, ErrChannelAdmissionConflict
	}
	if (r.State == "active" && r.ReturnedAtMs != 0) || (r.State == "returned" && r.ReturnedAtMs < r.StartedAtMs) || (r.State != "active" && r.State != "returned") {
		return nil, ErrChannelAdmissionConflict
	}
	return &r, nil
}
func (i *MeasuredRelayIntent) transaction(ctx context.Context, work func(*gorm.DB, *ChannelAdmissionMeasuredProducer) error) error {
	if i == nil || i.execution == nil || i.execution.store == nil || i.nonce == "" {
		return ErrChannelAdmissionConflict
	}
	return i.execution.store.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		present, e := measuredStoragePresent(tx)
		if e != nil {
			return e
		}
		if !present {
			return ErrChannelAdmissionUnavailable
		}
		r, e := lockMeasuredSource(tx, c, i.row.ID)
		if e != nil {
			return e
		}
		if r.AuthorizationHash != i.row.AuthorizationHash || r.AuthorizationHash != measuredProducerAuth(*r, i.nonce) {
			return ErrChannelAdmissionConflict
		}
		return work(tx, r)
	}, true)
}
func CreateMeasuredImageSettlementOwned(ctx context.Context, i *MeasuredRelayIntent, price Price, entry *Log, u *AsyncUsageInfo) error {
	return persistMeasuredOwned(ctx, i, price, entry, u, true)
}
func CreateMeasuredImageAttemptEvidenceOwned(ctx context.Context, i *MeasuredRelayIntent, price Price, u *AsyncUsageInfo) error {
	return persistMeasuredOwned(ctx, i, price, nil, u, false)
}

// Identity strings here only deny an unsafe legacy fallback. They never grant
// a capability or backfill coverage. Real admission commits the producer before
// any provider action, so a covered outer result must use its original handle.
func validateMeasuredLegacy(tx *gorm.DB, u *AsyncUsageInfo) error {
	present, err := measuredStoragePresent(tx)
	if err != nil || !present {
		return err
	}
	if u == nil {
		return ErrChannelAdmissionConflict
	}
	resource, err := admissionHash([]any{"relay", u.GroupID, u.TokenID, u.BillingOperationID})
	if err != nil {
		return err
	}
	var count int64
	err = tx.Table("channel_admission_measured_producers AS producer").
		Joins("JOIN channel_admission_uses AS original_use ON original_use.id = producer.use_id").
		Where("original_use.resource_key = ?", resource).Count(&count).Error
	if err != nil {
		return err
	}
	if count != 0 {
		return ErrChannelAdmissionConflict
	}
	return nil
}
func persistMeasuredOwned(ctx context.Context, i *MeasuredRelayIntent, price Price, entry *Log, u *AsyncUsageInfo, final bool) error {
	if i == nil || i.execution == nil {
		if e := validateMeasuredLegacy(LogDB.WithContext(ctx), u); e != nil {
			return e
		}
		// Only a genuinely uncovered call may use compatibility persistence.
		// Late installation cannot backfill it; a covered call cannot downgrade.
		if final {
			return createMeasuredImageSettlement(LogDB.WithContext(ctx), entry, u)
		}
		return createMeasuredImageAttemptEvidence(LogDB.WithContext(ctx), u)
	}
	if u == nil || u.ImageTaskID != "" || !reflect.DeepEqual(price, i.price) {
		return ErrChannelAdmissionConflict
	}
	return i.transaction(ctx, func(tx *gorm.DB, r *ChannelAdmissionMeasuredProducer) error {
		var scope RelayAdmissionScope
		if json.Unmarshal([]byte(r.ScopeJSON), &scope) != nil || scope.RequestID != u.RequestID || scope.BillingOperationID != u.BillingOperationID || scope.GroupID != u.GroupID || scope.TokenID != u.TokenID || scope.Mode != u.Mode || scope.Model != u.Model || u.ChannelID != i.executionChannel(tx) || u.RequestAt.UnixMilli() != r.RequestAtMs || u.DisableResolutionFuzzyMatch != r.DisableFuzzy {
			return ErrChannelAdmissionConflict
		}
		if r.HandoffKind != "" {
			kind := "evidence"
			if final {
				kind = "final"
			}
			u.ID = r.UsageID
			u.LogID = r.LogID
			u.MeasuredImage = true
			if r.HandoffKind != kind || imagePollUsageHash(u) != r.UsageHash || measuredFrozen(u) != r.FrozenHash {
				return ErrChannelAdmissionConflict
			}
			var stored AsyncUsageInfo
			if e := tx.First(&stored, "id=?", r.UsageID).Error; e != nil {
				return e
			}
			if imagePollUsageHash(&stored) != r.UsageHash || measuredFrozen(&stored) != r.FrozenHash {
				return ErrChannelAdmissionConflict
			}
			if e := validateMeasuredLog(tx, r); e != nil {
				return e
			}
			*u = stored
			if entry != nil {
				entry.ID = r.LogID
			}
			return nil
		}
		if r.State != "active" {
			return ErrChannelAdmissionConflict
		}
		// The source's request instant is a millisecond identity. Write an exact
		// representable value so PG microsecond rounding cannot cross that bound.
		u.RequestAt = u.RequestAt.UTC().Truncate(time.Millisecond)
		if entry != nil {
			entry.RequestAt = entry.RequestAt.UTC().Truncate(time.Millisecond)
		}
		kind := "evidence"
		var e error
		if final {
			kind = "final"
			if entry == nil || string(entry.BillingOperationID) != u.BillingOperationID || string(entry.RequestID) != u.RequestID || entry.GroupID != u.GroupID || entry.TokenID != u.TokenID || entry.ChannelID != u.ChannelID || entry.TokenName != u.TokenName || entry.Mode != u.Mode || entry.RequestAt.UnixMilli() != u.RequestAt.UnixMilli() || entry.Currency != u.PricingCurrency || entry.PricingVersion != u.PricingVersion {
				return ErrChannelAdmissionConflict
			}
			e = createMeasuredImageSettlement(tx, entry, u)
		} else {
			e = createMeasuredImageAttemptEvidence(tx, u)
		}
		if e != nil {
			return e
		}
		logHash := ""
		if final {
			logHash = measuredLogIdentity(entry)
		}
		values := map[string]any{"log_identity_hash": logHash, "handoff_kind": kind, "usage_id": u.ID, "log_id": u.LogID, "usage_hash": imagePollUsageHash(u), "frozen_hash": measuredFrozen(u)}
		result := tx.Model(r).Where("handoff_kind=? AND state=?", "", "active").Updates(values)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrChannelAdmissionConflict
		}
		return nil
	})
}
func (i *MeasuredRelayIntent) executionChannel(tx *gorm.DB) int {
	var u ChannelAdmissionUse
	if tx.First(&u, "id=?", i.row.UseID).Error != nil {
		return 0
	}
	return u.ChannelID
}
func RecordMeasuredProducerReturn(ctx context.Context, i *MeasuredRelayIntent) error {
	if i == nil || i.execution == nil {
		return nil
	}
	return i.transaction(ctx, func(tx *gorm.DB, r *ChannelAdmissionMeasuredProducer) error {
		if r.HandoffKind != "final" && r.HandoffKind != "evidence" {
			return ErrChannelAdmissionConflict
		}
		var u AsyncUsageInfo
		if e := tx.First(&u, "id=?", r.UsageID).Error; e != nil {
			return e
		}
		if imagePollUsageHash(&u) != r.UsageHash || measuredFrozen(&u) != r.FrozenHash {
			return ErrChannelAdmissionConflict
		}
		if e := validateMeasuredLog(tx, r); e != nil {
			return e
		}
		if r.State == "returned" {
			return nil
		}
		at, e := channelAdmissionTime(tx)
		if e != nil {
			return e
		}
		out := tx.Model(r).Where("state=?", "active").Updates(map[string]any{"state": "returned", "returned_at_ms": at.UnixMilli()})
		if out.Error != nil {
			return out.Error
		}
		if out.RowsAffected != 1 {
			return ErrChannelAdmissionConflict
		}
		return nil
	})
}

type measuredClaimHandle struct {
	store        *ChannelAdmissionStore
	row          ChannelAdmissionMeasuredClaim
	source       ChannelAdmissionMeasuredProducer
	nonce, token string
	parentCtx    context.Context
}

func measuredClaimAuth(r ChannelAdmissionMeasuredClaim, nonce string) string {
	r.AuthorizationHash = ""
	r.State = "offered"
	r.AcceptedAtMs = 0
	r.ExitedAtMs = 0
	r.ReleaseJSON = ""
	r.ReleaseHash = ""
	return measuredHash([]any{"measured-claim-v1", r, nonce})
}
func measuredReleaseHash(r ChannelAdmissionMeasuredClaim, nonce string) string {
	return measuredHash([]any{"measured-release-v1", r.ID, r.AuthorizationHash, r.ReleaseJSON, nonce})
}
func measuredProofRow(r ChannelAdmissionMeasuredClaim) *ChannelAdmissionTaskExecution {
	return &ChannelAdmissionTaskExecution{Epoch: r.Epoch, StorageGeneration: r.StorageGeneration, ServiceID: r.ServiceID, InstanceID: r.InstanceID, BuildSHA: r.BuildSHA, ExecutorProofJSON: r.ProofJSON}
}
func tryClaimMeasured(ctx context.Context, u *AsyncUsageInfo, token string, until, now time.Time) (bool, bool, error) {
	if u.ImageTaskID != "" || !u.MeasuredImage {
		return false, false, nil
	}
	present, e := measuredStoragePresent(LogDB.WithContext(ctx))
	if e != nil {
		return true, false, e
	}
	if !present {
		return false, false, nil
	}
	var sources []ChannelAdmissionMeasuredProducer
	if e = LogDB.WithContext(ctx).Where("usage_id=?", u.ID).Find(&sources).Error; e != nil {
		return true, false, e
	}
	if len(sources) == 0 {
		return false, false, nil
	} // historical/uncovered; never invent source
	if len(sources) != 1 || sources[0].HandoffKind != "final" {
		return true, false, ErrChannelAdmissionConflict
	}
	store, e := channelAdmissionRuntimeStore(LogDB)
	if e != nil {
		return true, false, e
	}
	if store == nil {
		return true, false, ErrChannelAdmissionUnavailable
	}
	// Proof lookup happens outside the transaction. Existing exact executor facts
	// survive source outage; a new executor must present fresh verified membership.
	proof, proofErr := store.deployment(ctx)
	nonce, e := newAdmissionGeneration()
	if e != nil {
		return true, false, e
	}
	var handle *measuredClaimHandle
	won := false
	e = store.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		ok, e := measuredStoragePresent(tx)
		if e != nil {
			return e
		}
		if !ok {
			return ErrChannelAdmissionUnavailable
		}
		source, e := lockMeasuredSource(tx, c, sources[0].ID)
		if e != nil {
			return e
		}
		var current AsyncUsageInfo
		if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id=?", u.ID).Error; e != nil {
			return e
		}
		if source.HandoffKind != "final" || source.UsageID != u.ID || source.LogID != current.LogID || source.UsageHash != imagePollUsageHash(&current) || source.UsageHash != imagePollUsageHash(u) || source.FrozenHash != measuredFrozen(&current) || source.FrozenHash != measuredFrozen(u) || current.ImageTaskID != "" || !current.MeasuredImage {
			return ErrChannelAdmissionConflict
		}
		at, e := channelAdmissionTime(tx)
		if e != nil {
			return e
		}
		if current.Status != AsyncUsageStatusPending || (!current.NextPollAt.IsZero() && current.NextPollAt.After(at)) {
			return nil
		}
		if !until.After(at) {
			return ErrImagePollClaimLost
		}
		ex := store.Executor
		if ex.Epoch != c.Epoch || ex.ServiceID == "" || ex.InstanceID == "" || !fullIdentityDigest(ex.BuildSHA) {
			return ErrChannelAdmissionUnavailable
		}
		var prior []ChannelAdmissionMeasuredClaim
		if e = tx.Where("source_id=?", source.ID).Find(&prior).Error; e != nil {
			return e
		}
		binding := ""
		for _, p := range prior {
			if p.UsageID != u.ID || p.StorageGeneration != c.StorageGeneration || !fullIdentityDigest(p.AuthorizationHash) || !validMeasuredClaim(p) {
				return ErrChannelAdmissionConflict
			}
			if _, e = nativeExecutionHistoricalProof(tx, c, measuredProofRow(p)); e != nil {
				return e
			}
			if p.Epoch == ex.Epoch && p.ServiceID == ex.ServiceID && p.InstanceID == ex.InstanceID && p.BuildSHA == ex.BuildSHA {
				binding = p.ProofJSON
			}
		}
		fresh := binding == ""
		if fresh {
			if proofErr != nil {
				return proofErr
			}
			if _, e = lockDeploymentProof(tx, c, proof, ex); e != nil {
				return e
			}
			raw, e := json.Marshal(proof)
			if e != nil {
				return e
			}
			binding = string(raw)
		}
		until = until.UTC().Truncate(time.Microsecond)
		result := tx.Model(&current).Updates(map[string]any{"processing_token": token, "next_poll_at": until, "updated_at": at})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrImagePollClaimLost
		}
		if e = tx.First(&current, "id=?", u.ID).Error; e != nil {
			return e
		}
		until = current.NextPollAt
		owner := measuredHash([]any{"measured-token", token})
		row := ChannelAdmissionMeasuredClaim{ID: measuredHash([]any{source.ID, u.ID, owner, nonce}), SourceID: source.ID, UsageID: u.ID, OwnerHash: owner, UntilMicros: until.UnixMicro(), Epoch: ex.Epoch, StorageGeneration: c.StorageGeneration, ServiceID: ex.ServiceID, InstanceID: ex.InstanceID, BuildSHA: ex.BuildSHA, ProofJSON: binding, State: "offered", OfferedAtMs: at.UnixMilli()}
		row.AuthorizationHash = measuredClaimAuth(row, nonce)
		if e = tx.Create(&row).Error; e != nil {
			return e
		}
		if e = validateMeasuredLog(tx, source); e != nil {
			return e
		}
		if fresh {
			if e = finalizeAdmissionProof(tx, c, proof, c.Revision); e != nil {
				return e
			}
		}
		at, e = channelAdmissionTime(tx)
		if e != nil {
			return e
		}
		if !until.After(at) {
			return ErrImagePollClaimLost
		}
		handle = &measuredClaimHandle{store: store, row: row, source: *source, nonce: nonce, token: token}
		won = true
		return nil
	}, true)
	if e != nil {
		return true, false, e
	}
	if won {
		u.ProcessingToken = token
		u.NextPollAt = until
		u.UpdatedAt = now
		u.measuredClaim = handle
	}
	return true, won, nil
}
func validMeasuredClaim(r ChannelAdmissionMeasuredClaim) bool {
	if r.OfferedAtMs <= 0 || r.UntilMicros <= r.OfferedAtMs*1000 || !fullIdentityDigest(r.OwnerHash) {
		return false
	}
	switch r.State {
	case "offered":
		return r.AcceptedAtMs == 0 && r.ExitedAtMs == 0 && r.ReleaseJSON == ""
	case "accepted":
		return r.AcceptedAtMs >= r.OfferedAtMs && r.ExitedAtMs == 0
	case "exited":
		return r.AcceptedAtMs >= r.OfferedAtMs && r.ExitedAtMs >= r.AcceptedAtMs
	default:
		return false
	}
}
func (h *measuredClaimHandle) matches(u *AsyncUsageInfo) bool {
	return u != nil && u.ID == h.row.UsageID && u.ImageTaskID == "" && u.ProcessingToken == h.token && imagePollUsageHash(u) == h.source.UsageHash && measuredFrozen(u) == h.source.FrozenHash
}
func (h *measuredClaimHandle) transaction(ctx context.Context, released bool, work func(*gorm.DB, *ChannelAdmissionMeasuredClaim, *AsyncUsageInfo) error) error {
	return h.store.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		ok, e := measuredStoragePresent(tx)
		if e != nil {
			return e
		}
		if !ok {
			return ErrChannelAdmissionUnavailable
		}
		source, e := lockMeasuredSource(tx, c, h.source.ID)
		if e != nil {
			return e
		}
		if source.UsageHash != h.source.UsageHash || source.FrozenHash != h.source.FrozenHash || source.AuthorizationHash != h.source.AuthorizationHash || source.LogIdentityHash != h.source.LogIdentityHash || source.HandoffKind != "final" {
			return ErrChannelAdmissionConflict
		}
		var u AsyncUsageInfo
		if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&u, "id=?", h.row.UsageID).Error; e != nil {
			return e
		}
		var r ChannelAdmissionMeasuredClaim
		if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&r, "id=?", h.row.ID).Error; e != nil {
			return e
		}
		if r.AuthorizationHash != h.row.AuthorizationHash || r.AuthorizationHash != measuredClaimAuth(r, h.nonce) || !validMeasuredClaim(r) || r.SourceID != source.ID || r.StorageGeneration != c.StorageGeneration || source.UsageHash != imagePollUsageHash(&u) || source.FrozenHash != measuredFrozen(&u) {
			return ErrChannelAdmissionConflict
		}
		if _, e = nativeExecutionHistoricalProof(tx, c, measuredProofRow(r)); e != nil {
			return e
		}
		if e = validateMeasuredLog(tx, source); e != nil {
			return e
		}
		deadline := u.NextPollAt.UnixMicro()
		if r.ReleaseJSON != "" {
			var receipt imagePollRelease
			if !released || r.ReleaseHash != measuredReleaseHash(r, h.nonce) || json.Unmarshal([]byte(r.ReleaseJSON), &receipt) != nil || receipt.Operation != u.BillingOperationID || receipt.DeadlineMicros < r.UntilMicros {
				return ErrImagePollClaimLost
			}
			deadline = receipt.DeadlineMicros
		} else if u.Status != AsyncUsageStatusPending || u.ProcessingToken != h.token || deadline < r.UntilMicros {
			return ErrImagePollClaimLost
		}
		at, e := channelAdmissionTime(tx)
		if e != nil {
			return e
		}
		if deadline <= at.UnixMicro() {
			return ErrImagePollClaimLost
		}
		if e = work(tx, &r, &u); e != nil {
			return e
		}
		at, e = channelAdmissionTime(tx)
		if e != nil {
			return e
		}
		if deadline <= at.UnixMicro() {
			return ErrImagePollClaimLost
		}
		return nil
	}, true)
}
func measuredClaimTransaction(ctx context.Context, u *AsyncUsageInfo, work func(*gorm.DB, *imageWalletClaimContext) error) error {
	h := u.measuredClaim
	if !h.matches(u) {
		return ErrImagePollClaimLost
	}
	return h.transaction(ctx, false, func(tx *gorm.DB, r *ChannelAdmissionMeasuredClaim, current *AsyncUsageInfo) error {
		if r.State != "accepted" {
			return ErrChannelAdmissionConflict
		}
		deadline := current.NextPollAt.UnixMicro()
		if e := work(tx, nil); e != nil {
			return e
		}
		var after AsyncUsageInfo
		if e := tx.First(&after, "id=?", current.ID).Error; e != nil {
			return e
		}
		if after.ProcessingToken != h.token || after.Status != AsyncUsageStatusPending {
			if after.ProcessingToken != "" {
				return ErrImagePollClaimLost
			}
			raw, e := json.Marshal(imagePollRelease{DeadlineMicros: deadline, Status: after.Status, NextPollAt: after.NextPollAt, Operation: after.BillingOperationID, Failed: after.Status == AsyncUsageStatusFailed || after.Error != ""})
			if e != nil {
				return e
			}
			r.ReleaseJSON = string(raw)
			r.ReleaseHash = measuredReleaseHash(*r, h.nonce)
			out := tx.Model(r).Where("state=? AND release_json=?", "accepted", "").Updates(map[string]any{"release_json": r.ReleaseJSON, "release_hash": r.ReleaseHash})
			if out.Error != nil {
				return out.Error
			}
			if out.RowsAffected != 1 {
				return ErrChannelAdmissionConflict
			}
		}
		return nil
	})
}
func acceptMeasuredClaim(ctx context.Context, u *AsyncUsageInfo) (bool, error) {
	h := u.measuredClaim
	if !h.matches(u) {
		return false, ErrImagePollClaimLost
	}
	accepted := false
	e := h.transaction(ctx, false, func(tx *gorm.DB, r *ChannelAdmissionMeasuredClaim, _ *AsyncUsageInfo) error {
		if r.State == "accepted" {
			return nil
		}
		if r.State != "offered" {
			return ErrChannelAdmissionConflict
		}
		at, e := channelAdmissionTime(tx)
		if e != nil {
			return e
		}
		out := tx.Model(r).Where("state=?", "offered").Updates(map[string]any{"state": "accepted", "accepted_at_ms": at.UnixMilli()})
		if out.Error != nil {
			return out.Error
		}
		if out.RowsAffected != 1 {
			return ErrChannelAdmissionConflict
		}
		accepted = true
		return nil
	})
	if accepted && e == nil {
		h.parentCtx = ctx
	}
	return accepted && e == nil, e
}
func returnMeasuredClaim(ctx context.Context, u *AsyncUsageInfo) error {
	h := u.measuredClaim
	if !h.matches(u) {
		return ErrImagePollClaimLost
	}
	return h.transaction(ctx, true, func(tx *gorm.DB, r *ChannelAdmissionMeasuredClaim, _ *AsyncUsageInfo) error {
		if r.State != "accepted" {
			return ErrChannelAdmissionConflict
		}
		at, e := channelAdmissionTime(tx)
		if e != nil {
			return e
		}
		out := tx.Model(r).Where("state=?", "accepted").Updates(map[string]any{"state": "exited", "exited_at_ms": at.UnixMilli()})
		if out.Error != nil {
			return out.Error
		}
		if out.RowsAffected != 1 {
			return ErrChannelAdmissionConflict
		}
		return nil
	})
}

// A database-loaded usage row cannot reconstruct the accepted invocation. Old
// source-less rows retain compatibility; covered rows require the private claim.
func validateUnownedMeasured(ctx context.Context, u *AsyncUsageInfo) error {
	if u == nil || u.ImageTaskID != "" || !u.MeasuredImage {
		return nil
	}
	present, e := measuredStoragePresent(LogDB.WithContext(ctx))
	if e != nil || !present {
		return e
	}
	var n int64
	if e = LogDB.WithContext(ctx).Model(&ChannelAdmissionMeasuredProducer{}).Where("usage_id=?", u.ID).Count(&n).Error; e != nil {
		return e
	}
	if n != 0 {
		return ErrImagePollClaimLost
	}
	return nil
}
