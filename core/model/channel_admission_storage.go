package model

import (
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var (
	ErrChannelAdmissionUnavailable = errors.New("channel admission protection unavailable")
	ErrChannelAdmissionConflict    = errors.New("channel admission identity or version changed")
	ErrChannelAdmissionSealed      = errors.New("channel identity no longer accepts new work")
	ErrChannelAdmissionRetained    = errors.New("channel identity still required by recorded work")
)

const ChannelAdmissionProtocolVersion = 1

// The singleton serializes admissions, family grants and seals across processes.
// It is installed by reviewed SQL, never by request-time AutoMigrate. An epoch
// is activated only by the future deployment verifier, not by a client header.
type ChannelAdmissionControl struct {
	ID                int    `gorm:"primaryKey"`
	Version           int    `gorm:"not null"`
	Epoch             string `gorm:"size:128;not null"`
	Revision          int64  `gorm:"not null"`
	EpochSequence     int64  `gorm:"not null"`
	StorageGeneration string `gorm:"size:128;not null"`
	MembershipHash    string `gorm:"size:64;not null"`
	ExecutorFenceHash string `gorm:"size:64;not null"`
	ActiveProofID     string `gorm:"size:128;not null"`
}

// The local revocation/verified-fact record is independent of the global
// admission lock. A trusted updater may revoke it while a sealer waits for a
// channel lock. Final authorization locks this row and rechecks its facts.
// No production proof installation or activation API exists yet.
type ChannelAdmissionProof struct {
	ID               string `gorm:"primaryKey;size:128"`
	FactHash         string `gorm:"size:64;not null"`
	Revoked          bool   `gorm:"not null"`
	ConsumedSequence int64  `gorm:"not null"`
}

// A sealed digest family admits only its exact current successor (if any).
// Same-key migrations advance this grant without unsealing aliases; key
// rotation leaves the old digest family without a grant. Previous barriers
// remain immutable, so continuous migrations preserve every retired identity.
type ChannelAdmissionFamily struct {
	KeyDigest           string `gorm:"primaryKey;size:64"`
	AllowedChannelID    int    `gorm:"not null"`
	AllowedIdentityHash string `gorm:"size:64;not null"`
	Revision            int64  `gorm:"not null"`
	OperationID         string `gorm:"size:128;not null"`
}

type ChannelAdmissionBarrier struct {
	OperationID      string `gorm:"primaryKey;size:128"`
	RequestHash      string `gorm:"size:64;not null"`
	Epoch            string `gorm:"size:128;not null"`
	Version          int    `gorm:"not null"`
	Revision         int64  `gorm:"not null"`
	FromChannelID    int    `gorm:"index;not null"`
	FromKeyDigest    string `gorm:"size:64;not null"`
	FromIdentityHash string `gorm:"size:64;not null"`
	ToChannelID      int    `gorm:"not null"`
	ToKeyDigest      string `gorm:"size:64;not null"`
	ToIdentityHash   string `gorm:"size:64;not null"`
	RecordedAtMs     int64  `gorm:"not null"`
}

// An open or unknown use has no automatic expiry. A lost worker, unknown
// provider acceptance or missing async outbox can never turn into zero usage
// just because a lease or timer elapsed. Closure requires server-side evidence.
type ChannelAdmissionUse struct {
	ID           string `gorm:"primaryKey;size:64"`
	RequestHash  string `gorm:"size:64;not null"`
	Epoch        string `gorm:"size:128;not null"`
	ChannelID    int    `gorm:"index;not null"`
	KeyDigest    string `gorm:"size:64;index;not null"`
	IdentityHash string `gorm:"size:64;not null"`
	Lane         string `gorm:"size:32;not null"`
	State        string `gorm:"size:16;index;not null"`
	AdmittedAtMs int64  `gorm:"not null"`
	ClosedAtMs   int64  `gorm:"not null"`
	ClosureHash  string `gorm:"size:64;not null"`
}

func ChannelAdmissionStorageReady(db *gorm.DB) bool {
	if db == nil {
		return false
	}
	for _, table := range []any{&ChannelAdmissionControl{}, &ChannelAdmissionProof{}, &ChannelAdmissionFamily{}, &ChannelAdmissionBarrier{}, &ChannelAdmissionUse{}} {
		statement := &gorm.Statement{DB: db}
		if statement.Parse(table) != nil {
			return false
		}
		rows, err := db.Session(&gorm.Session{Logger: logger.Discard}).Model(table).Select(statement.Schema.DBNames).Limit(0).Rows()
		if err != nil {
			return false
		}
		rowErr := rows.Err()
		closeErr := rows.Close()
		if rowErr != nil || closeErr != nil {
			return false
		}
	}
	return true
}
