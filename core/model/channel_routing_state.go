package model

import (
	"context"
	"encoding/json"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Opt-in additive storage. Never installed or backfilled by a request.
type ChannelRoutingState struct {
	ChannelID int   `gorm:"primaryKey;autoIncrement:false"`
	Version   int   `gorm:"not null"`
	Revision  int64 `gorm:"not null"`
}
type ChannelRoutingReceipt struct {
	RequestID     string `gorm:"primaryKey;size:128"`
	ChannelID     int    `gorm:"not null;index"`
	RequestHash   string `gorm:"not null;size:64"`
	RequestJSON   string `gorm:"not null;type:text"`
	BeforeJSON    string `gorm:"not null;type:text"`
	AfterJSON     string `gorm:"not null;type:text"`
	CommittedAtMs int64  `gorm:"not null"`
}

func routingStoragePresent(tx *gorm.DB) (bool, error) {
	if tx == nil {
		return false, ErrChannelAdmissionUnavailable
	}
	var n int64
	names := []string{"channel_routing_states", "channel_routing_receipts"}
	var q *gorm.DB
	switch tx.Dialector.Name() {
	case "sqlite":
		q = tx.Raw("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ?", names)
	case "postgres":
		q = tx.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name IN ?", names)
	case "mysql":
		q = tx.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN ?", names)
	default:
		return false, ErrChannelAdmissionUnavailable
	}
	e := q.Scan(&n).Error
	return n > 0, e
}
func channelRoutingStorageReady(tx *gorm.DB) (bool, error) {
	present, e := routingStoragePresent(tx)
	if e != nil || !present {
		return present, e
	}
	if tx.Dialector.Name() != "sqlite" && tx.Dialector.Name() != "postgres" {
		return true, ErrChannelAdmissionUnavailable
	}
	for _, q := range []*gorm.DB{
		tx.Model(&ChannelRoutingState{}).Select("channel_id, version, revision"),
		tx.Model(&ChannelRoutingReceipt{}).Select("request_id, channel_id, request_hash, request_json, before_json, after_json, committed_at_ms"),
	} {
		rows, e := q.Limit(0).Rows()
		if e != nil {
			return true, ErrChannelAdmissionUnavailable
		}
		rows.Close()
	}
	var n int64
	if e = tx.Model(&ChannelRoutingState{}).Where("version IS NULL OR version<>1 OR revision IS NULL OR revision<1").Count(&n).Error; e != nil || n != 0 {
		return true, ErrChannelAdmissionUnavailable
	}
	if e = tx.Raw("SELECT count(*) FROM channels c LEFT JOIN channel_routing_states r ON c.id=r.channel_id WHERE r.channel_id IS NULL").Scan(&n).Error; e != nil || n != 0 {
		return true, ErrChannelAdmissionUnavailable
	}
	return true, nil
}
func initializeChannelRoutingState(tx *gorm.DB, ids []int) error {
	present, e := routingStoragePresent(tx)
	if e != nil || !present {
		return e
	}
	for _, id := range ids {
		if e = tx.Create(&ChannelRoutingState{ChannelID: id, Version: 1, Revision: 1}).Error; e != nil {
			return e
		}
	}
	return nil
}

// Caller holds the physical channel row first; control/resource locks precede it
// when installed. Even a same-value old management write advances the revision.
func bumpChannelRoutingRevision(tx *gorm.DB, id int) error {
	present, e := channelRoutingStorageReady(tx)
	if e != nil || !present {
		return e
	}
	var row ChannelRoutingState
	if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "channel_id=?", id).Error; e != nil {
		return e
	}
	if row.Revision >= routingMaxSafe {
		return ErrChannelAdmissionUnavailable
	}
	r := tx.Model(&ChannelRoutingState{}).Where("channel_id=? AND revision=?", id, row.Revision).UpdateColumn("revision", row.Revision+1)
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected != 1 {
		return ErrChannelAdmissionConflict
	}
	return nil
}
func routingWriteTransaction(ctx context.Context, work func(*gorm.DB) error) error {
	if DB == nil {
		return ErrChannelAdmissionUnavailable
	}
	present, e := channelAdmissionTablesExist(DB.WithContext(ctx))
	if e != nil {
		return e
	}
	migration, e := migrationStoragePresent(DB.WithContext(ctx))
	if e != nil {
		return e
	}
	if present || migration {
		s := &ChannelAdmissionStore{DB: DB, LogDB: LogDB}
		return s.transaction(ctx, func(tx *gorm.DB, _ *ChannelAdmissionControl) error { return work(tx) }, true)
	}
	return DB.WithContext(ctx).Transaction(work)
}
func lockRoutingChannel(tx *gorm.DB, id int) (*Channel, error) {
	if tx.Dialector.Name() == "sqlite" {
		r := tx.Model(&Channel{}).Where("id=?", id).UpdateColumn("id", gorm.Expr("id"))
		if r.Error != nil {
			return nil, r.Error
		}
		if r.RowsAffected != 1 {
			return nil, ErrChannelAdmissionConflict
		}
	}
	var ch Channel
	if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&ch, id).Error; e != nil {
		return nil, e
	}
	return &ch, nil
}
func routingJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
