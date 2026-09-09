package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/labring/aiproxy/core/common/requesttrace"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const maxPersistedSpansPerTrace = 256

var errRequestTraceOwnership = errors.New("request trace span ownership mismatch")

// RequestTraceHead holds the bounded aggregate state for one service's view of a trace.
type RequestTraceHead struct {
	TraceID   string               `gorm:"primaryKey;size:32"`
	Service   requesttrace.Service `gorm:"primaryKey;size:16"`
	GroupID   string               `gorm:"size:64;index"`
	SpanCount int
	Truncated bool
	UpdatedAt time.Time `gorm:"index:idx_request_trace_head_updated_at"`
}

func (RequestTraceHead) TableName() string {
	return "request_trace_heads"
}

// RequestTraceSpan is the safe, validated database projection of requesttrace.Span.
type RequestTraceSpan struct {
	SpanID       string `gorm:"primaryKey;size:32;index:idx_request_trace_scope,priority:4;index:idx_request_trace_request_scope,priority:5"`
	Version      int
	TraceID      string               `gorm:"size:32;index:idx_request_trace_scope,priority:2"`
	Service      requesttrace.Service `gorm:"size:16;index:idx_request_trace_scope,priority:3;index:idx_request_trace_request_scope,priority:3"`
	GroupID      string               `gorm:"size:64;index:idx_request_trace_scope,priority:1;index:idx_request_trace_request_scope,priority:1"`
	RequestID    string               `gorm:"size:128;index;index:idx_request_trace_request_scope,priority:2"`
	ParentSpanID string               `gorm:"size:32"`
	Stage        requesttrace.Stage   `gorm:"size:32;index:idx_request_trace_request_scope,priority:4"`
	Status       requesttrace.Status  `gorm:"size:16;index"`
	StartedAt    time.Time            `gorm:"index"`
	EndedAt      *time.Time
	DurationMS   *float64
	Revision     int
	Attributes   requesttrace.Attributes `gorm:"serializer:json;type:text"`
	CreatedAt    time.Time
	UpdatedAt    time.Time `gorm:"index"`
}

func (RequestTraceSpan) TableName() string {
	return "request_trace_spans"
}

type TraceStore struct {
	db *gorm.DB
}

func NewTraceStore(db *gorm.DB) *TraceStore {
	return &TraceStore{db: db}
}

func (s *TraceStore) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("request trace store database is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.WithContext(ctx).AutoMigrate(&RequestTraceHead{}, &RequestTraceSpan{}, &RequestTraceNonce{}, &RequestTraceTask{})
}

func (s *TraceStore) Write(ctx context.Context, span requesttrace.Span) error {
	if s == nil || s.db == nil {
		return errors.New("request trace store database is nil")
	}
	if err := requesttrace.Validate(span); err != nil {
		return fmt.Errorf("validate request trace span: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return s.writeTransaction(tx, span)
	})
}

