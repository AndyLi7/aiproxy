package model

import (
	"context"
	"encoding/hex"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"strings"
	"time"
)

// RequestTraceTask is a diagnostic association, never a source of billing state.
type RequestTraceTask struct {
	PollCount    int64 `gorm:"not null;default:0"`
	FirstPollAt  *time.Time
	LastPollAt   *time.Time
	AsyncUsageID int       `gorm:"primaryKey"`
	GroupID      string    `gorm:"size:64;index:idx_trace_task_owner,priority:1;index:idx_trace_task_trace,priority:1"`
	TokenID      int       `gorm:"index:idx_trace_task_owner,priority:2"`
	ChannelID    int       `gorm:"index:idx_trace_task_owner,priority:3"`
	TaskID       string    `gorm:"size:256;index:idx_trace_task_owner,priority:4"`
	TraceID      string    `gorm:"size:32;index:idx_trace_task_trace,priority:2"`
	ParentSpanID string    `gorm:"size:32"`
	ExpiresAt    time.Time `gorm:"index"`
}

func (RequestTraceTask) TableName() string { return "request_trace_tasks" }

// No task, token or upstream identifiers are exposed by the aggregate view.
type TraceTaskSummary struct {
	PollCount   int64      `json:"poll_count"`
	FirstPollAt *time.Time `json:"first_poll_at,omitempty"`
	LastPollAt  *time.Time `json:"last_poll_at,omitempty"`
}

func (s *TraceStore) TaskTraceSummary(ctx context.Context, group, traceID string, now time.Time) (*TraceTaskSummary, error) {
	if s == nil || s.db == nil || ctx == nil || group == "" || !traceTaskRandomID(traceID) {
		return nil, errors.New("trace summary unavailable")
	}
	var rows []RequestTraceTask
	err := s.db.WithContext(ctx).Select("poll_count, first_poll_at, last_poll_at").Where("group_id = ? AND trace_id = ? AND expires_at > ?", group, traceID, now.UTC()).Limit(2).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	if len(rows) != 1 {
		return nil, errors.New("trace summary ambiguous")
	}
	return &TraceTaskSummary{PollCount: rows[0].PollCount, FirstPollAt: rows[0].FirstPollAt, LastPollAt: rows[0].LastPollAt}, nil
}

func (s *TraceStore) SaveTaskTrace(ctx context.Context, asyncID int, group, traceID, parentID string) error {
	if s == nil || s.db == nil || ctx == nil || asyncID <= 0 || group == "" || !traceTaskRandomID(traceID) || !traceTaskRandomID(parentID) {
		return errors.New("trace task association unavailable")
	}
	var info AsyncUsageInfo
	if err := s.db.WithContext(ctx).Where("id = ? AND group_id = ?", asyncID, group).First(&info).Error; err != nil {
		return err
	}
	if info.TokenID <= 0 || info.ChannelID <= 0 || info.UpstreamID == "" || len(info.UpstreamID) > 256 {
		return errors.New("trace task identity unavailable")
	}
	link := RequestTraceTask{AsyncUsageID: info.ID, GroupID: info.GroupID, TokenID: info.TokenID, ChannelID: info.ChannelID, TaskID: info.UpstreamID, TraceID: traceID, ParentSpanID: parentID, ExpiresAt: time.Now().UTC().Add(14 * 24 * time.Hour)}
	// Repeated diagnostic writes cannot change an existing task's association.
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&link).Error
}

// ResolveTaskTrace is called only after the business task ownership check.
// Exact token/channel scoping and ambiguity detection remain defense in depth.
func (s *TraceStore) ResolveTaskTrace(ctx context.Context, group string, tokenID, channelID int, taskID string, now time.Time) (RequestTraceTask, error) {
	if s == nil || s.db == nil || ctx == nil || group == "" || tokenID <= 0 || channelID <= 0 || taskID == "" || len(taskID) > 256 || now.IsZero() {
		return RequestTraceTask{}, errors.New("trace task association unavailable")
	}
	var links []RequestTraceTask
	err := s.db.WithContext(ctx).Where("group_id = ? AND token_id = ? AND channel_id = ? AND task_id = ? AND expires_at > ?", group, tokenID, channelID, taskID, now.UTC()).Limit(2).Find(&links).Error
	if err != nil {
		return RequestTraceTask{}, err
	}
	if len(links) != 1 {
		return RequestTraceTask{}, errors.New("trace task association unavailable")
	}
	return links[0], nil
}

func traceTaskRandomID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16 && strings.ToLower(id) == id
}

// RecordTaskPoll retains bounded aggregate facts independently of the span quota.
func (s *TraceStore) RecordTaskPoll(ctx context.Context, id int, group string, observed time.Time) error {
	if s == nil || s.db == nil || ctx == nil || id <= 0 || group == "" || observed.IsZero() {
		return errors.New("invalid task poll summary")
	}
	at := observed.UTC()
	result := s.db.WithContext(ctx).Model(&RequestTraceTask{}).Where("async_usage_id = ? AND group_id = ?", id, group).Updates(map[string]any{
		"poll_count":    gorm.Expr("CASE WHEN poll_count < 9223372036854775807 THEN poll_count + 1 ELSE poll_count END"),
		"first_poll_at": gorm.Expr("CASE WHEN first_poll_at IS NULL OR first_poll_at > ? THEN ? ELSE first_poll_at END", at, at),
		"last_poll_at":  gorm.Expr("CASE WHEN last_poll_at IS NULL OR last_poll_at < ? THEN ? ELSE last_poll_at END", at, at),
		"expires_at":    gorm.Expr("CASE WHEN expires_at < ? THEN ? ELSE expires_at END", at.Add(14*24*time.Hour), at.Add(14*24*time.Hour)),
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("task poll association unavailable")
	}
	return nil
}

func (s *TraceStore) CleanTaskTraces(ctx context.Context, now time.Time, limit int) (int64, error) {
	if s == nil || s.db == nil || ctx == nil || now.IsZero() || limit < 1 || limit > 500 {
		return 0, errors.New("invalid task trace cleanup")
	}
	var ids []int
	if err := s.db.WithContext(ctx).Model(&RequestTraceTask{}).Where("expires_at < ?", now.UTC()).Order("expires_at, async_usage_id").Limit(limit).Pluck("async_usage_id", &ids).Error; err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	result := s.db.WithContext(ctx).Where("async_usage_id IN ? AND expires_at < ?", ids, now.UTC()).Delete(&RequestTraceTask{})
	return result.RowsAffected, result.Error
}
