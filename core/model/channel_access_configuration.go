package model

import (
	"context"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"unicode/utf8"
)

// Immutable creation receipt. Deleting a channel must not free its access ID.
// Install explicitly with the reviewed access-configuration SQL, not on requests.
type ChannelAccessConfiguration struct {
	AccessID     string `gorm:"primaryKey;size:128"`
	RequestHash  string `gorm:"not null;size:64"`
	ChannelID    int    `gorm:"not null;index"`
	KeyDigest    string `gorm:"not null;size:64"`
	IdentityHash string `gorm:"not null;size:64"`
}
type AccessConfigurationReceipt struct {
	AccessID string          `json:"accessId"`
	Identity ChannelIdentity `json:"identity"`
}

func accessReceipt(r ChannelAccessConfiguration) AccessConfigurationReceipt {
	return AccessConfigurationReceipt{r.AccessID, ChannelIdentity{r.ChannelID, r.KeyDigest, r.IdentityHash}}
}
func ReadAccessConfiguration(ctx context.Context, id string) (AccessConfigurationReceipt, error) {
	var row ChannelAccessConfiguration
	if !ValidMigrationReadOperationID(id) {
		return AccessConfigurationReceipt{}, ErrChannelAdmissionConflict
	}
	e := DB.WithContext(ctx).First(&row, "access_id=?", id).Error
	return accessReceipt(row), e
}

// The ID reservation, physical channel and immutable receipt commit together.
// ON CONFLICT serializes duplicate/retried requests, including a lost HTTP reply.
// A replay never changes routing, status, credentials, or billing history.
func EnsureAccessConfiguration(ctx context.Context, id string, ch *Channel) (out AccessConfigurationReceipt, err error) {
	if !ValidMigrationReadOperationID(id) || ch == nil || ch.ID != 0 || ch.Status != ChannelStatusEnabled || channelKeyEmpty(ch.Key) || len(ch.Name) == 0 || utf8.RuneCountInString(ch.Name) > 64 || len(ch.Models) != 0 || len(ch.ModelMapping) != 0 {
		return out, ErrChannelAdmissionConflict
	}
	copy := *ch
	copy.ID = 1 // Identity hash excludes the physical ID.
	identity, e := ChannelIdentityOf(&copy)
	if e != nil {
		return out, e
	}
	hash, e := admissionHash(identity.IdentityHash)
	if e != nil {
		return out, e
	}
	err = routingWriteTransaction(ctx, func(tx *gorm.DB) error {
		ready, e := channelRoutingStorageReady(tx)
		if e != nil || !ready {
			return ErrChannelAdmissionUnavailable
		}
		// The zero channel is never visible outside this transaction.
		row := ChannelAccessConfiguration{AccessID: id, RequestHash: hash, ChannelID: 0, KeyDigest: identity.KeyDigest, IdentityHash: identity.IdentityHash}
		inserted := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "access_id"}}, DoNothing: true}).Create(&row)
		if inserted.Error != nil {
			return inserted.Error
		}
		if inserted.RowsAffected == 0 {
			if e = tx.First(&row, "access_id=?", id).Error; e != nil {
				return e
			}
			if row.RequestHash != hash {
				return ErrChannelAdmissionConflict
			}
			var live Channel
			if e = tx.First(&live, row.ChannelID).Error; e != nil {
				return ErrChannelAdmissionConflict
			}
			actual, e := ChannelIdentityOf(&live)
			if e != nil || actual != accessReceipt(row).Identity {
				return ErrChannelAdmissionConflict
			}
			out = accessReceipt(row)
			return nil
		}
		if e = tx.Create(ch).Error; e != nil {
			return e
		}
		if e = initializeChannelRoutingState(tx, []int{ch.ID}); e != nil {
			return e
		}
		row.ChannelID = ch.ID
		if e = tx.Model(&ChannelAccessConfiguration{}).Where("access_id=?", id).UpdateColumn("channel_id", ch.ID).Error; e != nil {
			return e
		}
		out = accessReceipt(row)
		return nil
	})
	// No routes exist at creation, but refresh metadata caches too.
	if err == nil {
		rebuildModelCachesAfterWrite()
	}
	return out, err
}
func accessConfigurationOwnsChannel(tx *gorm.DB, id int) bool {
	var row ChannelAccessConfiguration
	e := tx.First(&row, "channel_id=?", id).Error
	return e == nil && !errors.Is(e, gorm.ErrRecordNotFound)
}
