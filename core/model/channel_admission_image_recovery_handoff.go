package model

import (
	"context"
	"encoding/json"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// One wallet recovery call's owned scope, never supplier work, unjoined
// goroutines, earlier claims or continuous coverage. No automatic use closure.
type ChannelAdmissionImageRecoveryHandoff struct {
	ID                string `gorm:"primaryKey;size:64"`
	Version           int    `gorm:"not null"`
	ClaimHash         string `gorm:"not null;size:64;uniqueIndex"`
	UseID             string `gorm:"not null;size:128"`
	UseHash           string `gorm:"not null;size:128"`
	CoreHash          string `gorm:"not null;size:128"`
	ContractHash      string `gorm:"not null;size:128"`
	TaskHash          string `gorm:"not null;size:128"`
	ClaimID           string `gorm:"not null;size:128"`
	ClaimOwner        string `gorm:"not null;size:128"`
	ClaimUntilMicros  int64  `gorm:"not null"`
	Epoch             string `gorm:"not null;size:128"`
	StorageGeneration string `gorm:"not null;size:128"`
	ServiceID         string `gorm:"not null;size:128"`
	InstanceID        string `gorm:"not null;size:128"`
	BuildSHA          string `gorm:"not null;size:128"`
	ExecutorProofJSON string `gorm:"not null"`
	UseFactsJSON      string `gorm:"not null"`
	AuthorizationHash string `gorm:"not null;size:128"`
	State             string `gorm:"not null;size:128"`
	ExecutionState    string `gorm:"not null;size:128"`
	ContinuationState string `gorm:"not null;size:128"`
	OfferedAtMs       int64  `gorm:"not null"`
	AcceptedAtMs      int64  `gorm:"not null"`
	ExitedAtMs        int64  `gorm:"not null"`
}

type ImageRecoveryHandoffHandle struct {
	claim     *imageBillingRecoveryClaim
	task      *ImageTask
	id, nonce string
}
type imageRecoveryUseFact struct {
	ID        string `json:"ID"`
	Index     int    `json:"Index"`
	Hash      string `json:"Hash"`
	Persisted bool   `json:"Persisted"`
}

// Only selected_provider_binding is route-specific; the rest is the frozen
// public/provider contract. The full selected contract remains scope-bound.
func imageRecoveryCoreHash(task *ImageTask) string {
	contract := task.ValidationContract

	var object map[string]json.RawMessage
	if contract != "" && json.Unmarshal([]byte(contract), &object) == nil {
		delete(object, "selected_provider_binding")

		if raw, err := json.Marshal(object); err == nil {
			contract = string(raw)
		}
	}

	hash, _ := admissionHash(
		[]any{
			"image-recovery-core-v1",
			task.ID,
			task.GroupID,
			task.TokenID,
			task.CreatedAt.UnixMicro(),
			task.UsageID,
			task.Fingerprint,
			task.BillingOperationID,
			task.Model,
			task.RequestModel,
			contract,
			task.ExpectedImages,
			task.PrepaymentQuoteJSON,
		},
	)

	return hash
}

func imageRecoveryContractHash(task *ImageTask) string {
	hash, _ := admissionHash([]any{"image-recovery-selected-contract-v1", task.ValidationContract})
	return hash
}

func imageRecoveryHandoffClaim(row ChannelAdmissionImageRecoveryHandoff) string {
	hash, _ := admissionHash(
		[]any{"image-recovery-handoff-claim-v1", row.UseID, row.ClaimID, row.ClaimOwner},
	)
	return hash
}

func imageRecoveryHandoffAuth(row ChannelAdmissionImageRecoveryHandoff, nonce string) string {
	row.AuthorizationHash = ""
	row.State = "offered"
	row.ExecutionState = "active"
	row.AcceptedAtMs = 0
	row.ExitedAtMs = 0
	hash, _ := admissionHash([]any{"image-recovery-handoff-v1", row, nonce})

	return hash
}

func imageRecoveryProofRow(
	row ChannelAdmissionImageRecoveryHandoff,
) *ChannelAdmissionTaskExecution {
	return &ChannelAdmissionTaskExecution{
		Epoch:             row.Epoch,
		StorageGeneration: row.StorageGeneration,
		ServiceID:         row.ServiceID,
		InstanceID:        row.InstanceID,
		BuildSHA:          row.BuildSHA,
		ExecutorProofJSON: row.ExecutorProofJSON,
	}
}

func imageRecoveryHandoffStoragePresent(tx *gorm.DB) (bool, error) {
	var (
		count int64
		err   error
	)
	switch tx.Name() {
	case "sqlite":
		err = tx.Raw("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", "channel_admission_image_recovery_handoffs").
			Scan(&count).
			Error
	case "postgres":
		err = tx.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name=?", "channel_admission_image_recovery_handoffs").
			Scan(&count).
			Error
	default:
		return false, ErrChannelAdmissionUnavailable
	}

	if err != nil {
		return false, err
	}

	base, err := imageRecoveryClaimStoragePresent(tx)
	if err != nil {
		return false, err
	}

	if !base {
		if count != 0 {
			return false, ErrChannelAdmissionUnavailable
		}

		relay, err := channelExecutionStoragePresent(tx)
		if err != nil {
			return false, err
		}

		if relay {
			var n int64
			if err = tx.Model(&ChannelAdmissionExecutionProtocol{}).
				Where("id=6").
				Count(&n).
				Error; err != nil {
				return false, err
			}

			if n != 0 {
				return false, ErrChannelAdmissionUnavailable
			}
		}

		return false, nil
	}

	var protocols []ChannelAdmissionExecutionProtocol
	if err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id=6").
		Find(&protocols).
		Error; err != nil {
		return false, err
	}

	if count == 0 && len(protocols) == 0 {
		return false, nil
	}

	if count != 1 || len(protocols) != 1 || protocols[0].Version != 1 {
		return false, ErrChannelAdmissionUnavailable
	}

	statement := &gorm.Statement{DB: tx}
	if err = statement.Parse(&ChannelAdmissionImageRecoveryHandoff{}); err != nil {
		return false, err
	}

	rows, err := tx.Model(&ChannelAdmissionImageRecoveryHandoff{}).
		Select(statement.Schema.DBNames).
		Limit(0).
		Rows()
	if err != nil {
		return false, err
	}

	rowErr := rows.Err()
	//nolint:sqlclosecheck // The LIMIT 0 schema probe must close before the next query on this transaction.
	closeErr := rows.Close()

	if rowErr != nil {
		return false, rowErr
	}

	return true, closeErr
}

// Validate immutable admission facts, not current provider credentials: this
// scope only reconciles the wallet. Later attempts are not retroactive coverage.
func validateImageRecoveryUseFacts(
	tx *gorm.DB,
	task *ImageTask,
	root *ChannelAdmissionUse,
	raw string,
) error {
	var facts []imageRecoveryUseFact
	if json.Unmarshal([]byte(raw), &facts) != nil || len(facts) == 0 {
		return ErrChannelAdmissionConflict
	}

	seen := map[int]bool{}
	for _, fact := range facts {
		if fact.Index < -1 || seen[fact.Index] || !fullIdentityDigest(fact.Hash) {
			return ErrChannelAdmissionConflict
		}

		seen[fact.Index] = true

		var use ChannelAdmissionUse
		if err := tx.First(&use, "id = ?", fact.ID).Error; err != nil {
			return err
		}

		if use.Lane != "image" || use.State != "open" || use.ClosedAtMs != 0 ||
			use.ClosureHash != "" ||
			executionUseHash(&use) != fact.Hash {
			return ErrChannelAdmissionConflict
		}

		if fact.Index == -1 {
			if use.ID != root.ID || use.ResourceKey != imageAdmissionResource(task) ||
				use.RequestHash != imageAdmissionRequestHash(task) {
				return ErrChannelAdmissionConflict
			}
		} else {
			identity := ChannelIdentity{
				ChannelID:    use.ChannelID,
				KeyDigest:    use.KeyDigest,
				IdentityHash: use.IdentityHash,
			}
			if (fact.Persisted && fact.Index >= len(task.Attempts)) ||
				(fact.Index < len(task.Attempts) && task.Attempts[fact.Index].ChannelID != use.ChannelID) {
				return ErrChannelAdmissionConflict
			}

			if !validChannelIdentity(identity) ||
				use.ResourceKey != imageAttemptResource(task, fact.Index) ||
				use.RequestHash != imageAttemptHash(task, fact.Index, identity) {
				return ErrChannelAdmissionConflict
			}
		}
	}

	if !seen[-1] {
		return ErrChannelAdmissionConflict
	}

	return nil
}

func captureImageRecoveryUseFacts(
	tx *gorm.DB,
	task *ImageTask,
	use *ChannelAdmissionUse,
) (string, error) {
	facts := []imageRecoveryUseFact{{ID: use.ID, Index: -1, Hash: executionUseHash(use)}}
	for i := 0; i <= len(task.Attempts); i++ {
		var rows []ChannelAdmissionUse
		if err := tx.Where("resource_key = ?", imageAttemptResource(task, i)).
			Find(&rows).
			Error; err != nil {
			return "", err
		}

		if len(rows) > 1 || (i < len(task.Attempts) && len(rows) != 1) {
			return "", ErrChannelAdmissionConflict
		}

		if len(rows) == 1 {
			facts = append(
				facts,
				imageRecoveryUseFact{
					ID:        rows[0].ID,
					Index:     i,
					Hash:      executionUseHash(&rows[0]),
					Persisted: i < len(task.Attempts),
				},
			)
		}
	}

	raw, err := json.Marshal(facts)
	if err != nil {
		return "", err
	}

	return string(raw), validateImageRecoveryUseFacts(tx, task, use, string(raw))
}

func validImageRecoveryHandoffState(row ChannelAdmissionImageRecoveryHandoff) bool {
	if row.OfferedAtMs <= 0 || row.ClaimUntilMicros <= row.OfferedAtMs*1000 ||
		row.ContinuationState != "unproven" {
		return false
	}

	switch row.State {
	case "offered":
		return row.ExecutionState == "active" && row.AcceptedAtMs == 0 && row.ExitedAtMs == 0
	case "accepted":
		return row.ExecutionState == "active" && row.AcceptedAtMs >= row.OfferedAtMs &&
			row.ExitedAtMs == 0
	case "exited":
		return (row.ExecutionState == "returned" || row.ExecutionState == "error_returned") &&
			row.ExitedAtMs >= row.AcceptedAtMs &&
			row.AcceptedAtMs >= row.OfferedAtMs
	}

	return false
}

//nolint:gocyclo // Historical recovery binding validates task, contract, admission use, and executor evidence as one fail-closed record.
func frozenImageRecoveryBinding(
	tx *gorm.DB,
	c *ChannelAdmissionControl,
	use *ChannelAdmissionUse,
	task *ImageTask,
	executor ChannelExecutorIdentity,
	proof VerifiedDeploymentProof,
	proofErr error,
) (nativeExecutionBinding, error) {
	var rows []ChannelAdmissionImageRecoveryHandoff
	if err := tx.Where("use_id = ?", use.ID).Find(&rows).Error; err != nil {
		return nativeExecutionBinding{}, err
	}

	binding := nativeExecutionBinding{}
	for _, row := range rows {
		if row.Version != 1 || row.UseHash != executionUseHash(use) ||
			row.CoreHash != imageRecoveryCoreHash(task) ||
			row.StorageGeneration != c.StorageGeneration ||
			!fullIdentityDigest(row.AuthorizationHash) ||
			!fullIdentityDigest(row.TaskHash) ||
			!fullIdentityDigest(row.ContractHash) ||
			!fullIdentityDigest(row.ClaimOwner) ||
			row.ClaimID != imageAdmissionResource(task) ||
			row.ClaimHash != imageRecoveryHandoffClaim(row) ||
			!validImageRecoveryHandoffState(row) {
			return nativeExecutionBinding{}, ErrChannelAdmissionConflict
		}

		if err := validateImageRecoveryUseFacts(tx, task, use, row.UseFactsJSON); err != nil {
			return nativeExecutionBinding{}, err
		}

		if err := nativeExecutionHistoricalProof(tx, c, imageRecoveryProofRow(row)); err != nil {
			return nativeExecutionBinding{}, err
		}

		if row.Epoch == c.Epoch && row.ServiceID == executor.ServiceID &&
			row.InstanceID == executor.InstanceID &&
			row.BuildSHA == executor.BuildSHA {
			binding = nativeExecutionBinding{source: "durable", proofJSON: row.ExecutorProofJSON}
		}
	}

	if binding.source != "" {
		return binding, nil
	}

	var stored ChannelAdmissionProof
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		First(&stored, "id = ?", c.ActiveProofID).
		Error; err != nil {
		return nativeExecutionBinding{}, err
	}

	if stored.Revoked || stored.ConsumedSequence != c.EpochSequence ||
		!fullIdentityDigest(stored.FactHash) {
		return nativeExecutionBinding{}, ErrChannelAdmissionUnavailable
	}

	if proofErr != nil {
		hash, _ := proof.Fingerprint()

		empty, _ := (VerifiedDeploymentProof{}).Fingerprint()
		if hash != empty || errors.Is(proofErr, ErrChannelAdmissionConflict) ||
			errors.Is(proofErr, ErrChannelAdmissionSealed) ||
			len(rows) != 0 ||
			use.Epoch != c.Epoch {
			return nativeExecutionBinding{}, ErrChannelAdmissionUnavailable
		}

		return nativeExecutionBinding{}, nil
	}

	if _, err := lockDeploymentProof(tx, c, proof, executor); err != nil {
		return nativeExecutionBinding{}, err
	}

	raw, err := json.Marshal(proof)
	if err != nil {
		return nativeExecutionBinding{}, err
	}

	return nativeExecutionBinding{source: "snapshot", proofJSON: string(raw)}, nil
}

// Prepare runs before the owned Sync call, after its real claim. No claim
// capability (e.g. late installation) means no retrospective registration.
//
//nolint:gocyclo // Claim validation, frozen binding, dispatch ownership, and handoff persistence remain within one transaction.
func PrepareImageRecoveryHandoff(
	ctx context.Context,
	task *ImageTask,
) (*ImageRecoveryHandoffHandle, error) {
	if task == nil {
		return nil, ErrChannelAdmissionConflict
	}

	store, err := channelAdmissionRuntimeStore(LogDB)
	if err != nil || store == nil {
		return nil, err
	}

	present, err := imageRecoveryHandoffStoragePresent(LogDB)
	if err != nil || !present {
		return nil, err
	}

	claim := task.imageRecoveryClaim
	if claim == nil {
		return nil, nil
	}

	if claim.handoffPrepared.Load() {
		return nil, ErrChannelAdmissionConflict
	}

	store = claim.store

	var prior int64
	if err = store.DB.WithContext(ctx).
		Model(&ChannelAdmissionImageRecoveryHandoff{}).
		Where("use_id = ? AND epoch = ? AND service_id = ? AND instance_id = ? AND build_sha = ?", claim.row.UseID, store.Executor.Epoch, store.Executor.ServiceID, store.Executor.InstanceID, store.Executor.BuildSHA).
		Count(&prior).
		Error; err != nil {
		return nil, err
	}

	var proof VerifiedDeploymentProof

	proofErr := ErrChannelAdmissionUnavailable
	if prior == 0 {
		proof, proofErr = store.deployment(ctx)
	}

	nonce, err := newAdmissionGeneration()
	if err != nil {
		return nil, err
	}

	var handle *ImageRecoveryHandoffHandle

	err = claim.withClaimControl(
		ctx,
		task,
		false,
		func(tx *gorm.DB, c *ChannelAdmissionControl, current *ImageTask) error {
			present, e := imageRecoveryHandoffStoragePresent(tx)
			if e != nil {
				return e
			}

			if !present {
				return ErrChannelAdmissionUnavailable
			}

			use, e := imageReservationUse(tx, current)
			if e != nil {
				return e
			}

			if use == nil {
				// Missing historical reservation evidence cannot become an
				// uncovered legacy task, even after its sidecar was overwritten.
				var prior int64
				if e = tx.Model(&ChannelAdmissionImageRecoveryHandoff{}).
					Where("claim_id = ?", claim.row.ID).
					Count(&prior).
					Error; e != nil {
					return e
				}

				if prior != 0 {
					return ErrChannelAdmissionConflict
				}

				return nil
			}

			current.AdmissionGeneration = use.ID
			if e = sameImageWalletSnapshot(current, task); e != nil {
				return e
			}

			if current.ValidationContract != task.ValidationContract ||
				imageRecoveryCoreHash(current) != imageRecoveryCoreHash(task) {
				return ErrChannelAdmissionConflict
			}

			executor := store.Executor
			if executor.Epoch != c.Epoch || executor.ServiceID == "" ||
				len(executor.ServiceID) > 128 ||
				executor.InstanceID == "" ||
				len(executor.InstanceID) > 128 ||
				!fullIdentityDigest(executor.BuildSHA) {
				return ErrChannelAdmissionUnavailable
			}

			binding, e := frozenImageRecoveryBinding(tx, c, use, current, executor, proof, proofErr)
			if e != nil {
				return e
			}

			if binding.source == "" {
				return nil
			}

			facts, e := captureImageRecoveryUseFacts(tx, current, use)
			if e != nil {
				return e
			}

			now, e := channelAdmissionTime(tx)
			if e != nil {
				return e
			}

			id, _ := admissionHash(
				[]any{"image-recovery-handoff-id-v1", use.ID, claim.row.Owner, nonce},
			)
			row := ChannelAdmissionImageRecoveryHandoff{
				ID:                id,
				Version:           1,
				UseID:             use.ID,
				UseHash:           executionUseHash(use),
				CoreHash:          imageRecoveryCoreHash(current),
				ContractHash:      imageRecoveryContractHash(current),
				TaskHash:          claim.row.TaskHash,
				ClaimID:           claim.row.ID,
				ClaimOwner:        claim.row.Owner,
				ClaimUntilMicros:  claim.row.UntilMicros,
				Epoch:             c.Epoch,
				StorageGeneration: c.StorageGeneration,
				ServiceID:         executor.ServiceID,
				InstanceID:        executor.InstanceID,
				BuildSHA:          executor.BuildSHA,
				ExecutorProofJSON: binding.proofJSON,
				UseFactsJSON:      facts,
				State:             "offered",
				ExecutionState:    "active",
				ContinuationState: "unproven",
				OfferedAtMs:       now.UnixMilli(),
			}
			row.ClaimHash = imageRecoveryHandoffClaim(row)

			row.AuthorizationHash = imageRecoveryHandoffAuth(row, nonce)
			if !validImageRecoveryHandoffState(row) {
				return ErrChannelAdmissionConflict
			}

			if e = tx.Create(&row).Error; e != nil {
				return e
			}

			if binding.source == "snapshot" {
				if e = finalizeAdmissionProof(tx, c, proof, c.Revision); e != nil {
					return e
				}
			}

			handle = &ImageRecoveryHandoffHandle{claim: claim, task: task, id: id, nonce: nonce}

			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	if handle != nil {
		claim.handoffPrepared.Store(true)
	}

	return handle, nil
}

func (h *ImageRecoveryHandoffHandle) withHandoff(
	ctx context.Context,
	release bool,
	work func(*gorm.DB, *ChannelAdmissionImageRecoveryHandoff) error,
) error {
	if h == nil || h.claim == nil || h.task == nil || h.id == "" || h.nonce == "" {
		return ErrChannelAdmissionConflict
	}

	return h.claim.withClaimControl(
		ctx,
		h.task,
		release,
		func(tx *gorm.DB, c *ChannelAdmissionControl, current *ImageTask) error {
			present, err := imageRecoveryHandoffStoragePresent(tx)
			if err != nil {
				return err
			}

			if !present {
				return ErrChannelAdmissionUnavailable
			}

			var row ChannelAdmissionImageRecoveryHandoff
			if err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				First(&row, "id = ?", h.id).
				Error; err != nil {
				return err
			}

			use, err := imageReservationUse(tx, current)
			if err != nil {
				return err
			}

			if use == nil {
				return ErrChannelAdmissionConflict
			}

			current.AdmissionGeneration = use.ID
			if row.Version != 1 || row.UseID != use.ID || row.UseHash != executionUseHash(use) ||
				row.CoreHash != imageRecoveryCoreHash(current) ||
				row.ContractHash != imageRecoveryContractHash(current) ||
				row.TaskHash != h.claim.row.TaskHash ||
				row.ClaimID != h.claim.row.ID ||
				row.ClaimOwner != h.claim.row.Owner ||
				row.ClaimUntilMicros != h.claim.row.UntilMicros ||
				row.StorageGeneration != c.StorageGeneration ||
				row.AuthorizationHash != imageRecoveryHandoffAuth(row, h.nonce) ||
				row.ClaimHash != imageRecoveryHandoffClaim(row) ||
				!validImageRecoveryHandoffState(row) {
				return ErrChannelAdmissionConflict
			}

			if err = validateImageRecoveryUseFacts(tx, current, use, row.UseFactsJSON); err != nil {
				return err
			}

			if err = nativeExecutionHistoricalProof(
				tx,
				c,
				imageRecoveryProofRow(row),
			); err != nil {
				return err
			}

			return work(tx, &row)
		},
	)
}

func (h *ImageRecoveryHandoffHandle) Accept(ctx context.Context) (accepted bool, err error) {
	err = h.withHandoff(
		ctx,
		false,
		func(tx *gorm.DB, row *ChannelAdmissionImageRecoveryHandoff) error {
			if row.State == "accepted" {
				return nil
			}

			if row.State != "offered" {
				return ErrChannelAdmissionConflict
			}

			now, e := channelAdmissionTime(tx)
			if e != nil {
				return e
			}

			if now.UnixMilli() < row.OfferedAtMs {
				return ErrChannelAdmissionConflict
			}

			result := tx.Model(&ChannelAdmissionImageRecoveryHandoff{}).
				Where("id = ? AND state = ?", row.ID, "offered").
				Updates(map[string]any{"state": "accepted", "accepted_at_ms": now.UnixMilli()})
			if result.Error != nil {
				return result.Error
			}

			if result.RowsAffected != 1 {
				return ErrChannelAdmissionConflict
			}

			accepted = true

			return nil
		},
	)
	if err != nil {
		return false, err
	}

	return accepted, nil
}

// Only after observed normal owned work/cleanup completion. The existing claim
// transaction checks its pre-release deadline AFTER both release and exit writes.
func (h *ImageRecoveryHandoffHandle) ReleaseAndRecordExit(ctx context.Context, failed bool) error {
	return h.withHandoff(
		ctx,
		true,
		func(tx *gorm.DB, row *ChannelAdmissionImageRecoveryHandoff) error {
			if row.State != "accepted" {
				return ErrChannelAdmissionConflict
			}

			now, err := channelAdmissionTime(tx)
			if err != nil {
				return err
			}

			if now.UnixMilli() < row.AcceptedAtMs {
				return ErrChannelAdmissionConflict
			}

			if err = releaseImageRecoveryClaim(tx, h.claim.row); err != nil {
				return err
			}

			state := "returned"
			if failed {
				state = "error_returned"
			}

			result := tx.Model(&ChannelAdmissionImageRecoveryHandoff{}).
				Where("id = ? AND state = ?", row.ID, "accepted").
				Updates(map[string]any{"state": "exited", "execution_state": state, "exited_at_ms": now.UnixMilli()})
			if result.Error != nil {
				return result.Error
			}

			if result.RowsAffected != 1 {
				return ErrChannelAdmissionConflict
			}

			return nil
		},
	)
}
