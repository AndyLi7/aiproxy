package model

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// One invocation of the actual image outbox worker. Neither an expired lease nor
// a returned invocation proves producer continuity, child completion or use closure.
type ChannelAdmissionImagePollHandoff struct {
	ChannelAdmissionImageRecoveryHandoff `gorm:"embedded"`
	UsageID                              int    `gorm:"not null"`
	UsageHash                            string `gorm:"not null;size:128"`
	ReleaseJSON                          string `gorm:"not null"`
	ReleaseHash                          string `gorm:"not null;size:128"`
}

func (ChannelAdmissionImagePollHandoff) TableName() string {
	return "channel_admission_image_poll_handoffs"
}

type imagePollHandle struct {
	store *ChannelAdmissionStore
	// Set once after successful Accept, before worker/renewal start; read-only thereafter.
	parentCtx    context.Context
	row          ChannelAdmissionImagePollHandoff
	nonce, token string
	taskID       string
}
type imagePollRelease struct {
	DeadlineMicros int64
	Status         AsyncUsageStatus
	NextPollAt     time.Time
	Operation      string
	Failed         bool
	TerminalCancel bool
}

func imagePollUsageHash(u *AsyncUsageInfo) string {
	h, _ := admissionHash([]any{"image-poll-usage-v1", u.ID, u.ImageTaskID, u.GroupID, u.TokenID, u.LogID, u.RequestID, u.BillingOperationID, u.Mode, u.ChannelID, u.BaseURL, u.UpstreamID, u.TokenName, u.Capability, u.MeasuredImage, u.UsageModelID, u.UsageSource, u.UsageAtMs, u.RequestAt.UnixMilli(), u.Model, u.InternalImageTask, u.PricingCurrency, u.PricingVersion})
	return h
}
func imagePollTaskHash(t *ImageTask) string {
	h, _ := admissionHash([]any{"image-poll-task-v1", imageRecoveryCoreHash(t), imageRecoveryContractHash(t), t.UpstreamID, t.UpstreamModel, t.KeyFingerprint, t.ChannelType, t.ArchiveRequired})
	return h
}
func imagePollClaimHash(r ChannelAdmissionImagePollHandoff) string {
	h, _ := admissionHash([]any{"image-poll-claim-v1", r.UseID, r.UsageID, r.ClaimOwner})
	return h
}
func imagePollAuth(r ChannelAdmissionImagePollHandoff, nonce string) string {
	r.AuthorizationHash = ""
	r.State = "offered"
	r.ExecutionState = "active"
	r.AcceptedAtMs = 0
	r.ExitedAtMs = 0
	r.ReleaseJSON = ""
	r.ReleaseHash = ""
	h, _ := admissionHash([]any{"image-poll-auth-v1", r, nonce})
	return h
}
func imagePollReleaseHash(r ChannelAdmissionImagePollHandoff, nonce string) string {
	h, _ := admissionHash([]any{"image-poll-release-v1", r.ID, r.AuthorizationHash, r.ReleaseJSON, nonce})
	return h
}
func imagePollStoragePresent(tx *gorm.DB) (bool, error) {
	var count int64
	switch tx.Dialector.Name() {
	case "sqlite":
		if e := tx.Raw("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", "channel_admission_image_poll_handoffs").Scan(&count).Error; e != nil {
			return false, e
		}
	case "postgres":
		if e := tx.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name=?", "channel_admission_image_poll_handoffs").Scan(&count).Error; e != nil {
			return false, e
		}
	case "mysql":
		// Preserve legacy MySQL only when this extension is absent. Never silently
		// accept a partial install on an unsupported admission dialect.
		if e := tx.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?", "channel_admission_image_poll_handoffs").Scan(&count).Error; e != nil {
			return false, e
		}
		if count != 0 {
			return false, ErrChannelAdmissionUnavailable
		}
		if e := tx.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?", "channel_admission_execution_protocols").Scan(&count).Error; e != nil {
			return false, e
		}
		if count != 0 {
			if e := tx.Model(&ChannelAdmissionExecutionProtocol{}).Where("id=7").Count(&count).Error; e != nil {
				return false, e
			}
			if count != 0 {
				return false, ErrChannelAdmissionUnavailable
			}
		}
		return false, nil
	default:
		return false, ErrChannelAdmissionUnavailable
	}
	base, e := imageRecoveryHandoffStoragePresent(tx)
	if e != nil {
		return false, e
	}
	relay, e := channelExecutionStoragePresent(tx)
	if e != nil {
		return false, e
	}
	var protocols []ChannelAdmissionExecutionProtocol
	if relay {
		if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=7").Find(&protocols).Error; e != nil {
			return false, e
		}
	}
	if count == 0 && len(protocols) == 0 {
		return false, nil
	}
	if !base || count != 1 || len(protocols) != 1 || protocols[0].Version != 1 {
		return false, ErrChannelAdmissionUnavailable
	}
	statement := &gorm.Statement{DB: tx}
	if e = statement.Parse(&ChannelAdmissionImagePollHandoff{}); e != nil {
		return false, e
	}
	rows, e := tx.Model(&ChannelAdmissionImagePollHandoff{}).Select(statement.Schema.DBNames).Limit(0).Rows()
	if e != nil {
		return false, e
	}
	re := rows.Err()
	ce := rows.Close()
	if re != nil {
		return false, re
	}
	return true, ce
}
func imagePollHistoryBinding(tx *gorm.DB, c *ChannelAdmissionControl, use *ChannelAdmissionUse, task *ImageTask, executor ChannelExecutorIdentity, proof VerifiedDeploymentProof, proofErr error) (nativeExecutionBinding, error) {
	var rows []ChannelAdmissionImagePollHandoff
	if e := tx.Where("use_id=?", use.ID).Find(&rows).Error; e != nil {
		return nativeExecutionBinding{}, e
	}
	binding := nativeExecutionBinding{}
	for _, r := range rows {
		if r.Version != 1 || r.UseHash != executionUseHash(use) || r.CoreHash != imageRecoveryCoreHash(task) || r.StorageGeneration != c.StorageGeneration || !fullIdentityDigest(r.AuthorizationHash) || !fullIdentityDigest(r.TaskHash) || !fullIdentityDigest(r.UsageHash) || !fullIdentityDigest(r.ClaimOwner) || r.ClaimID != imageAdmissionResource(task) || r.ClaimHash != imagePollClaimHash(r) || !validImageRecoveryHandoffState(r.ChannelAdmissionImageRecoveryHandoff) {
			return nativeExecutionBinding{}, ErrChannelAdmissionConflict
		}
		if e := validateImageRecoveryUseFacts(tx, task, use, r.UseFactsJSON); e != nil {
			return nativeExecutionBinding{}, e
		}
		if _, e := nativeExecutionHistoricalProof(tx, c, imageRecoveryProofRow(r.ChannelAdmissionImageRecoveryHandoff)); e != nil {
			return nativeExecutionBinding{}, e
		}
		if r.Epoch == c.Epoch && r.ServiceID == executor.ServiceID && r.InstanceID == executor.InstanceID && r.BuildSHA == executor.BuildSHA {
			binding = nativeExecutionBinding{source: "durable", proofJSON: r.ExecutorProofJSON}
		}
	}
	if binding.source != "" {
		return binding, nil
	}
	if len(rows) > 0 && proofErr != nil {
		return nativeExecutionBinding{}, ErrChannelAdmissionUnavailable
	}
	return frozenImageRecoveryBinding(tx, c, use, task, executor, proof, proofErr)
}

