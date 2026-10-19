package model

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"
)

// ClaimNativeRecovery leases one bounded page. It never returns a new submission
// to be sent to a provider: unknown outcomes are reconciliation-only. The owner
// argument labels a worker; the returned RecoveryOwner is a unique claim token
// that callers must retain for all completion and release operations.
func ClaimNativeRecovery(
	db *gorm.DB,
	owner string,
	now time.Time,
	limit int,
) ([]NativeTask, error) {
	if owner == "" || len(owner) > 64 || limit < 1 || limit > 100 {
		return nil, ErrNativeTaskConflict
	}

	var candidates []NativeTask

	err := db.Where("next_recovery_at <= ? AND recovery_until <= ? AND (status NOT IN ? OR billing_settled = ?)", now.Unix(), now.Unix(), []string{"completed", "failed"}, false).
		Order("next_recovery_at ASC, created_at ASC").
		Limit(limit).
		Find(&candidates).
		Error
	if err != nil {
		return nil, err
	}

	claimed := []NativeTask{}
	for _, task := range candidates {
		// Worker names and second-resolution deadlines can repeat after an
		// early release. The existing owner column carries a unique claim
		// capability, just as dispatch_owner does; callers retain this value.
		nonce, err := newAdmissionGeneration()
		if err != nil {
			return nil, err
		}

		claimOwner, err := admissionHash([]any{"native-recovery-claim-v1", owner, nonce})
		if err != nil {
			return nil, err
		}

		result := db.Model(&NativeTask{}).
			Where("id = ? AND group_id = ? AND token_id = ? AND created_at = ? AND recovery_until <= ? AND next_recovery_at <= ?", task.ID, task.GroupID, task.TokenID, task.CreatedAt, now.Unix(), now.Unix()).
			Updates(map[string]any{"recovery_owner": claimOwner, "recovery_until": now.Add(2 * time.Minute).Unix()})
		if result.Error != nil {
			return nil, result.Error
		}

		if result.RowsAffected == 1 {
			task.RecoveryOwner = claimOwner
			task.RecoveryUntil = now.Add(2 * time.Minute).Unix()
			claimed = append(claimed, task)
		}
	}

	return claimed, nil
}

// ReleaseNativeRecoveryAt releases the lease only while the task still has
// status, the state its next time was computed from: a queue move meanwhile
// (enqueued, claimed, returned) changes when recovery must look again, so the
// caller recomputes. released is false when the status changed. owner must be
// the original ClaimNativeRecovery result token, not the worker label.
func ReleaseNativeRecoveryAt(db *gorm.DB, id, owner, status string, next time.Time) (bool, error) {
	result := db.Model(&NativeTask{}).
		Where("id = ? AND recovery_owner = ? AND status = ?", id, owner, status).
		Updates(map[string]any{"recovery_owner": "", "recovery_until": 0, "next_recovery_at": next.Unix()})
	return result.RowsAffected == 1, result.Error
}

func ReleaseNativeRecovery(db *gorm.DB, id, owner string, next time.Time) error {
	result := db.Model(&NativeTask{}).
		Where("id = ? AND recovery_owner = ?", id, owner).
		Updates(map[string]any{"recovery_owner": "", "recovery_until": 0, "next_recovery_at": next.Unix()})
	if result.Error != nil {
		return result.Error
	}

	if result.RowsAffected != 1 {
		return ErrNativeTaskConflict
	}

	return nil
}

