package model

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Activate is a server-only protocol operation using already verified external
// facts from LoadDeployment. There is no HTTP route or real verifier wired to
// it. It installs/changes the local epoch only if proof IDs are consumed once,
// sequences advance, storage generation matches and the independent recovery
// watermark is not ahead of the database. Normal Admit/Seal checks do not
// consume a proof again. A retry never refreshes an old proof's validity.
//
//nolint:gocyclo // Activation verifies the prior receipt, deployment membership, storage generation, and final epoch CAS atomically.
func (s *ChannelAdmissionStore) Activate(
	ctx context.Context,
	expectedSequence int64,
) (result *ChannelAdmissionControl, err error) {
	if expectedSequence < 0 {
		return nil, ErrChannelAdmissionConflict
	}

	proof, err := s.deployment(ctx)
	if err != nil {
		return nil, err
	}

	if proof.Sequence <= 0 || proof.Epoch == "" || len(proof.Epoch) > 128 ||
		proof.StorageGeneration == "" ||
		len(proof.StorageGeneration) > 128 {
		return nil, ErrChannelAdmissionUnavailable
	}

	hash, err := proof.Fingerprint()
	if err != nil {
		return nil, err
	}

	err = s.transaction(ctx, func(tx *gorm.DB, current *ChannelAdmissionControl) error {
		var existing ChannelAdmissionProof

		read := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&existing, "id = ?", proof.ProofID).
			Error
		if read != nil && !errors.Is(read, gorm.ErrRecordNotFound) {
			return read
		}

		if read == nil && existing.ConsumedSequence != 0 {
			if existing.FactHash != hash || existing.Revoked ||
				existing.ConsumedSequence != proof.Sequence ||
				current.ActiveProofID != proof.ProofID ||
				current.Epoch != proof.Epoch ||
				current.EpochSequence != proof.Sequence ||
				current.StorageGeneration != proof.StorageGeneration ||
				current.MembershipHash != proof.MembershipHash ||
				current.ExecutorFenceHash != proof.ExecutorFenceHash ||
				current.Revision < proof.StorageWatermark {
				return ErrChannelAdmissionConflict
			}
			// Only return the already-recorded activation. No new period, membership,
			// permission or journal revision is created by a response replay.
			result = current

			return nil
		}

		if current.EpochSequence != expectedSequence || proof.Sequence <= current.EpochSequence ||
			current.Revision < proof.StorageWatermark ||
			(current.StorageGeneration != "" && current.StorageGeneration != proof.StorageGeneration) {
			return ErrChannelAdmissionConflict
		}

		if read == nil && (existing.Revoked || existing.FactHash != hash) {
			return ErrChannelAdmissionConflict
		}

		record := ChannelAdmissionProof{
			ID:               proof.ProofID,
			FactHash:         hash,
			ConsumedSequence: proof.Sequence,
		}
		if errors.Is(read, gorm.ErrRecordNotFound) {
			if err := tx.Create(&record).Error; err != nil {
				return err
			}
		} else {
			update := tx.Model(&ChannelAdmissionProof{}).
				Where("id = ? AND consumed_sequence = ? AND revoked = ?", proof.ProofID, 0, false).
				Update("consumed_sequence", proof.Sequence)
			if update.Error != nil {
				return update.Error
			}

			if update.RowsAffected != 1 {
				return ErrChannelAdmissionConflict
			}
		}

		next := *current
		next.Epoch = proof.Epoch
		next.EpochSequence = proof.Sequence
		next.StorageGeneration = proof.StorageGeneration
		next.MembershipHash = proof.MembershipHash
		next.ExecutorFenceHash = proof.ExecutorFenceHash

		next.ActiveProofID = proof.ProofID
		if _, err := lockDeploymentProof(tx, &next, proof, s.Executor); err != nil {
			return err
		}

		clock := admissionClockSQL(tx)

		update := tx.Model(&ChannelAdmissionControl{}).
			Where("id = ? AND epoch_sequence = ? AND revision = ? AND storage_generation = ?", 1, current.EpochSequence, current.Revision, current.StorageGeneration).
			Where("EXISTS (SELECT 1 FROM channel_admission_proofs WHERE id = ? AND fact_hash = ? AND revoked = ? AND consumed_sequence = ?)", proof.ProofID, hash, false, proof.Sequence).
			Where(clock+" >= ? AND "+clock+" < ? AND "+clock+" < ?", proof.IssuedAtMs, proof.ValidUntilMs, proof.RevocationFreshUntilMs).
			Updates(map[string]any{"epoch": next.Epoch, "epoch_sequence": next.EpochSequence, "storage_generation": next.StorageGeneration, "membership_hash": next.MembershipHash, "executor_fence_hash": next.ExecutorFenceHash, "active_proof_id": next.ActiveProofID})
		if update.Error != nil {
			return update.Error
		}

		if update.RowsAffected != 1 {
			return ErrChannelAdmissionUnavailable
		}

		result = &next

		return nil
	}, true)
	if err != nil {
		return nil, err
	}

	return result, nil
}