// Claim and offer are inseparable when coverage is available. The original
// three-minute proposal/duration is retained; no new TTL is introduced.
func tryClaimImagePoll(ctx context.Context, info *AsyncUsageInfo, token string, leaseUntil, now time.Time) (handled, won bool, err error) {
	if info.ImageTaskID == "" {
		return false, false, nil
	}
	present, e := imagePollStoragePresent(LogDB.WithContext(ctx))
	if e != nil {
		return true, false, e
	}
	if !present {
		return false, false, nil
	}
	store, e := channelAdmissionRuntimeStore(LogDB)
	if e != nil {
		return true, false, e
	}
	if store == nil {
		return true, false, ErrChannelAdmissionUnavailable
	}
	var proof VerifiedDeploymentProof
	proofErr := ErrChannelAdmissionUnavailable
	var prior int64
	resource := imageAdmissionResource(&ImageTask{ID: info.ImageTaskID, GroupID: info.GroupID, TokenID: info.TokenID})
	if e := LogDB.WithContext(ctx).Model(&ChannelAdmissionImagePollHandoff{}).Where("claim_id=? AND epoch=? AND service_id=? AND instance_id=? AND build_sha=?", resource, store.Executor.Epoch, store.Executor.ServiceID, store.Executor.InstanceID, store.Executor.BuildSHA).Count(&prior).Error; e != nil {
		return true, false, e
	}
	if prior == 0 {
		proof, proofErr = store.deployment(ctx)
	}
	nonce, e := newAdmissionGeneration()
	if e != nil {
		return true, false, e
	}
	var handle *imagePollHandle
	err = store.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		present, e := imagePollStoragePresent(tx)
		if e != nil {
			return e
		}
		if !present {
			return ErrChannelAdmissionUnavailable
		}
		var task ImageTask
		if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&task, "id=?", info.ImageTaskID).Error; e != nil {
			return e
		}
		var u AsyncUsageInfo
		if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&u, "id=?", info.ID).Error; e != nil {
			return e
		}
		if imagePollUsageHash(&u) != imagePollUsageHash(info) || task.UsageID != u.ID || task.GroupID != u.GroupID || task.TokenID != u.TokenID || task.BillingOperationID != u.BillingOperationID {
			return ErrImagePollClaimLost
		}
		at, e := channelAdmissionTime(tx)
		if e != nil {
			return e
		}
		if u.Status != AsyncUsageStatusPending || (!u.NextPollAt.IsZero() && u.NextPollAt.After(at)) {
			return nil
		}
		if !leaseUntil.After(at) {
			return ErrImagePollClaimLost
		}
		leaseUntil = leaseUntil.UTC().Truncate(time.Microsecond)
		claimed := tx.Model(&AsyncUsageInfo{}).Where("id=?", u.ID).Updates(map[string]any{"processing_token": token, "next_poll_at": leaseUntil, "updated_at": at})
		if claimed.Error != nil {
			return claimed.Error
		}
		if claimed.RowsAffected != 1 {
			return ErrImagePollClaimLost
		}
		// Bind the value actually stored by the existing timestamp column,
		// including its precision, instead of treating the proposal as a receipt.
		var stored AsyncUsageInfo
		if e = tx.First(&stored, "id=?", u.ID).Error; e != nil {
			return e
		}
		if stored.Status != AsyncUsageStatusPending || stored.ProcessingToken != token || imagePollUsageHash(&stored) != imagePollUsageHash(&u) || stored.NextPollAt.IsZero() {
			return ErrImagePollClaimLost
		}
		leaseUntil = stored.NextPollAt
		inactive, e := channelAdmissionNeverActivated(tx, c)
		if e != nil {
			return e
		}
		if !inactive {
			use, e := imageReservationUse(tx, &task)
			if e != nil {
				return e
			}
			if use != nil {
				task.AdmissionGeneration = use.ID
				executor := store.Executor
				if executor.Epoch != c.Epoch || executor.ServiceID == "" || executor.InstanceID == "" || !fullIdentityDigest(executor.BuildSHA) {
					return ErrChannelAdmissionUnavailable
				}
				binding, e := imagePollHistoryBinding(tx, c, use, &task, executor, proof, proofErr)
				if e != nil {
					return e
				}
				if binding.source != "" {
					facts, e := captureImageRecoveryUseFacts(tx, &task, use)
					if e != nil {
						return e
					}
					owner, _ := admissionHash([]any{"image-poll-token-v1", token})
					id, _ := admissionHash([]any{"image-poll-id-v1", use.ID, u.ID, owner, nonce})
					r := ChannelAdmissionImagePollHandoff{ChannelAdmissionImageRecoveryHandoff: ChannelAdmissionImageRecoveryHandoff{ID: id, Version: 1, UseID: use.ID, UseHash: executionUseHash(use), CoreHash: imageRecoveryCoreHash(&task), ContractHash: imageRecoveryContractHash(&task), TaskHash: imagePollTaskHash(&task), ClaimID: imageAdmissionResource(&task), ClaimOwner: owner, ClaimUntilMicros: leaseUntil.UnixMicro(), Epoch: c.Epoch, StorageGeneration: c.StorageGeneration, ServiceID: executor.ServiceID, InstanceID: executor.InstanceID, BuildSHA: executor.BuildSHA, ExecutorProofJSON: binding.proofJSON, UseFactsJSON: facts, State: "offered", ExecutionState: "active", ContinuationState: "unproven", OfferedAtMs: at.UnixMilli()}, UsageID: u.ID, UsageHash: imagePollUsageHash(&u)}
					r.ClaimHash = imagePollClaimHash(r)
					r.AuthorizationHash = imagePollAuth(r, nonce)
					if !validImageRecoveryHandoffState(r.ChannelAdmissionImageRecoveryHandoff) {
						return ErrChannelAdmissionConflict
					}
					if e = tx.Create(&r).Error; e != nil {
						return e
					}
					if binding.source == "snapshot" {
						if e = finalizeAdmissionProof(tx, c, proof, c.Revision); e != nil {
							return e
						}
					}
					handle = &imagePollHandle{store: store, row: r, nonce: nonce, token: token, taskID: task.ID}
				}
			} else {
				var prior int64
				if e = tx.Model(&ChannelAdmissionImagePollHandoff{}).Where("claim_id=?", imageAdmissionResource(&task)).Count(&prior).Error; e != nil {
					return e
				}
				if prior != 0 {
					return ErrChannelAdmissionConflict
				}
			}
		}
		at, e = channelAdmissionTime(tx)
		if e != nil {
			return e
		}
		if !leaseUntil.After(at) {
			return ErrImagePollClaimLost
		}
		won = true
		return nil
	}, true)
	if err != nil {
		return true, false, err
	}
	if won {
		info.ProcessingToken = token
		info.NextPollAt = leaseUntil
		info.UpdatedAt = now
		info.imagePoll = handle
	}
	return true, won, nil
}

