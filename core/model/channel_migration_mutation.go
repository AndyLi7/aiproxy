package model

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	sqlitedriver "github.com/glebarez/go-sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func migrationStoragePresent(tx *gorm.DB) (bool, error) {
	var count int64
	var err error
	names := []string{"channel_migration_protocols", "channel_migration_prepareds", "channel_migration_probe_records", "channel_migration_shadow_protocols", "channel_migration_shadow_resources"}
	switch tx.Dialector.Name() {
	case "sqlite":
		err = tx.Raw("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ?", names).Scan(&count).Error
	case "postgres":
		err = tx.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name IN ?", names).Scan(&count).Error
	case "mysql":
		err = tx.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN ?", names).Scan(&count).Error
	default:
		return false, ErrChannelAdmissionUnavailable
	}
	return count > 0, err
}
func guardPreparedChannelMutation(tx *gorm.DB, ids []int) error {
	if err := guardRetiredTargetMutation(tx, ids); err != nil {
		return err
	}
	if err := guardOwnedShadowMutation(tx, ids); err != nil {
		return err
	}
	exists, err := migrationStoragePresent(tx)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if err := migrationStorageReady(tx); err != nil {
		return err
	}
	var count int64
	if err := tx.Model(&ChannelMigrationPrepared{}).Where("state IN ? AND (from_channel_id IN ? OR to_channel_id IN ?)", []string{"prepared", "sealed"}, ids, ids).Count(&count).Error; err != nil {
		return err
	}
	if count != 0 {
		return ErrChannelAdmissionConflict
	}
	return nil
}
func migrationStatesReady(tx *gorm.DB) error {
	var invalid int64
	if err := tx.Model(&ChannelMigrationPrepared{}).Where("state IS NULL OR state NOT IN ?", []string{"prepared", "sealed", "committed", "abandoned"}).Count(&invalid).Error; err != nil {
		return err
	}
	if invalid != 0 {
		return ErrChannelAdmissionUnavailable
	}
	return nil
}
func withMigrationChannelStatus(ctx context.Context, id, status int) error {
	write := func(tx *gorm.DB) error {
		if err := guardPreparedChannelMutation(tx, []int{id}); err != nil {
			return err
		}
		// Check the key again under the same transaction/lock as the write.
		if status == ChannelStatusEnabled {
			var ch Channel
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&ch, id).Error; err != nil {
				return err
			}
			if ch.Status != ChannelStatusEnabled && channelKeyEmpty(ch.Key) {
				return ErrChannelKeyEmpty
			}
		}
		return HandleUpdateResult(tx.Model(&Channel{}).Where("id = ?", id).Update("status", status), ErrChannelNotFound)
	}
	db := DB.WithContext(ctx)
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Re-read installation state on every fresh attempt. A newly installed
		// migration extension must immediately take the existing control path.
		exists, err := migrationStoragePresent(db)
		if err != nil {
			return err
		}
		if exists {
			store := &ChannelAdmissionStore{DB: DB, LogDB: LogDB}
			return store.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error { return write(tx) }, true)
		}
		// This closure is deliberately limited to DB guards/key checks and one
		// status update: never put network/cache/monitor callbacks in a retried tx.
		// A retry requires an explicitly successful rollback. BEGIN/COMMIT errors
		// and uncertain rollback never replay the write callback.
		retryReady := false
		err = db.Transaction(func(tx *gorm.DB) error {
			writeErr := write(tx)
			if db.Dialector.Name() == "sqlite" && sqliteStatusBusy(writeErr) {
				if rollbackErr := tx.Rollback().Error; rollbackErr != nil {
					return errors.Join(writeErr, rollbackErr)
				}
				retryReady = true
			}
			return writeErr
		})
		if err != nil && ctx.Err() != nil {
			return errors.Join(err, ctx.Err())
		}
		if !retryReady || !sqliteStatusBusy(err) || attempt+1 >= sqliteStatusAttempts {
			return err
		}
		wait := time.NewTimer(time.Duration(1<<attempt) * 2 * time.Millisecond)
		select {
		case <-ctx.Done():
			wait.Stop()
			return ctx.Err()
		case <-wait.C:
		}
	}
}
func withMigrationConfigWrite(configs []ModelConfig, deleted []string, write func(*gorm.DB) error) error {
	exists, err := migrationStoragePresent(DB)
	if err != nil {
		return err
	}
	if !exists {
		return DB.Transaction(write)
	}
	store := &ChannelAdmissionStore{DB: DB, LogDB: LogDB}
	return store.transaction(context.Background(), func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		if err := migrationStorageReady(tx); err != nil {
			return err
		}
		var preparations []ChannelMigrationPrepared
		if err := tx.Where("state IN ?", []string{"prepared", "sealed"}).Find(&preparations).Error; err != nil {
			return err
		}
		ids := map[int]bool{}
		for _, p := range preparations {
			ids[p.FromChannelID] = true
			ids[p.ToChannelID] = true
		}
		if err := addPendingRetiredIDs(tx, ids); err != nil {
			return err
		}
		if len(ids) == 0 {
			return write(tx)
		}
		models := append([]string{}, deleted...)
		for _, cfg := range configs {
			models = append(models, cfg.Model)
			raw, err := json.Marshal(cfg.Config)
			if err != nil {
				return err
			}
			if yes, err := migrationConfigReferences(raw, ids); err != nil {
				return err
			} else if yes {
				return ErrChannelAdmissionConflict
			}
		}
		rows, err := migrationRawRows(tx.Table("model_configs").Select("config").Where("model IN ?", models))
		if err != nil {
			return err
		}
		for _, row := range rows {
			raw, _ := row["config"].(string)
			if yes, err := migrationConfigReferences([]byte(raw), ids); err != nil {
				return err
			} else if yes {
				return ErrChannelAdmissionConflict
			}
		}
		return write(tx)
	}, true)
}
func migrationConfigReferences(raw []byte, ids map[int]bool) (bool, error) {
	if len(raw) == 0 {
		return false, nil
	}
	var cfg map[string]json.RawMessage
	if json.Unmarshal(raw, &cfg) != nil {
		return false, ErrChannelAdmissionConflict
	}
	if binding := cfg["x_token_platform_native_result_v1"]; binding != nil {
		var b struct {
			ChannelID int `json:"channelId"`
		}
		if json.Unmarshal(binding, &b) != nil {
			return false, ErrChannelAdmissionConflict
		}
		if ids[b.ChannelID] {
			return true, nil
		}
	}
	if quote := cfg["upstream_prepayment_v1"]; quote != nil {
		var q struct {
			Routes []struct {
				ChannelID int `json:"channelId"`
			} `json:"routes"`
		}
		if json.Unmarshal(quote, &q) != nil {
			return false, ErrChannelAdmissionConflict
		}
		for _, route := range q.Routes {
			if ids[route.ChannelID] {
				return true, nil
			}
		}
	}
	return false, nil
}

// Primary result code is the low byte; BUSY_SNAPSHOT/RECOVERY/TIMEOUT retain 5.
// Only the actual SQLite driver error qualifies, not a string/code lookalike.
const sqliteStatusAttempts = 8

func sqliteStatusBusy(err error) bool {
	var busy *sqlitedriver.Error
	return errors.As(err, &busy) && busy.Code()&0xff == 5
}
