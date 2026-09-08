package model

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/labring/aiproxy/core/common/requesttrace"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	defaultTracePageLimit = 50
	maxTracePageLimit     = 100
	traceRetention        = 14 * 24 * time.Hour
	maxTraceCleanBatch    = 1000
)

// TraceQuery scopes a trace page to exactly one group, trace, and service.
// An empty GroupID matches only spans recorded without a group.
type TraceQuery struct {
	GroupID     string
	TraceID     string
	Service     requesttrace.Service
	AfterSpanID string
	Limit       int
}

// TracePage is a span-ID ordered page. Truncated reports whether another page exists.
type TracePage struct {
	Items      []requesttrace.Span
	NextCursor string
	Truncated  bool
}

type traceSpanProjection struct {
	Version      int
	TraceID      string
	SpanID       string
	ParentSpanID string
	RequestID    string
	GroupID      string
	Service      requesttrace.Service
	Stage        requesttrace.Stage
	Status       requesttrace.Status
	StartedAt    time.Time
	EndedAt      *time.Time
	DurationMS   *float64
	Revision     int
	Attributes   requesttrace.Attributes `gorm:"serializer:json;type:text"`
}

func (s *TraceStore) List(ctx context.Context, q TraceQuery) (TracePage, error) {
	if s == nil || s.db == nil {
		return TracePage{}, errors.New("request trace store database is nil")
	}
	if err := ctx.Err(); err != nil {
		return TracePage{}, err
	}

	limit := q.Limit
	if limit <= 0 {
		limit = defaultTracePageLimit
	}
	if limit > maxTracePageLimit {
		limit = maxTracePageLimit
	}

	query := s.db.WithContext(ctx).Model(&RequestTraceSpan{}).Select([]string{
		"version", "trace_id", "span_id", "parent_span_id", "request_id", "group_id",
		"service", "stage", "status", "started_at", "ended_at", "duration_ms", "revision", "attributes",
	}).Where("group_id = ? AND trace_id = ? AND service = ?", q.GroupID, q.TraceID, q.Service)
	if q.AfterSpanID != "" {
		query = query.Where("span_id > ?", q.AfterSpanID)
	}

	var rows []traceSpanProjection
	if err := query.Order("span_id ASC").Limit(limit + 1).Find(&rows).Error; err != nil {
		return TracePage{}, fmt.Errorf("list request trace spans: %w", err)
	}

	page := TracePage{Items: make([]requesttrace.Span, 0, min(len(rows), limit))}
	if len(rows) > limit {
		page.Truncated = true
		rows = rows[:limit]
	}
	for _, row := range rows {
		page.Items = append(page.Items, requesttrace.Span{
			Version:      row.Version,
			TraceID:      row.TraceID,
			SpanID:       row.SpanID,
			ParentSpanID: row.ParentSpanID,
			RequestID:    row.RequestID,
			GroupID:      row.GroupID,
			Service:      row.Service,
			Stage:        row.Stage,
			Status:       row.Status,
			StartedAt:    row.StartedAt,
			EndedAt:      row.EndedAt,
			DurationMS:   row.DurationMS,
			Revision:     row.Revision,
			Attributes:   row.Attributes,
		})
	}
	if page.Truncated {
		page.NextCursor = page.Items[len(page.Items)-1].SpanID
	}
	return page, nil
}

type traceCleanupCandidate struct {
	SpanID  string
	TraceID string
	Service requesttrace.Service
}

type traceHeadKey struct {
	TraceID string
	Service requesttrace.Service
}

// CleanExpired deletes at most batchSize trace spans whose persisted update time is older than now-14 days.
// The supplied time is expected to come from trusted server scheduling code, never an API request.
func (s *TraceStore) CleanExpired(ctx context.Context, now time.Time, batchSize int) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("request trace store database is nil")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if batchSize < 1 || batchSize > maxTraceCleanBatch {
		return 0, fmt.Errorf("request trace cleanup batch size must be between 1 and %d", maxTraceCleanBatch)
	}
	cutoff := now.UTC().Add(-traceRetention)

	var deleted int64
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var candidates []traceCleanupCandidate
		if err := tx.Model(&RequestTraceSpan{}).
			Select("span_id", "trace_id", "service").
			Where("updated_at < ?", cutoff).
			Order("updated_at ASC, span_id ASC").
			Limit(batchSize).
			Find(&candidates).Error; err != nil {
			return fmt.Errorf("select expired request trace spans: %w", err)
		}

		byHead := make(map[traceHeadKey][]string, len(candidates))
		for _, candidate := range candidates {
			key := traceHeadKey{TraceID: candidate.TraceID, Service: candidate.Service}
			byHead[key] = append(byHead[key], candidate.SpanID)
		}
		var staleHeads []traceHeadKey
		if err := tx.Model(&RequestTraceHead{}).
			Select("trace_id", "service").
			Where("updated_at < ?", cutoff).
			Where("NOT EXISTS (SELECT 1 FROM request_trace_spans WHERE request_trace_spans.trace_id = request_trace_heads.trace_id AND request_trace_spans.service = request_trace_heads.service)").
			Order("trace_id ASC, service ASC").
			Limit(batchSize).
			Find(&staleHeads).Error; err != nil {
			return fmt.Errorf("select empty expired request trace heads: %w", err)
		}
		for _, key := range staleHeads {
			if _, ok := byHead[key]; !ok {
				byHead[key] = nil
			}
		}
		keys := make([]traceHeadKey, 0, len(byHead))
		for key := range byHead {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].TraceID == keys[j].TraceID {
				return keys[i].Service < keys[j].Service
			}
			return keys[i].TraceID < keys[j].TraceID
		})

		for _, key := range keys {
			lockedHead := tx
			if tx.Dialector.Name() == "postgres" {
				lockedHead = lockedHead.Clauses(clause.Locking{Strength: "UPDATE"})
			}
			var head RequestTraceHead
			err := lockedHead.Where("trace_id = ? AND service = ?", key.TraceID, key.Service).First(&head).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			if err != nil {
				return fmt.Errorf("lock request trace head for cleanup: %w", err)
			}

			if spanIDs := byHead[key]; len(spanIDs) > 0 {
				result := tx.Where("span_id IN ? AND updated_at < ?", spanIDs, cutoff).Delete(&RequestTraceSpan{})
				if result.Error != nil {
					return fmt.Errorf("delete expired request trace spans: %w", result.Error)
				}
				deleted += result.RowsAffected
			}

			var remaining int64
			if err := tx.Model(&RequestTraceSpan{}).
				Where("trace_id = ? AND service = ?", key.TraceID, key.Service).
				Count(&remaining).Error; err != nil {
				return fmt.Errorf("count request trace spans after cleanup: %w", err)
			}
			if remaining == 0 {
				result := tx.Where("trace_id = ? AND service = ? AND updated_at < ?", key.TraceID, key.Service, cutoff).
					Delete(&RequestTraceHead{})
				if result.Error != nil {
					return fmt.Errorf("delete expired request trace head: %w", result.Error)
				}
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}
