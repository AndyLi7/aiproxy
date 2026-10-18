package model

import (
	"context"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"sort"
)

// Ordinary routing writes still work. Physical identity edits/deletes share
// the admission control lock so they cannot race a newly accepted task or a
// seal. Migration history retains physical rows even after their uses close.
func persistChannelPatchWithAdmission(channel *Channel, patch *ChannelPatch) error {
	if err := shadowOrdinaryWriteReady(DB); err != nil {
		return err
	}
	store, err := channelAdmissionRuntimeStore(LogDB)
	if err != nil {
		return err
	}
	if store == nil {
		return DB.Transaction(func(tx *gorm.DB) error {
			present, e := channelRoutingStorageReady(tx)
			if e != nil {
				return e
			}
			if present {
				if _, e = lockRoutingChannel(tx, channel.ID); e != nil {
					return e
				}
			}
			return persistChannelPatch(tx, channel, patch)
		})
	}
	return store.transaction(context.Background(), func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		inactive, err := channelAdmissionNeverActivated(tx, c)
		if err != nil {
			return err
		}
		var current Channel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ?", channel.ID).Error; err != nil {
			return err
		}
		if err := guardPreparedChannelMutation(tx, []int{channel.ID}); err != nil {
			return err
		}
		if patch.emptiesKeyOfEnabled(&current) {
			return ErrChannelKeyEmpty
		}
		if !inactive && patch.changesCredentials(&current) {
			if err := guardAdmissionChannelMutation(tx, []int{channel.ID}); err != nil {
				return err
			}
		}
		return persistChannelPatch(tx, channel, patch)
	}, true)
}

func deleteChannelsWithAdmission(ids []int, single bool) error {
	if err := shadowOrdinaryWriteReady(DB); err != nil {
		return err
	}
	write := func(tx *gorm.DB) error {
		protected, e := channelRoutingStorageReady(tx)
		if e != nil {
			return e
		}
		if protected {
			ordered := append([]int{}, ids...)
			sort.Ints(ordered)
			for _, id := range ordered {
				if _, e = lockRoutingChannel(tx, id); e != nil {
					return e
				}
				if e = bumpChannelRoutingRevision(tx, id); e != nil {
					return e
				}
			}
		}
		result := tx.Where("id IN ?", ids).Delete(&Channel{})
		if single {
			return HandleUpdateResult(result, ErrChannelNotFound)
		}
		return result.Error
	}
	store, err := channelAdmissionRuntimeStore(LogDB)
	if err != nil {
		return err
	}
	if store == nil {
		return DB.Transaction(write)
	}
	ids = append([]int(nil), ids...)
	sort.Ints(ids)
	return store.transaction(context.Background(), func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		inactive, err := channelAdmissionNeverActivated(tx, c)
		if err != nil {
			return err
		}
		var channels []Channel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id IN ?", ids).Order("id").Find(&channels).Error; err != nil {
			return err
		}
		if err := guardOwnedShadowMutation(tx, ids); err != nil {
			return err
		}
		if !inactive {
			if err := guardAdmissionChannelMutation(tx, ids); err != nil {
				return err
			}
		}
		return write(tx)
	}, true)
}

func guardAdmissionChannelMutation(tx *gorm.DB, ids []int) error {
	if err := guardPreparedChannelMutation(tx, ids); err != nil {
		return err
	}
	checks := []*gorm.DB{
		tx.Model(&ChannelAdmissionBarrier{}).Where("from_channel_id IN ? OR to_channel_id IN ?", ids, ids),
		tx.Model(&ChannelAdmissionUse{}).Where("channel_id IN ?", ids),
		tx.Model(&NativeTask{}).Where("channel_id IN ? AND (status NOT IN ? OR billing_settled = ? OR recovery_owner <> ? OR dispatch_owner <> ?)", ids, []string{"completed", "failed"}, false, "", ""),
		// Include every async row, not just rows already linked to an image task.
		tx.Model(&AsyncUsageInfo{}).Where("channel_id IN ? AND (status NOT IN ? OR processing_token <> ?)", ids, []AsyncUsageStatus{AsyncUsageStatusCompleted, AsyncUsageStatusFailed}, ""),
		tx.Model(&ImageTask{}).Where("usage_id IN (?) AND (status NOT IN ? OR billing_settled = ?)", tx.Model(&AsyncUsageInfo{}).Select("id").Where("channel_id IN ?", ids), []string{"completed", "failed"}, false),
	}
	for _, query := range checks {
		var count int64
		if err := query.Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return ErrChannelAdmissionRetained
		}
	}
	return nil
}