// Locks control before task/usage/journal. Business callers may re-read these
// same locked rows and take logs afterward, never acquire control inside them.
func (h *imagePollHandle) transaction(ctx context.Context, allowReleased bool, work func(*gorm.DB, *ChannelAdmissionControl, *ChannelAdmissionImagePollHandoff, *AsyncUsageInfo) error) error {
	if h == nil || h.store == nil || h.nonce == "" {
		return ErrChannelAdmissionConflict
	}
	return h.store.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		present, e := imagePollStoragePresent(tx)
		if e != nil {
			return e
		}
		if !present {
			return ErrChannelAdmissionUnavailable
		}
		var task ImageTask
		if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&task, "id=?", h.taskID).Error; e != nil {
			return e
		}
		var u AsyncUsageInfo
		if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&u, "id=?", h.row.UsageID).Error; e != nil {
			return e
		}
		var r ChannelAdmissionImagePollHandoff
		if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&r, "id=?", h.row.ID).Error; e != nil {
			return e
		}
		use, e := imageReservationUse(tx, &task)
		if e != nil {
			return e
		}
		if use == nil {
			return ErrChannelAdmissionConflict
		}
		task.AdmissionGeneration = use.ID
		if r.Version != 1 || r.AuthorizationHash != h.row.AuthorizationHash || r.AuthorizationHash != imagePollAuth(r, h.nonce) || r.ClaimHash != imagePollClaimHash(r) || r.TaskHash != imagePollTaskHash(&task) || r.UsageHash != imagePollUsageHash(&u) || r.UseID != use.ID || r.UseHash != executionUseHash(use) || r.StorageGeneration != c.StorageGeneration || !validImageRecoveryHandoffState(r.ChannelAdmissionImageRecoveryHandoff) {
			return ErrChannelAdmissionConflict
		}
		if e = validateImageRecoveryUseFacts(tx, &task, use, r.UseFactsJSON); e != nil {
			return e
		}
		if _, e = nativeExecutionHistoricalProof(tx, c, imageRecoveryProofRow(r.ChannelAdmissionImageRecoveryHandoff)); e != nil {
			return e
		}
		deadline := u.NextPollAt.UnixMicro()
		if r.ReleaseJSON != "" {
			var receipt imagePollRelease
			if !allowReleased || r.ReleaseHash != imagePollReleaseHash(r, h.nonce) || json.Unmarshal([]byte(r.ReleaseJSON), &receipt) != nil || receipt.DeadlineMicros < r.ClaimUntilMicros || receipt.Operation != u.BillingOperationID {
				return ErrImagePollClaimLost
			}
			deadline = receipt.DeadlineMicros
		} else if u.Status != AsyncUsageStatusPending || u.ProcessingToken != h.token || deadline < r.ClaimUntilMicros {
			return ErrImagePollClaimLost
		}
		at, e := channelAdmissionTime(tx)
		if e != nil {
			return e
		}
		if deadline <= at.UnixMicro() {
			return ErrImagePollClaimLost
		}
		if e = work(tx, c, &r, &u); e != nil {
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
func (h *imagePollHandle) matches(info *AsyncUsageInfo) bool {
	return info != nil && info.ID == h.row.UsageID && info.ImageTaskID == h.taskID && info.ProcessingToken == h.token && imagePollUsageHash(info) == h.row.UsageHash
}

// Legacy receipt APIs lack a context argument. A successfully accepted private
// invocation supplies its immutable parent; unscoped calls retain compatibility.
func imagePollParentContext(info *AsyncUsageInfo) context.Context {
	if info != nil && info.imagePoll != nil && info.imagePoll.parentCtx != nil {
		return info.imagePoll.parentCtx
	}
	return context.Background()
}
func imagePollTransaction(ctx context.Context, info *AsyncUsageInfo, work func(*gorm.DB, *imageWalletClaimContext) error) error {
	if info == nil || info.imagePoll == nil {
		return LogDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return work(tx, nil) }, claimTxOptions(LogDB)...)
	}
	h := info.imagePoll
	if !h.matches(info) {
		return ErrImagePollClaimLost
	}
	return h.transaction(ctx, false, func(tx *gorm.DB, c *ChannelAdmissionControl, r *ChannelAdmissionImagePollHandoff, u *AsyncUsageInfo) error {
		if r.State != "accepted" {
			return ErrChannelAdmissionConflict
		}
		originalDeadline := u.NextPollAt.UnixMicro()
		if e := work(tx, &imageWalletClaimContext{h.store, c}); e != nil {
			return e
		}
		var after AsyncUsageInfo
		if e := tx.First(&after, "id=?", u.ID).Error; e != nil {
			return e
		}
		if after.ProcessingToken != h.token || after.Status != AsyncUsageStatusPending {
			if after.ProcessingToken != "" {
				return ErrImagePollClaimLost
			}
			var task ImageTask
			if e := tx.First(&task, "id=?", h.taskID).Error; e != nil {
				return e
			}
			receipt := imagePollRelease{DeadlineMicros: originalDeadline, Status: after.Status, NextPollAt: after.NextPollAt, Operation: after.BillingOperationID, Failed: after.Status == AsyncUsageStatusFailed || after.Error != "", TerminalCancel: after.Status == AsyncUsageStatusFailed && task.Status == "failed" && task.Error != nil && task.Error.Code == AsyncGenerationTimeoutCode}
			raw, e := json.Marshal(receipt)
			if e != nil {
				return e
			}
			r.ReleaseJSON = string(raw)
			r.ReleaseHash = imagePollReleaseHash(*r, h.nonce)
			result := tx.Model(&ChannelAdmissionImagePollHandoff{}).Where("id=? AND state=? AND release_json=?", r.ID, "accepted", "").Updates(map[string]any{"release_json": r.ReleaseJSON, "release_hash": r.ReleaseHash})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrChannelAdmissionConflict
			}
		}
		return nil
	})
}

