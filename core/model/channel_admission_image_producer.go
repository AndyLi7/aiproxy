package model

import (
	"context"
	"encoding/json"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ChannelAdmissionImageProducer covers the frontend invocation, not upstream
// completion or any poll/billing continuation. Active has no TTL.
type ChannelAdmissionImageProducer struct {
	ID                string `gorm:"primaryKey;size:128"`
	ParentID          string `gorm:"not null;size:128"`
	Version           int    `gorm:"not null"`
	Kind              string `gorm:"not null;size:32"`
	UseID             string `gorm:"not null;size:128;uniqueIndex"`
	UseHash           string `gorm:"not null;size:128"`
	TaskID            string `gorm:"not null;size:128"`
	UsageID           int    `gorm:"not null"`
	CoreHash          string `gorm:"not null;size:128"`
	AttemptIndex      int    `gorm:"not null"`
	Epoch             string `gorm:"not null;size:128"`
	StorageGeneration string `gorm:"not null;size:128"`
	ServiceID         string `gorm:"not null;size:128"`
	InstanceID        string `gorm:"not null;size:128"`
	BuildSHA          string `gorm:"not null;size:128"`
	AuthorizationHash string `gorm:"not null;size:128"`
	State             string `gorm:"not null;size:32"`
	ContinuationState string `gorm:"not null;size:32"`
	StartedAtMs       int64  `gorm:"not null"`
	ReturnedAtMs      int64  `gorm:"not null"`
	ReceiptJSON       string `gorm:"not null"`
	ReceiptHash       string `gorm:"not null;size:128"`
}

type imageProducerHandle struct {
	store         *ChannelAdmissionStore
	nonce, rootID string
	// Appended only by the synchronous submitter after the attempt commits.
	rows map[string]ChannelAdmissionImageProducer
}

type imageProducerReceipt struct {
	Kind               string `json:"Kind"`
	Facts              string `json:"Facts"`
	UsageID            int    `json:"UsageID"`
	ReleaseClosureHash string `json:"ReleaseClosureHash"`
}

func imageProducerAuth(r ChannelAdmissionImageProducer, nonce string) string {
	r.AuthorizationHash = ""
	r.State = "active"
	r.ReturnedAtMs = 0
	r.ReceiptJSON = ""
	r.ReceiptHash = ""
	h, _ := admissionHash([]any{"image-producer-v1", r, nonce})

	return h
}

func imageProducerReceiptHash(r ChannelAdmissionImageProducer, nonce string) string {
	h, _ := admissionHash(
		[]any{"image-producer-receipt-v1", r.ID, r.AuthorizationHash, r.ReceiptJSON, nonce},
	)
	return h
}

func imageProducerStoragePresent(tx *gorm.DB) (bool, error) {
	var (
		count int64
		err   error
	)
	switch tx.Name() {
	case "sqlite":
		err = tx.Raw("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", "channel_admission_image_producers").
			Scan(&count).
			Error
	case "postgres":
		err = tx.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name=?", "channel_admission_image_producers").
			Scan(&count).
			Error
	case "mysql":
		if err = tx.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?", "channel_admission_image_producers").
			Scan(&count).
			Error; err != nil {
			return false, err
		}

		if count != 0 {
			return false, ErrChannelAdmissionUnavailable
		}

		var protocols int64
		if err = tx.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?", "channel_admission_execution_protocols").
			Scan(&protocols).
			Error; err != nil {
			return false, err
		}

		if protocols != 0 {
			if err = tx.Model(&ChannelAdmissionExecutionProtocol{}).
				Where("id=8").
				Count(&protocols).
				Error; err != nil {
				return false, err
			}

			if protocols != 0 {
				return false, ErrChannelAdmissionUnavailable
			}
		}

		return false, nil
	default:
		return false, ErrChannelAdmissionUnavailable
	}

	if err != nil {
		return false, err
	}

	base, err := channelExecutionStoragePresent(tx)
	if err != nil {
		return false, err
	}

	var protocols []ChannelAdmissionExecutionProtocol
	if base {
		if err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id=8").
			Find(&protocols).
			Error; err != nil {
			return false, err
		}
	}

	if count == 0 && len(protocols) == 0 {
		return false, nil
	}

	if count != 1 || !base || len(protocols) != 1 || protocols[0].Version != 1 {
		return false, ErrChannelAdmissionUnavailable
	}

	statement := &gorm.Statement{DB: tx}
	if err = statement.Parse(&ChannelAdmissionImageProducer{}); err != nil {
		return false, err
	}

	rows, err := tx.Model(&ChannelAdmissionImageProducer{}).
		Select(statement.Schema.DBNames).
		Limit(0).
		Rows()
	if err != nil {
		return false, err
	}

	e := rows.Err()
	//nolint:sqlclosecheck // The LIMIT 0 schema probe must close before the next query on this transaction.
	closeErr := rows.Close()

	if e != nil {
		return false, e
	}

	return true, closeErr
}

func startImageProducer(
	tx *gorm.DB,
	c *ChannelAdmissionControl,
	s *ChannelAdmissionStore,
	task *ImageTask,
	use *ChannelAdmissionUse,
) (*imageProducerHandle, error) {
	present, e := imageProducerStoragePresent(tx)
	if e != nil || !present {
		return nil, e
	}

	nonce, e := newAdmissionGeneration()
	if e != nil {
		return nil, e
	}

	h := &imageProducerHandle{
		store: s,
		nonce: nonce,
		rows:  map[string]ChannelAdmissionImageProducer{},
	}

	row, e := h.newRow(tx, c, task, use, -1)
	if e != nil {
		return nil, e
	}

	h.rootID = row.ID
	h.rows[row.ID] = row

	return h, nil
}

func (h *imageProducerHandle) newRow(
	tx *gorm.DB,
	c *ChannelAdmissionControl,
	task *ImageTask,
	use *ChannelAdmissionUse,
	index int,
) (ChannelAdmissionImageProducer, error) {
	r := ChannelAdmissionImageProducer{}
	if use.Lane != "image" || use.State != "open" || c.StorageGeneration == "" {
		return r, ErrChannelAdmissionConflict
	}

	now, e := channelAdmissionTime(tx)
	if e != nil {
		return r, e
	}

	id, _ := admissionHash([]any{"image-producer-id-v1", h.nonce, use.ID})

	kind := "attempt"
	if index < 0 {
		kind = "root"
	}

	r = ChannelAdmissionImageProducer{
		ID:                id,
		ParentID:          h.rootID,
		Version:           1,
		Kind:              kind,
		UseID:             use.ID,
		UseHash:           executionUseHash(use),
		TaskID:            task.ID,
		UsageID:           task.UsageID,
		CoreHash:          imageRecoveryCoreHash(task),
		AttemptIndex:      index,
		Epoch:             c.Epoch,
		StorageGeneration: c.StorageGeneration,
		ServiceID:         h.store.Executor.ServiceID,
		InstanceID:        h.store.Executor.InstanceID,
		BuildSHA:          h.store.Executor.BuildSHA,
		State:             "active",
		ContinuationState: "unproven",
		StartedAtMs:       now.UnixMilli(),
	}
	r.AuthorizationHash = imageProducerAuth(r, h.nonce)

	return r, tx.Create(&r).Error
}

func (h *imageProducerHandle) lockRow(
	tx *gorm.DB,
	c *ChannelAdmissionControl,
	id string,
) (*ChannelAdmissionImageProducer, error) {
	original, ok := h.rows[id]
	if !ok {
		return nil, ErrChannelAdmissionConflict
	}

	var r ChannelAdmissionImageProducer
	if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&r, "id=?", id).Error; e != nil {
		return nil, e
	}

	if r.AuthorizationHash != original.AuthorizationHash ||
		r.AuthorizationHash != imageProducerAuth(r, h.nonce) ||
		r.StorageGeneration != c.StorageGeneration ||
		r.ContinuationState != "unproven" ||
		r.Version != 1 ||
		r.StartedAtMs <= 0 {
		return nil, ErrChannelAdmissionConflict
	}

	if r.ReceiptJSON == "" {
		if r.ReceiptHash != "" {
			return nil, ErrChannelAdmissionConflict
		}
	} else if r.ReceiptHash != imageProducerReceiptHash(r, h.nonce) {
		return nil, ErrChannelAdmissionConflict
	}

	var use ChannelAdmissionUse
	if e := tx.First(&use, "id=?", r.UseID).Error; e != nil {
		return nil, e
	}

	if use.Lane != "image" || executionUseHash(&use) != r.UseHash {
		return nil, ErrChannelAdmissionConflict
	}

	if use.State != "open" || use.ClosedAtMs != 0 || use.ClosureHash != "" {
		var receipt imageProducerReceipt
		if json.Unmarshal([]byte(r.ReceiptJSON), &receipt) != nil ||
			receipt.Kind != "reservation_released" ||
			r.Kind != "root" ||
			r.State == "active" ||
			use.State != "closed" ||
			use.ClosedAtMs <= 0 ||
			use.ClosureHash != receipt.ReleaseClosureHash {
			return nil, ErrChannelAdmissionConflict
		}
	}

	return &r, nil
}