func (s *TraceStore) writeTransaction(tx *gorm.DB, span requesttrace.Span) error {
	now := time.Now().UTC()
	head := RequestTraceHead{
		TraceID:   span.TraceID,
		Service:   span.Service,
		GroupID:   span.GroupID,
		Truncated: span.Truncated,
		UpdatedAt: now,
	}
	if err := tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "trace_id"}, {Name: "service"}},
		DoNothing: true,
	}).Create(&head).Error; err != nil {
		return fmt.Errorf("create request trace head: %w", err)
	}

	query := tx
	if tx.Dialector.Name() == "postgres" {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	if err := query.Where("trace_id = ? AND service = ?", span.TraceID, span.Service).
		First(&head).Error; err != nil {
		return fmt.Errorf("lock request trace head: %w", err)
	}
	if head.GroupID != span.GroupID {
		return fmt.Errorf("%w: trace/service group differs", errRequestTraceOwnership)
	}

	var existing RequestTraceSpan
	err := tx.Where("span_id = ?", span.SpanID).First(&existing).Error
	switch {
	case err == nil:
		if !sameTraceSpanIdentity(existing, span) {
			return errRequestTraceOwnership
		}
		if span.Revision <= existing.Revision || terminalTraceStatus(existing.Status) {
			return markTraceTruncated(tx, head, span.Truncated)
		}

		result := tx.Model(&RequestTraceSpan{}).
			Where("span_id = ? AND revision < ? AND status = ?", span.SpanID, span.Revision, requesttrace.StatusRunning).
			Updates(map[string]any{
				"status":      span.Status,
				"ended_at":    span.EndedAt,
				"duration_ms": span.DurationMS,
				"revision":    span.Revision,
				"updated_at":  now,
			})
		if result.Error != nil {
			return fmt.Errorf("update request trace span: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return markTraceTruncated(tx, head, span.Truncated)
		}
		return touchTraceHead(tx, head, span.Truncated, now)

	case !errors.Is(err, gorm.ErrRecordNotFound):
		return fmt.Errorf("read request trace span: %w", err)
	}

	if head.SpanCount >= maxPersistedSpansPerTrace {
		return markTraceTruncated(tx, head, true)
	}

	headUpdate := tx.Model(&RequestTraceHead{}).
		Where("trace_id = ? AND service = ? AND span_count < ?", span.TraceID, span.Service, maxPersistedSpansPerTrace).
		Updates(map[string]any{
			"span_count": gorm.Expr("span_count + 1"),
			"truncated":  head.Truncated || span.Truncated,
			"updated_at": now,
		})
	if headUpdate.Error != nil {
		return fmt.Errorf("reserve request trace span quota: %w", headUpdate.Error)
	}
	if headUpdate.RowsAffected == 0 {
		return markTraceTruncated(tx, head, true)
	}

	persisted := RequestTraceSpan{
		SpanID:       span.SpanID,
		Version:      span.Version,
		TraceID:      span.TraceID,
		Service:      span.Service,
		GroupID:      span.GroupID,
		RequestID:    span.RequestID,
		ParentSpanID: span.ParentSpanID,
		Stage:        span.Stage,
		Status:       span.Status,
		StartedAt:    canonicalTraceTimestamp(span.StartedAt),
		EndedAt:      span.EndedAt,
		DurationMS:   span.DurationMS,
		Revision:     span.Revision,
		Attributes:   span.Attributes,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := tx.Create(&persisted).Error; err != nil {
		return fmt.Errorf("create request trace span: %w", err)
	}

	return nil
}

func sameTraceSpanIdentity(persisted RequestTraceSpan, span requesttrace.Span) bool {
	return persisted.TraceID == span.TraceID &&
		persisted.Service == span.Service &&
		persisted.GroupID == span.GroupID &&
		persisted.RequestID == span.RequestID &&
		persisted.ParentSpanID == span.ParentSpanID &&
		persisted.Stage == span.Stage &&
		canonicalTraceTimestamp(persisted.StartedAt).Equal(canonicalTraceTimestamp(span.StartedAt))
}

func canonicalTraceTimestamp(value time.Time) time.Time {
	return value.Truncate(time.Microsecond)
}

func terminalTraceStatus(status requesttrace.Status) bool {
	return status != requesttrace.StatusRunning
}

func markTraceTruncated(tx *gorm.DB, head RequestTraceHead, truncated bool) error {
	if !truncated || head.Truncated {
		return nil
	}
	return tx.Model(&RequestTraceHead{}).
		Where("trace_id = ? AND service = ?", head.TraceID, head.Service).
		UpdateColumn("truncated", true).Error
}

func touchTraceHead(tx *gorm.DB, head RequestTraceHead, truncated bool, now time.Time) error {
	return tx.Model(&RequestTraceHead{}).
		Where("trace_id = ? AND service = ?", head.TraceID, head.Service).
		Updates(map[string]any{
			"truncated":  head.Truncated || truncated,
			"updated_at": now,
		}).Error
}