// No private handle means compatibility without invented historical coverage.
func AcceptImagePoll(ctx context.Context, info *AsyncUsageInfo) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if info == nil {
		return false, ErrImagePollClaimLost
	}
	if info.imagePoll == nil {
		if info.ImageTaskID != "" {
			if _, err := imagePollStoragePresent(LogDB.WithContext(ctx)); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	if !info.imagePoll.matches(info) {
		return false, ErrImagePollClaimLost
	}
	accepted := false
	e := info.imagePoll.transaction(ctx, false, func(tx *gorm.DB, _ *ChannelAdmissionControl, r *ChannelAdmissionImagePollHandoff, _ *AsyncUsageInfo) error {
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
		result := tx.Model(&ChannelAdmissionImagePollHandoff{}).Where("id=? AND state=?", r.ID, "offered").Updates(map[string]any{"state": "accepted", "accepted_at_ms": at.UnixMilli()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrChannelAdmissionConflict
		}
		accepted = true
		return nil
	})
	if accepted && e == nil {
		info.imagePoll.parentCtx = ctx
	}
	return accepted && e == nil, e
}

// Call only after the worker and renewal goroutine have normally returned.
// Panic/Goexit do not reach this call. Expired old records remain active.
func RecordImagePollReturn(ctx context.Context, info *AsyncUsageInfo) error {
	if info == nil || info.imagePoll == nil {
		return nil
	}
	if !info.imagePoll.matches(info) {
		return ErrImagePollClaimLost
	}
	return info.imagePoll.transaction(ctx, true, func(tx *gorm.DB, _ *ChannelAdmissionControl, r *ChannelAdmissionImagePollHandoff, _ *AsyncUsageInfo) error {
		if r.State != "accepted" {
			return ErrChannelAdmissionConflict
		}
		state := "returned"
		if r.ReleaseJSON != "" {
			var receipt imagePollRelease
			if e := json.Unmarshal([]byte(r.ReleaseJSON), &receipt); e != nil {
				return e
			}
			if receipt.Failed {
				state = "error_returned"
			}
		}
		at, e := channelAdmissionTime(tx)
		if e != nil {
			return e
		}
		result := tx.Model(&ChannelAdmissionImagePollHandoff{}).Where("id=? AND state=?", r.ID, "accepted").Updates(map[string]any{"state": "exited", "execution_state": state, "exited_at_ms": at.UnixMilli()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrChannelAdmissionConflict
		}
		return nil
	})
}

// Only a winning timeout release grants bounded cancel cleanup. The wallet's
// independent two-minute lease is never substituted for this original deadline.
func ValidateImagePollTerminalCleanup(ctx context.Context, info *AsyncUsageInfo) error {
	if info == nil {
		return ErrImagePollClaimLost
	}
	if info.imagePoll == nil {
		return nil
	}
	if !info.imagePoll.matches(info) {
		return ErrImagePollClaimLost
	}
	return info.imagePoll.transaction(ctx, true, func(_ *gorm.DB, _ *ChannelAdmissionControl, r *ChannelAdmissionImagePollHandoff, _ *AsyncUsageInfo) error {
		var receipt imagePollRelease
		if r.State != "accepted" || r.ReleaseJSON == "" || json.Unmarshal([]byte(r.ReleaseJSON), &receipt) != nil || !receipt.TerminalCancel {
			return ErrImagePollClaimLost
		}
		return nil
	})
}

func RenewImagePollClaim(ctx context.Context, info *AsyncUsageInfo, until time.Time) (bool, error) {
	if info == nil {
		return false, nil
	}
	if info.imagePoll == nil {
		return RenewAsyncUsageClaimContext(ctx, info.ID, info.ProcessingToken, until)
	}
	renewed := false
	e := imagePollTransaction(ctx, info, func(tx *gorm.DB, _ *imageWalletClaimContext) error {
		var e error
		renewed, e = renewAsyncUsageClaimTx(tx, info.ID, info.ProcessingToken, until)
		return e
	})
	if errors.Is(e, errAsyncUsageRenewalExpired) || errors.Is(e, ErrImagePollClaimLost) {
		return false, nil
	}
	return renewed && e == nil, e
}