func imageProducerAttempt(
	tx *gorm.DB,
	c *ChannelAdmissionControl,
	task *ImageTask,
	use *ChannelAdmissionUse,
	index int,
) (*ChannelAdmissionImageProducer, error) {
	present, e := imageProducerStoragePresent(tx)
	if e != nil {
		return nil, e
	}

	h := task.imageProducer
	if h == nil {
		return nil, imageProducerMissingSource(tx, task, present)
	} // Late install stays uncovered; existing covered roots cannot lose authority.

	if !present {
		return nil, ErrChannelAdmissionUnavailable
	}

	root, e := h.lockRow(tx, c, h.rootID)
	if e != nil {
		return nil, e
	}

	if root.State != "active" || root.ReceiptJSON != "" ||
		root.CoreHash != imageRecoveryCoreHash(task) {
		return nil, ErrChannelAdmissionConflict
	}

	row, e := h.newRow(tx, c, task, use, index)

	return &row, e
}

func imageProducerRecordReceipt(
	tx *gorm.DB,
	c *ChannelAdmissionControl,
	task *ImageTask,
	kind, releaseHash string,
) error {
	h := task.imageProducer

	present, e := imageProducerStoragePresent(tx)
	if e != nil {
		return e
	}

	if h == nil {
		return imageProducerMissingSource(tx, task, present)
	}

	if !present || c == nil {
		return ErrChannelAdmissionUnavailable
	}

	ids := make([]string, 1, 2)

	ids[0] = h.rootID
	if kind != "reservation_released" {
		var attemptID string
		for id, row := range h.rows {
			if row.Kind == "attempt" && row.UseID == task.AdmissionAttempt {
				attemptID = id
				break
			}
		}

		if attemptID == "" {
			return ErrChannelAdmissionConflict
		}

		ids = append(ids, attemptID)
	} else if len(h.rows) != 1 {
		return ErrChannelAdmissionConflict
	}

	for _, id := range ids {
		r, e := h.lockRow(tx, c, id)
		if e != nil {
			return e
		}

		if r.State != "active" || r.ReceiptJSON != "" || r.CoreHash != imageRecoveryCoreHash(task) {
			return ErrChannelAdmissionConflict
		}

		raw, e := json.Marshal(
			imageProducerReceipt{
				Kind:               kind,
				Facts:              imageSubmissionFacts(task),
				UsageID:            task.UsageID,
				ReleaseClosureHash: releaseHash,
			},
		)
		if e != nil {
			return e
		}

		r.ReceiptJSON = string(raw)
		r.ReceiptHash = imageProducerReceiptHash(*r, h.nonce)

		q := tx.Model(&ChannelAdmissionImageProducer{}).
			Where("id=? AND state=? AND receipt_json=? AND authorization_hash=?", r.ID, "active", "", r.AuthorizationHash).
			Updates(map[string]any{"receipt_json": r.ReceiptJSON, "receipt_hash": r.ReceiptHash})
		if q.Error != nil {
			return q.Error
		}

		if q.RowsAffected != 1 {
			return ErrChannelAdmissionConflict
		}
	}

	return nil
}

