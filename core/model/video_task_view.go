package model

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/labring/aiproxy/core/relay/mode"
)

const maxGroupVideoTaskPage = 10000

var errInvalidGroupVideoTaskQuery = errors.New("invalid group video task query")

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

var videoCreationModes = []int{
	int(mode.VideoGenerationsJobs),
	int(mode.Videos),
	int(mode.GeminiVideo),
	int(mode.AliVideo),
	int(mode.DoubaoVideo),
}

func ListGroupVideoTasks(group string, page, perPage int) (*GroupVideoTaskPage, error) {
	if group == "" || len(group) > 64 || page < 1 || page > maxGroupVideoTaskPage ||
		perPage < 1 || perPage > 100 || LogDB == nil || DB == nil {
		return nil, errInvalidGroupVideoTaskQuery
	}

	query := LogDB.Model(&AsyncUsageInfo{}).
		Where("group_id = ?", group).
		Where("mode IN ?", videoCreationModes)

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, err
	}

	var infos []AsyncUsageInfo
	if err := query.
		Order("created_at DESC, id DESC").
		Offset((page - 1) * perPage).
		Limit(perPage).
		Find(&infos).Error; err != nil {
		return nil, err
	}

	channelIDs := make([]int, 0, len(infos))
	seenChannelIDs := make(map[int]struct{}, len(infos))
	for i := range infos {
		if infos[i].ChannelID <= 0 {
			continue
		}
		if _, exists := seenChannelIDs[infos[i].ChannelID]; exists {
			continue
		}
		seenChannelIDs[infos[i].ChannelID] = struct{}{}
		channelIDs = append(channelIDs, infos[i].ChannelID)
	}

	channels, err := GetChannelsBasicInfoByIDs(channelIDs)
	if err != nil {
		return nil, err
	}
	providers := make(map[int]string, len(channels))
	for _, channel := range channels {
		providers[channel.ID] = channel.Type.String()
	}

	items := make([]GroupVideoTaskView, 0, len(infos))
	for i := range infos {
		info := &infos[i]
		item := GroupVideoTaskView{
			ID:           strconv.Itoa(info.ID),
			RequestID:    info.RequestID,
			PublicTaskID: info.UpstreamID,
			Model:        info.Model,
			TokenName:    info.TokenName,
			Provider:     providers[info.ChannelID],
			Status:       groupVideoTaskStatus(info.Status),
			Params: GroupVideoTaskParams{
				Size:          info.UsageContext.Resolution,
				Resolution:    info.UsageContext.NativeResolution,
				GenerateAudio: info.UsageContext.OutputAudio,
			},
			CreatedAt: info.CreatedAt,
		}

		if info.Status == AsyncUsageStatusCompleted || info.Status == AsyncUsageStatusFailed {
			completedAt := info.UpdatedAt
			item.CompletedAt = &completedAt
			if amount, currency := groupVideoTaskSettlement(info); amount != nil {
				item.Amount = amount
				item.Currency = currency
			}
		}
		items = append(items, item)
	}

	return &GroupVideoTaskPage{Items: items, Total: total}, nil
}

func groupVideoTaskSettlement(info *AsyncUsageInfo) (*float64, string) {
	if info == nil || info.Amount.UsedAmount < 0 || math.IsNaN(info.Amount.UsedAmount) ||
		math.IsInf(info.Amount.UsedAmount, 0) {
		return nil, ""
	}
	if info.PricingCurrency != "" && !strings.EqualFold(info.PricingCurrency, "USD") {
		return nil, ""
	}

	amount := info.Amount.UsedAmount
	return &amount, "USD"
}

func groupVideoTaskStatus(status AsyncUsageStatus) string {
	switch status {
	case AsyncUsageStatusCompleted:
		return "completed"
	case AsyncUsageStatusFailed:
		return "failed"
	default:
		return "processing"
	}
}
