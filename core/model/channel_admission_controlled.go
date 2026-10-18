package model

import (
	"context"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"regexp"
)

// InstallControlledAdmissionRuntime is only for the private local bootstrap.
// Until independent migration intent/checkpoints are installed, ALL migration
// authorization and legacy Seal paths refuse. Normal task Prepare is unrelated.
// No restore closure exists: an established controlled process cannot go legacy.
func InstallControlledAdmissionRuntime(executor ChannelExecutorIdentity, load func(context.Context) (VerifiedDeploymentProof, error)) (*ChannelAdmissionStore, error) {
	if DB == nil || DB != LogDB || load == nil || executor.InstanceID == "" {
		return nil, ErrChannelAdmissionUnavailable
	}
	admissionRuntime.Store(&channelAdmissionRuntime{controlled: true, executor: executor, load: load})
	return &ChannelAdmissionStore{DB: DB, LogDB: LogDB, Executor: executor, LoadDeployment: load, controlledRuntime: true}, nil
}

// CheckControlledFreshStorage never writes or repairs storage. Only a reviewed,
// fully provisioned seed with no task, money, admission or migration history is
// accepted. It is not a recovery watermark and never licenses an existing DB.
func CheckControlledFreshStorage(db *gorm.DB) error {
	if db == nil || db.Dialector.Name() != "sqlite" || !ChannelAdmissionStorageReady(db) {
		return ErrChannelAdmissionUnavailable
	}
	for _, m := range []any{&Channel{}, &ModelConfig{}, &Group{}, &Token{}, &Option{}, &NativeTask{}, &ImageTask{}, &AsyncUsageInfo{}, &Log{}, &ChannelAdmissionExecution{}, &ChannelAdmissionExecutionProtocol{}, &ChannelAdmissionTaskExecution{}, &ChannelAdmissionDispatchHandoff{}, &ChannelAdmissionRecoveryHandoff{}, &ChannelAdmissionImageRecoveryClaim{}, &ChannelAdmissionImageRecoveryHandoff{}} {
		st := &gorm.Statement{DB: db}
		if st.Parse(m) != nil {
			return ErrChannelAdmissionUnavailable
		}
		rows, e := db.Model(m).Select(st.Schema.DBNames).Limit(0).Rows()
		if e != nil {
			return ErrChannelAdmissionUnavailable
		}
		rowErr := rows.Err()
		closeErr := rows.Close()
		if rowErr != nil || closeErr != nil {
			return ErrChannelAdmissionUnavailable
		}
	}
	var c ChannelAdmissionControl
	if e := db.First(&c, 1).Error; e != nil {
		return ErrChannelAdmissionUnavailable
	}
	if c.Version != ChannelAdmissionProtocolVersion || c.Epoch != "" || c.Revision != 0 || c.EpochSequence != 0 || c.ActiveProofID != "" || c.StorageGeneration != "" || c.MembershipHash != "" || c.ExecutorFenceHash != "" {
		return ErrChannelAdmissionConflict
	}
	// Every present business-history table must be empty. A partial protocol is
	// rejected below rather than filled by AutoMigrate.
	var tables []string
	if e := db.Raw("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&tables).Error; e != nil {
		return e
	}
	allowed := map[string]bool{"channels": true, "model_configs": true, "groups": true, "tokens": true, "options": true, "channel_admission_controls": true, "channel_admission_execution_protocols": true, "channel_migration_protocols": true, "channel_migration_shadow_protocols": true, "channel_migration_probe_runtime_protocols": true}
	for _, name := range tables {
		if !regexp.MustCompile(`^[A-Za-z0-9_]+$`).MatchString(name) {
			return ErrChannelAdmissionConflict
		}
		if !allowed[name] {
			var n int64
			if e := db.Table(name).Count(&n).Error; e != nil {
				return e
			}
			if n != 0 {
				return ErrChannelAdmissionConflict
			}
		}
	}
	if e := migrationStorageReady(db); e != nil {
		return e
	}
	return nil
}

// RevokeDeploymentProof updates ONLY the exact previously installed proof.
// The row lock is independent of control so a waiter observes revocation before
// final authorization. It does not claim remote execution has stopped.
func RevokeDeploymentProof(ctx context.Context, db *gorm.DB, id, factHash string) error {
	if db == nil || (db.Dialector.Name() != "sqlite" && db.Dialector.Name() != "postgres") || id == "" || !fullIdentityDigest(factHash) {
		return ErrChannelAdmissionUnavailable
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var p ChannelAdmissionProof
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&p, "id=?", id).Error; e != nil {
			return e
		}
		if p.FactHash != factHash || p.ConsumedSequence <= 0 {
			return ErrChannelAdmissionConflict
		}
		if p.Revoked {
			return nil
		}
		r := tx.Model(&ChannelAdmissionProof{}).Where("id=? AND fact_hash=? AND revoked=?", id, factHash, false).Update("revoked", true)
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected != 1 {
			return errors.New("controlled proof revoke conflict")
		}
		return nil
	}, claimTxOptions(db)...)
}