// RecordImageProducerReturn is called only after the actual frontend and its
// synchronous cleanup return normally. It never certifies backend completion.
func RecordImageProducerReturn(ctx context.Context, task *ImageTask, failed bool) error {
	if task == nil || task.imageProducer == nil {
		return nil
	}

	h := task.imageProducer

	return h.store.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		present, e := imageProducerStoragePresent(tx)
		if e != nil {
			return e
		}

		if !present {
			return ErrChannelAdmissionUnavailable
		}
		// Stable ordering; no task lock is needed or legal after a definite release.
		var rows []ChannelAdmissionImageProducer
		if e = tx.Where("id=? OR parent_id=?", h.rootID, h.rootID).
			Order("id").
			Find(&rows).
			Error; e != nil {
			return e
		}

		if len(rows) != len(h.rows) {
			return ErrChannelAdmissionConflict
		}

		now, e := channelAdmissionTime(tx)
		if e != nil {
			return e
		}

		for _, row := range rows {
			r, e := h.lockRow(tx, c, row.ID)
			if e != nil {
				return e
			}

			state := "returned"
			if failed {
				state = "error_returned"
			}

			if r.State == state && r.ReturnedAtMs >= r.StartedAtMs {
				continue
			}

			if r.State != "active" || r.ReturnedAtMs != 0 || now.UnixMilli() < r.StartedAtMs {
				return ErrChannelAdmissionConflict
			}

			var receipt imageProducerReceipt
			if r.ReceiptJSON != "" && json.Unmarshal([]byte(r.ReceiptJSON), &receipt) != nil {
				return ErrChannelAdmissionConflict
			}

			if receipt.Kind == "reservation_released" {
				if len(rows) != 1 || r.Kind != "root" || receipt.ReleaseClosureHash == "" {
					return ErrChannelAdmissionConflict
				}

				var count int64
				if e = tx.Model(&ImageTask{}).
					Where("id=?", r.TaskID).
					Count(&count).
					Error; e != nil {
					return e
				}

				if count != 0 {
					return ErrChannelAdmissionConflict
				}

				var use ChannelAdmissionUse
				if e = tx.First(&use, "id=?", r.UseID).Error; e != nil {
					return e
				}

				if receipt.ReleaseClosureHash != imageReleaseHash(use) {
					return ErrChannelAdmissionConflict
				}

				q := tx.Model(&ChannelAdmissionUse{}).
					Where("id=? AND state=? AND closure_hash=?", r.UseID, "open", "").
					Updates(map[string]any{"state": "closed", "closed_at_ms": now.UnixMilli(), "closure_hash": receipt.ReleaseClosureHash})
				if q.Error != nil {
					return q.Error
				}

				if q.RowsAffected != 1 {
					return ErrChannelAdmissionConflict
				}
			}

			q := tx.Model(&ChannelAdmissionImageProducer{}).
				Where("id=? AND state=? AND returned_at_ms=? AND authorization_hash=?", r.ID, "active", 0, r.AuthorizationHash).
				Updates(map[string]any{"state": state, "returned_at_ms": now.UnixMilli()})
			if q.Error != nil {
				return q.Error
			}

			if q.RowsAffected != 1 {
				return ErrChannelAdmissionConflict
			}
		}

		return nil
	}, true)
}

