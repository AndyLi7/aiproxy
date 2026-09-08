package model

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm/clause"
)

// RequestTraceNonce retains only a digest, not the signed envelope or raw nonce.
type RequestTraceNonce struct {
	Digest    string    `gorm:"primaryKey;size:64"`
	ExpiresAt time.Time `gorm:"index"`
}

func (RequestTraceNonce) TableName() string { return "request_trace_nonces" }

func (s *TraceStore) ClaimTraceNonce(ctx context.Context, digest string, expires time.Time) (bool, error) {
	if s == nil || s.db == nil || ctx == nil {
		return false, errors.New("trace nonce store unavailable")
	}
	decoded, err := hex.DecodeString(digest)
	now := time.Now()
	if err != nil || len(decoded) != 32 || strings.ToLower(digest) != digest || !expires.After(now) || expires.After(now.Add(3*time.Minute)) {
		return false, errors.New("invalid trace nonce claim")
	}
	result := s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&RequestTraceNonce{Digest: digest, ExpiresAt: expires.UTC()})
	return result.Error == nil && result.RowsAffected == 1, result.Error
}

func (s *TraceStore) CleanTraceNonces(ctx context.Context, now time.Time, limit int) (int64, error) {
	if s == nil || s.db == nil || ctx == nil || now.IsZero() || limit < 1 || limit > 500 {
		return 0, errors.New("invalid trace nonce cleanup")
	}
	var digests []string
	if err := s.db.WithContext(ctx).Model(&RequestTraceNonce{}).Where("expires_at < ?", now.UTC()).Order("expires_at, digest").Limit(limit).Pluck("digest", &digests).Error; err != nil {
		return 0, err
	}
	if len(digests) == 0 {
		return 0, nil
	}
	result := s.db.WithContext(ctx).Where("digest IN ? AND expires_at < ?", digests, now.UTC()).Delete(&RequestTraceNonce{})
	return result.RowsAffected, result.Error
}