// SaveNativeBillingTerminal stores the billing flag of a wallet receipt and
// mirrors the task into its call row. It first reads the task under its row
// lock: the status before this sync decides whether a missing call row may be
// inserted (a refunded reservation is flipped to failed below, but never had
// a 202 answer), and the lock serialises it with RecordNativeTaskLog.
func SaveNativeBillingTerminal(db *gorm.DB, id, group string, token int, receipt string) error {
	var parsed struct {
		Status           string          `json:"status"`
		SubmissionStatus json.RawMessage `json:"submissionStatus"`
	}
	if json.Unmarshal([]byte(receipt), &parsed) != nil {
		return ErrNativeTaskConflict
	}
	// A hold the wallet still had queued (waiting) when it was refunded was
	// never sent: its reserved task reads as a queue timeout, never as a
	// provider that did not confirm (spec §6.5).
	var submission string

	_ = json.Unmarshal(parsed.SubmissionStatus, &submission)

	reservedCode := "submission_timeout"
	if submission == NativeStatusWaiting {
		reservedCode = QueueTimeoutCode
	}

	terminal := parsed.Status == "settled" || parsed.Status == "refunded" ||
		parsed.Status == "estimated"
	// Read outside the transaction: the key's name lives in the main database.
	tokenName := ""
	if NativeRecoveryRowNeeded(db, id, group, token, receipt) {
		tokenName = nativeRecoveryTokenName(token)
	}

	return db.Transaction(func(tx *gorm.DB) error {
		locked, err := lockNativeTask(tx, id, group, token)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNativeTaskConflict
		}

		if err != nil {
			return err
		}

		result := tx.Model(&NativeTask{}).
			Where("id = ? AND group_id = ? AND token_id = ?", id, group, token).
			Update("billing_settled", terminal)
		if result.Error != nil {
			return result.Error
		}

		if result.RowsAffected != 1 {
			return ErrNativeTaskConflict
		}

		if parsed.Status == "refunded" {
			if err := tx.Model(&NativeTask{}).
				Where("id = ? AND group_id = ? AND token_id = ? AND status = ?", id, group, token, "reserved").
				Updates(map[string]any{"status": "failed", "error_code": reservedCode}).
				Error; err != nil {
				return err
			}

			if err := tx.Model(&NativeTask{}).
				Where("id = ? AND group_id = ? AND token_id = ? AND status IN ?", id, group, token, []string{"submitting", "submission_unknown"}).
				Updates(map[string]any{"status": "failed", "error_code": "submission_timeout"}).
				Error; err != nil {
				return err
			}
			// A queued task the wallet refunded (its 2-hour backstop) was never
			// sent: it can no longer be claimed, and its customer reads a queue
			// failure, not a provider that did not confirm (spec §6.5).
			if err := tx.Model(&NativeTask{}).
				Where("id = ? AND group_id = ? AND token_id = ? AND upstream_id = ? AND status IN ?", id, group, token, "", []string{NativeStatusWaiting, NativeStatusDispatching}).
				Updates(map[string]any{"status": "failed", "error_code": QueueTimeoutCode, "updated_at": time.Now()}).
				Error; err != nil {
				return err
			}
		}

		return syncNativeTaskLog(tx, id, group, token, receipt, locked, tokenName)
	})
}

// ExtendNativeRecovery preserves the exact claim token while extending its
// existing live lease. It accepts the same interface used by long delivery,
// but evaluates database time only after the task lock, and again after the
// write against the ORIGINAL deadline so a delayed write cannot revive a lease.
func ExtendNativeRecovery(db *gorm.DB, id, owner string, until time.Time) error {
	if owner == "" || len(owner) > 64 || until.IsZero() {
		return ErrNativeTaskConflict
	}

	clock := ""
	switch db.Name() {
	case "postgres":
		clock = "EXTRACT(EPOCH FROM clock_timestamp())"
	case "sqlite":
		clock = "CAST(strftime('%s','now') AS INTEGER)"
	case "mysql":
		clock = "UNIX_TIMESTAMP()"
	default:
		return ErrNativeTaskConflict
	}

	return db.Transaction(func(tx *gorm.DB) error {
		query := tx.Model(&NativeTask{}).Where("id = ? AND recovery_owner = ?", id, owner)

		locked, err := lockNativeTaskTransition(query)
		if err != nil {
			return err
		}

		if !locked {
			return ErrNativeTaskConflict
		}

		var current NativeTask
		if err = tx.Where("id = ? AND recovery_owner = ?", id, owner).
			First(&current).
			Error; err != nil {
			return err
		}

		next := max(current.RecoveryUntil, until.Unix())
		if next != current.RecoveryUntil {
			result := tx.Model(&NativeTask{}).
				Where("id = ? AND recovery_owner = ? AND recovery_until = ?", id, owner, current.RecoveryUntil).
				Where("recovery_until > "+clock).
				UpdateColumn("recovery_until", next)
			if result.Error != nil {
				return result.Error
			}

			if result.RowsAffected != 1 {
				return ErrNativeTaskConflict
			}
		}
		// Check the former deadline, not the newly written future deadline.
		var valid int64
		if err = tx.Model(&NativeTask{}).
			Where("id = ? AND recovery_owner = ? AND recovery_until = ?", id, owner, next).
			Where("? > "+clock, current.RecoveryUntil).
			Count(&valid).
			Error; err != nil {
			return err
		}

		if valid != 1 {
			return ErrNativeTaskConflict
		}

		return nil
	}, claimTxOptions(db)...)
}