// A DB read may preserve public generation fields, but not the original actor.
func imageProducerMissingSource(tx *gorm.DB, task *ImageTask, present bool) error {
	if !present || task.AdmissionGeneration == "" {
		return nil
	}

	var count int64
	if e := tx.Model(&ChannelAdmissionImageProducer{}).
		Where("use_id=? AND kind=?", task.AdmissionGeneration, "root").
		Count(&count).
		Error; e != nil {
		return e
	}

	if count != 0 {
		return ErrChannelAdmissionConflict
	}

	return nil
}

// Lock producer rows before the final log lock; rechecking in the write phase
// only revisits locks already held by the same transaction.
func lockImageProducerReceipt(tx *gorm.DB, c *ChannelAdmissionControl, task *ImageTask) error {
	present, e := imageProducerStoragePresent(tx)
	if e != nil {
		return e
	}

	h := task.imageProducer
	if h == nil {
		return imageProducerMissingSource(tx, task, present)
	}

	if !present || c == nil {
		return ErrChannelAdmissionUnavailable
	}

	ids := make([]string, 1, 2)
	ids[0] = h.rootID

	var attempt string
	for id, row := range h.rows {
		if row.Kind == "attempt" && row.UseID == task.AdmissionAttempt {
			attempt = id
			break
		}
	}

	if attempt == "" {
		return ErrChannelAdmissionConflict
	}

	ids = append(ids, attempt)
	for _, id := range ids {
		row, e := h.lockRow(tx, c, id)
		if e != nil {
			return e
		}

		if row.State != "active" || row.ReceiptJSON != "" {
			return ErrChannelAdmissionConflict
		}
	}

	return nil
}
