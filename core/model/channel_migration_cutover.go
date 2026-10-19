package model

import (
	"context"
	"math"

	"gorm.io/gorm"
)

// CutoverPrepared atomically seals old admission and commits the reviewed full
// pin batch and target enable. Only a forward, still-prepared operation may
// start here. Historical sealed completion and reverse require their separate
// reviewed authorization; this API never adopts either. The narrow probe HTTP
// adapter additionally binds the original task and prepared receipt in this transaction.
func (s *ChannelAdmissionStore) CutoverPrepared(
	ctx context.Context,
	input ChannelMigrationRequest,
) (result *ChannelMigrationPrepared, err error) {
	if input.TargetResourceKind != "" || input.Seal.RevertOf != "" {
		return nil, ErrChannelAdmissionConflict
	}
	return s.cutoverPrepared(ctx, input, nil)
}

func (s *ChannelAdmissionStore) CutoverReversePrepared(
	ctx context.Context,
	input ChannelMigrationRequest,
) (*ChannelMigrationPrepared, error) {
	if !reverseMigration(input) {
		return nil, ErrChannelAdmissionConflict
	}
	return s.cutoverPrepared(ctx, input, nil)
}

func (s *ChannelAdmissionStore) cutoverPrepared(
	ctx context.Context,
	input ChannelMigrationRequest,
	guard func(*gorm.DB, *ChannelAdmissionControl) error,
) (result *ChannelMigrationPrepared, err error) {
	a, err := s.migrationAuthorization(ctx, input)
	if err != nil {
		return nil, err
	}

	hash, err := admissionHash(input.Seal)
	if err != nil {
		return nil, err
	}

	err = s.controlTransaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		// Keep resource-before-prepared order used by all command writers.
		if err := lockMigrationTarget(tx, input); err != nil {
			return err
		}

		if guard != nil {
			if err := guard(tx, c); err != nil {
				return err
			}
		}

		row, err := loadMigration(tx, input.Seal.OperationID)
		if err != nil {
			return err
		}

		if row.State == "committed" {
			// Replay consumes no revisions. Exact authorization, current head
			// and complete postconditions are checked by the existing helper.
			result, err = s.commitPreparedPinsTx(tx, c, a, row.SealRevision)
			if err == nil && guard != nil {
				err = guard(tx, c)
				if err == nil {
					_, err = lockDeploymentProof(tx, c, a.deployment, s.Executor)
				}

				if err == nil {
					err = finalizeMigrationProbe(tx, a.probe)
				}
			}

			return err
		}

		if row.State != "prepared" {
			return ErrChannelAdmissionConflict
		}

		if c.Revision > math.MaxInt64-2 {
			return ErrChannelAdmissionUnavailable
		}

		barrier, err := s.sealTx(tx, c, input.Seal, a, a.deployment, nil, hash)
		if err != nil {
			return err
		}
		// sealTx's proof CAS advances persistent control, not this local value.
		// Re-read inside the same held transaction before the second proof CAS.
		if err = tx.First(c, 1).Error; err != nil {
			return err
		}

		if c.Revision != barrier.Revision {
			return ErrChannelAdmissionConflict
		}

		result, err = s.commitPreparedPinsTx(tx, c, a, barrier.Revision)
		if err == nil && guard != nil {
			err = guard(tx, c)
			if err == nil {
				_, err = lockDeploymentProof(tx, c, a.deployment, s.Executor)
			}

			if err == nil {
				err = finalizeMigrationProbe(tx, a.probe)
			}
		}

		return err
	}, false, true)
	if err != nil {
		return nil, err
	}

	if s.DB == DB {
		rebuildModelCachesAfterWrite()
	}

	return result, nil
}
