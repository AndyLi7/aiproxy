package model

import (
	"errors"
	"strconv"
	"time"

	"github.com/labring/aiproxy/core/relay/mode"
)

const (
	MaxGroupVideoTaskPage     = 10_000
	MaxGroupVideoTaskPageSize = 100
)

var videoCreationModes = []int{
	int(mode.VideoGenerationsJobs),
	int(mode.Videos),
	int(mode.GeminiVideo),
	int(mode.AliVideo),
	int(mode.DoubaoVideo),
}

type GroupVideoTaskParams struct {
	Size          string `json:"size,omitempty"`
	Resolution    string `json:"resolution,omitempty"`
	GenerateAudio *bool  `json:"generate_audio,omitempty"`
}

type GroupVideoTaskView struct {
	ID           string               `json:"id"`
	RequestID    string               `json:"request_id"`
	PublicTaskID string               `json:"public_task_id,omitempty"`
	Model        string               `json:"model"`
	TokenName    string               `json:"token_name,omitempty"`
	Provider     string               `json:"provider,omitempty"`
	Status       string               `json:"status"`
	Params       GroupVideoTaskParams `json:"params"`
	Amount       *float64             `json:"amount"`
	Currency     string               `json:"currency,omitempty"`
	CreatedAt    time.Time            `json:"created_at"`
	CompletedAt  *time.Time           `json:"completed_at"`
}

type GroupVideoTaskPage struct {
	Items []GroupVideoTaskView `json:"items"`
	Total int64                `json:"total"`
}

func ListGroupVideoTasks(group string, page, perPage int) (GroupVideoTaskPage, error) {
	if group == "" || len(group) > 64 {
		return GroupVideoTaskPage{}, errors.New("invalid group")
	}
	if page <= 0 || page > MaxGroupVideoTaskPage ||
		perPage <= 0 || perPage > MaxGroupVideoTaskPageSize {
		return GroupVideoTaskPage{}, errors.New("invalid pagination")
	}
	if LogDB == nil || DB == nil {
		return GroupVideoTaskPage{}, errors.New("database is not initialized")
	}

	query := LogDB.Model(&AsyncUsageInfo{}).
		Where("group_id = ?", group).
		Where("mode IN ?", videoCreationModes).
		Where("status IN ?", []AsyncUsageStatus{
			AsyncUsageStatusPending,
			AsyncUsageStatusCompleted,
			AsyncUsageStatusFailed,
		})

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return GroupVideoTaskPage{}, err
	}

	infos := make([]AsyncUsageInfo, 0, perPage)
	if err := query.
		Order("created_at DESC, id DESC").
		Limit(perPage).
		Offset((page - 1) * perPage).
		Find(&infos).Error; err != nil {
		return GroupVideoTaskPage{}, err
	}

	channelIDs := make([]int, 0, len(infos))
	seenChannelIDs := make(map[int]struct{}, len(infos))
	for i := range infos {
		if infos[i].ChannelID == 0 {
			continue
		}
		if _, ok := seenChannelIDs[infos[i].ChannelID]; ok {
			continue
		}
		seenChannelIDs[infos[i].ChannelID] = struct{}{}
		channelIDs = append(channelIDs, infos[i].ChannelID)
	}

	channels, err := GetChannelsBasicInfoByIDs(channelIDs)
	if err != nil {
		return GroupVideoTaskPage{}, err
	}
	providers := make(map[int]string, len(channels))
	for _, channel := range channels {
		providers[channel.ID] = channel.Type.String()
	}

	items := make([]GroupVideoTaskView, 0, len(infos))
	for i := range infos {
		items = append(items, groupVideoTaskView(infos[i], providers[infos[i].ChannelID]))
	}

	return GroupVideoTaskPage{Items: items, Total: total}, nil
}

func groupVideoTaskView(info AsyncUsageInfo, provider string) GroupVideoTaskView {
	view := GroupVideoTaskView{
		ID:           strconv.Itoa(info.ID),
		RequestID:    info.RequestID,
		PublicTaskID: info.UpstreamID,
		Model:        info.Model,
		TokenName:    info.TokenName,
		Provider:     provider,
		Status:       groupVideoTaskStatus(info.Status),
		Params: GroupVideoTaskParams{
			Size:          info.UsageContext.Resolution,
			Resolution:    info.UsageContext.NativeResolution,
			GenerateAudio: info.UsageContext.OutputAudio,
		},
		Currency:  info.PricingCurrency,
		CreatedAt: info.CreatedAt,
	}
	if info.Status == AsyncUsageStatusCompleted && info.PricingCurrency != "" {
		amount := info.Amount.UsedAmount
		view.Amount = &amount
	}
	if info.Status == AsyncUsageStatusCompleted || info.Status == AsyncUsageStatusFailed {
		completedAt := info.UpdatedAt
		view.CompletedAt = &completedAt
	}
	return view
}

func groupVideoTaskStatus(status AsyncUsageStatus) string {
	switch status {
	case AsyncUsageStatusPending:
		return "processing"
	case AsyncUsageStatusCompleted:
		return "completed"
	case AsyncUsageStatusFailed:
		return "failed"
	default:
		return "failed"
	}
}