// WithNativeRecoveryOwner fences delivery mutations with the live lease token.
type nativeRecoveryOwnerKey struct{}

func WithNativeRecoveryOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, nativeRecoveryOwnerKey{}, owner)
}

func nativeRecoveryFence(db *gorm.DB) *gorm.DB {
	if owner, ok := db.Statement.Context.Value(nativeRecoveryOwnerKey{}).(string); ok {
		switch db.Name() {
		case "postgres":
			return db.Where(
				"recovery_owner = ? AND recovery_until > EXTRACT(EPOCH FROM clock_timestamp())",
				owner,
			)
		case "sqlite":
			return db.Where(
				"recovery_owner = ? AND recovery_until > CAST(strftime('%s','now') AS INTEGER)",
				owner,
			)
		case "mysql":
			return db.Where("recovery_owner = ? AND recovery_until > UNIX_TIMESTAMP()", owner)
		default:
			return db.Where("1 = 0")
		}
	}

	return db
}

func CheckNativeRecovery(db *gorm.DB, id string) error {
	var count int64
	if err := nativeRecoveryFence(
		db.Model(&NativeTask{}),
	).Where("id = ?", id).
		Count(&count).
		Error; err != nil {
		return err
	}

	if count != 1 {
		return ErrNativeTaskConflict
	}

	return nil
}

// ClaimNativeDelivery covers direct delivery too, sharing recovery's durable lease.
func ClaimNativeDelivery(
	db *gorm.DB,
	id, group string,
	token int,
	owner string,
	until time.Time,
) error {
	result := db.Model(&NativeTask{}).
		Where("id = ? AND group_id = ? AND token_id = ? AND recovery_until <= ? AND status IN ?", id, group, token, time.Now().Unix(), []string{"result_received", "delivery_ready"}).
		Updates(map[string]any{"recovery_owner": owner, "recovery_until": until.Unix()})
	if result.Error != nil {
		return result.Error
	}

	if result.RowsAffected != 1 {
		return ErrNativeTaskConflict
	}

	return nil
}

// WithNativeRecoveryGuard protects all receipt writes as one transaction. An
// ownerless legacy call keeps its existing behavior; a recovery/delivery caller
// must still own its live lease after the row lock and after synchronous writes.
func WithNativeRecoveryGuard(db *gorm.DB, id string, work func(*gorm.DB) error) error {
	if _, ok := db.Statement.Context.Value(nativeRecoveryOwnerKey{}).(string); !ok {
		return work(db)
	}

	return db.Transaction(func(tx *gorm.DB) error {
		locked, err := lockNativeTaskTransition(tx.Model(&NativeTask{}).Where("id = ?", id))
		if err != nil {
			return err
		}

		if !locked {
			return ErrNativeTaskConflict
		}

		if err = CheckNativeRecovery(tx, id); err != nil {
			return err
		}

		if err = work(tx); err != nil {
			return err
		}

		return CheckNativeRecovery(tx, id)
	}, claimTxOptions(db)...)
}
