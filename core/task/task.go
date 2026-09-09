package task

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/consume"
	"github.com/labring/aiproxy/core/common/conv"
	"github.com/labring/aiproxy/core/common/ipblack"
	"github.com/labring/aiproxy/core/common/notify"
	"github.com/labring/aiproxy/core/common/oncall"
	"github.com/labring/aiproxy/core/common/trylock"
	"github.com/labring/aiproxy/core/controller"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/labring/aiproxy/core/relay/adaptors"
	"github.com/labring/aiproxy/core/trace"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// AutoTestBannedModelsTask 自动测试被禁用的模型
func AutoTestBannedModelsTask(ctx context.Context) {
	ticker := time.NewTicker(time.Second * 30)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			controller.AutoTestBannedModels()
		}
	}
}

// DetectIPGroupsTask 检测 IP 使用多个 group 的情况
func DetectIPGroupsTask(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !trylock.Lock("runDetectIPGroups", time.Minute) {
				continue
			}

			detectIPGroups()
		}
	}
}

func detectIPGroups() {
	threshold := config.GetIPGroupsThreshold()
	if threshold < 1 {
		return
	}

	ipGroupList, err := model.GetIPGroups(int(threshold), time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		notify.ErrorThrottle("detectIPGroups", time.Minute, "detect IP groups failed", err.Error())
		return
	}

	if len(ipGroupList) == 0 {
		return
	}

	banThreshold := config.GetIPGroupsBanThreshold()
	for ip, groups := range ipGroupList {
		slices.Sort(groups)

		groupsJSON, err := sonic.MarshalString(groups)
		if err != nil {
			notify.ErrorThrottle(
				"detectIPGroupsMarshal",
				time.Minute,
				"marshal IP groups failed",
				err.Error(),
			)

			continue
		}

		if banThreshold >= threshold && len(groups) >= int(banThreshold) {
			rowsAffected, err := model.UpdateGroupsStatus(groups, model.GroupStatusDisabled)
			if err != nil {
				notify.ErrorThrottle(
					"detectIPGroupsBan",
					time.Minute,
					"update groups status failed",
					err.Error(),
				)
			}

			if rowsAffected > 0 {
				notify.Warn(
					fmt.Sprintf(
						"Suspicious activity: IP %s is using %d groups (exceeds ban threshold of %d). IP and all groups have been disabled.",
						ip,
						len(groups),
						banThreshold,
					),
					groupsJSON,
				)
				ipblack.SetIPBlackAnyWay(ip, time.Hour*48)
			}

			continue
		}

		h := sha256.New()
		h.Write(conv.StringToBytes(groupsJSON))
		groupsHash := hex.EncodeToString(h.Sum(nil))
		hashKey := fmt.Sprintf("%s:%s", ip, groupsHash)

		notify.WarnThrottle(
			hashKey,
			time.Hour*3,
			fmt.Sprintf(
				"Potential abuse: IP %s is using %d groups (exceeds threshold of %d)",
				ip,
				len(groups),
				threshold,
			),
			groupsJSON,
		)
	}
}

// UsageAlertTask 用量异常告警任务
func UsageAlertTask(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !trylock.Lock("runUsageAlert", time.Hour) {
				continue
			}

			checkUsageAlert()
		}
	}
}

func checkUsageAlert() {
	threshold := config.GetUsageAlertThreshold()
	if threshold <= 0 {
		return
	}

	// 获取配置的白名单
	whitelist := config.GetUsageAlertWhitelist()

	// 获取前三天平均用量最低阈值
	minAvgThreshold := config.GetUsageAlertMinAvgThreshold()

	alerts, err := model.GetGroupUsageAlert(float64(threshold), float64(minAvgThreshold), whitelist)
	if err != nil {
		notify.ErrorThrottle(
			"usageAlertError",
			time.Minute*5,
			"check usage alert failed",
			err.Error(),
		)

		return
	}

	if len(alerts) == 0 {
		return
	}

	// 计算到明天 0 点的时间，确保每个 group 一天只告警一次
	now := time.Now()
	tomorrow := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	lockDuration := tomorrow.Sub(now)

	// 过滤掉当天已经告警过的 group（通过 trylock 判断）
	var validAlerts []model.GroupUsageAlertItem
	for _, alert := range alerts {
		lockKey := "usageAlert:" + alert.GroupID
		// 尝试获取锁，如果获取失败说明当天已经告警过
		if trylock.Lock(lockKey, lockDuration) {
			validAlerts = append(validAlerts, alert)
		}
	}

	if len(validAlerts) == 0 {
		return
	}

	message := formatGroupUsageAlerts(validAlerts)
	notify.Warn(
		fmt.Sprintf("Detected %d groups with abnormal usage", len(validAlerts)),
		message,
	)
}

// formatGroupUsageAlerts 格式化告警消息
func formatGroupUsageAlerts(alerts []model.GroupUsageAlertItem) string {
	if len(alerts) == 0 {
		return ""
	}

	var result strings.Builder
	for _, alert := range alerts {
		fmt.Fprintf(&result, "GroupID: %s | 3-Day Avg: %.4f | Today: %.4f | Ratio: %.2fx\n",
			alert.GroupID,
			alert.ThreeDayAvgAmount,
			alert.TodayAmount,
			alert.Ratio)
	}

	return result.String()
}

// CleanLogTask 清理日志任务
func CleanLogTask(ctx context.Context) {
	// the interval should not be too large to avoid cleaning too much at once
	ticker := time.NewTicker(time.Second * 5)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !trylock.Lock("runCleanLog", time.Second*3) {
				continue
			}

			optimize := trylock.Lock("runOptimizeLog", time.Hour*24)

			err := model.CleanLog(int(config.GetCleanLogBatchSize()), optimize)
			if err != nil {
				notify.ErrorThrottle(
					"cleanLogError",
					time.Minute*5,
					"clean log failed",
					err.Error(),
				)
			}
		}
	}
}

const (
	asyncUsagePollInterval    = time.Second * 3
	asyncUsageProcessingLease = time.Minute * 3
	asyncUsageBatchSize       = 50
	asyncUsageConcurrency     = 10
	asyncUsageMaxRetry        = 10
)

var (
	errAsyncUsageMetricsPending    = errors.New("async usage metrics pending")
	errAsyncUsageSettlementPending = errors.New("async usage settlement pending")
	errAsyncUsageManualSettlement  = errors.New("async usage requires manual settlement reconciliation")
	markAsyncUsageBalanceAttempted = model.MarkAsyncUsageBalanceConsumeAttempted
	markAsyncUsageBalanceConsumed  = model.MarkAsyncUsageBalanceConsumed
	completeClaimedAsyncUsageInfo  = model.CompleteClaimedAsyncUsageInfo
)

func AsyncUsagePollTask(ctx context.Context) {
	ticker := time.NewTicker(asyncUsagePollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for {
				fullBatch := processAsyncUsages(ctx)
				if !fullBatch {
					break
				}

				select {
				case <-ctx.Done():
					return
				default:
				}

				log.Debugf(
					"async usage poll: batch full, continue immediately batch_size=%d",
					asyncUsageBatchSize,
				)
			}
		}
	}
}

func processAsyncUsages(ctx context.Context) bool {
	infos, err := model.GetPendingAsyncUsages(asyncUsageBatchSize)
	if err != nil {
		notify.ErrorThrottle(
			"asyncUsagePoll",
			time.Minute*5,
			"get pending async usages failed",
			err.Error(),
		)

		return false
	}

	if len(infos) == 0 {
		return false
	}

	claimedInfos := make([]*model.AsyncUsageInfo, 0, len(infos))
	for _, info := range infos {
		claimed, err := claimAsyncUsage(info)
		if err != nil {
			notify.ErrorThrottle(
				"asyncUsageClaim",
				time.Minute*5,
				"claim async usage failed",
				err.Error(),
			)

			continue
		}

		if claimed {
			claimedInfos = append(claimedInfos, info)
		}
	}

	if len(claimedInfos) == 0 {
		return len(infos) == asyncUsageBatchSize
	}

	log.Debugf(
		"async usage poll: pending=%d claimed=%d batch_size=%d concurrency=%d",
		len(infos),
		len(claimedInfos),
		asyncUsageBatchSize,
		asyncUsageConcurrency,
	)

	sem := make(chan struct{}, asyncUsageConcurrency)

	var wg sync.WaitGroup

	for _, info := range claimedInfos {
		select {
		case <-ctx.Done():
			wg.Wait()
			return false
		case sem <- struct{}{}:
		}

		wg.Add(1)

		go func(info *model.AsyncUsageInfo) {
			defer wg.Done()
			defer func() {
				<-sem
			}()

			processOneAsyncUsage(ctx, info)
		}(info)
	}

	wg.Wait()

	return len(infos) == asyncUsageBatchSize
}

func claimAsyncUsage(info *model.AsyncUsageInfo) (bool, error) {
	now := time.Now()
	token := common.ShortUUID()
	leaseUntil := now.Add(asyncUsageProcessingLease)

	claimed, err := model.TryClaimAsyncUsageInfo(info, token, leaseUntil, now)
	if err != nil || !claimed {
		return claimed, err
	}

	log.Debugf(
		"async usage poll: claimed id=%d request_id=%s upstream_id=%s lease_until=%s",
		info.ID,
		info.RequestID,
		info.UpstreamID,
		leaseUntil.Format(time.RFC3339),
	)

	return true, nil
}

func processOneAsyncUsage(ctx context.Context, info *model.AsyncUsageInfo) {
	ctx, stopRenew := startAsyncUsageClaimRenewal(ctx, info)
	defer stopRenew()

	log.Debugf(
		"async usage poll: start id=%d request_id=%s upstream_id=%s mode=%d model=%s channel_id=%d retry=%d next_poll_at=%s",
		info.ID,
		info.RequestID,
		info.UpstreamID,
		info.Mode,
		info.Model,
		info.ChannelID,
		info.RetryCount,
		info.NextPollAt.Format(time.RFC3339),
	)
	if info.Amount.UsedAmount > 0 {
		completePolledAsyncUsage(ctx, info, info.Usage, info.UsageContext)

		return
	}

	channel, err := model.GetChannelByID(info.ChannelID)
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			log.Debugf(
				"async usage poll: retry id=%d request_id=%s reason=channel_get_error err=%v",
				info.ID,
				info.RequestID,
				err,
			)
			scheduleAsyncUsageRetry(info, fmt.Errorf("get channel: %w", err))

			return
		}

		log.Debugf(
			"async usage poll: fail id=%d request_id=%s reason=channel_not_found err=%v",
			info.ID,
			info.RequestID,
			err,
		)
		markAsyncUsageFailed(info, "channel not found: "+err.Error())

		return
	}

	a, ok := adaptors.GetAdaptor(channel.Type)
	if !ok {
		log.Debugf(
			"async usage poll: fail id=%d request_id=%s reason=adaptor_not_found channel_type=%d",
			info.ID,
			info.RequestID,
			channel.Type,
		)
		markAsyncUsageFailed(
			info,
			fmt.Sprintf("adaptor not found for channel type %d", channel.Type),
		)

		return
	}

	fetcher, ok := a.(adaptor.AsyncUsageFetcher)
	if !ok {
		log.Debugf(
			"async usage poll: fail id=%d request_id=%s reason=fetcher_not_supported channel_type=%d",
			info.ID,
			info.RequestID,
			channel.Type,
		)
		markAsyncUsageFailed(info, "adaptor does not support async usage fetching")

		return
	}

	finishTracePoll := trace.Current().BeginTaskPoll(ctx, info)
	usage, usageContext, completed, err := fetcher.FetchAsyncUsage(ctx, adaptor.AsyncUsageRequest{
		Channel: channel,
		Info:    info,
		Store:   controller.AdaptorStore,
	})
	finishTracePoll(completed, err)
	if err != nil {
		if completed {
			log.Debugf(
				"async usage poll: fail id=%d request_id=%s upstream_id=%s reason=upstream_final_error err=%v",
				info.ID,
				info.RequestID,
				info.UpstreamID,
				err,
			)
			markAsyncUsageFailed(info, err.Error())

			return
		}

		scheduleAsyncUsageRetry(info, err)

		return
	}

	if !completed {
		log.Debugf(
			"async usage poll: pending id=%d request_id=%s upstream_id=%s",
			info.ID,
			info.RequestID,
			info.UpstreamID,
		)
		touchAsyncUsagePollCursor(info)

		return
	}

	completePolledAsyncUsage(ctx, info, usage, usageContext)
}

func completePolledAsyncUsage(
	ctx context.Context,
	info *model.AsyncUsageInfo,
	usage model.Usage,
	usageContext model.UsageContext,
) {
	if err := completeAsyncUsage(ctx, info, usage, usageContext); err != nil {
		if errors.Is(err, context.Canceled) {
			log.Debugf(
				"async usage poll: claim work cancelled id=%d request_id=%s upstream_id=%s",
				info.ID,
				info.RequestID,
				info.UpstreamID,
			)

			return
		}
		if errors.Is(err, errAsyncUsageMetricsPending) {
			log.Debugf(
				"async usage poll: metrics_pending id=%d request_id=%s upstream_id=%s",
				info.ID,
				info.RequestID,
				info.UpstreamID,
			)

			return
		}
		if errors.Is(err, errAsyncUsageSettlementPending) {
			log.Debugf(
				"async usage poll: settlement_pending id=%d request_id=%s upstream_id=%s err=%v",
				info.ID,
				info.RequestID,
				info.UpstreamID,
				err,
			)
			scheduleAsyncUsageSettlementRetry(info)

			return
		}
		if errors.Is(err, errAsyncUsageManualSettlement) {
			log.Errorf(
				"async usage poll: manual settlement reconciliation required id=%d request_id=%s upstream_id=%s err=%v",
				info.ID,
				info.RequestID,
				info.UpstreamID,
				err,
			)
			parkAsyncUsageSettlement(info, err)

			return
		}

		log.Debugf(
			"async usage poll: settlement_error id=%d request_id=%s upstream_id=%s err=%v",
			info.ID,
			info.RequestID,
			info.UpstreamID,
			err,
		)
		notify.ErrorThrottle(
			"asyncUsageSettlement",
			time.Minute*5,
			"settle completed async usage",
			err.Error(),
		)
		scheduleAsyncUsageSettlementRetry(info)

		return
	}

	log.Debugf(
		"async usage poll: completed id=%d request_id=%s upstream_id=%s input=%d output=%d total=%d",
		info.ID,
		info.RequestID,
		info.UpstreamID,
		usage.InputTokens,
		usage.OutputTokens,
		usage.TotalTokens,
	)
}

func startAsyncUsageClaimRenewal(
	ctx context.Context,
	info *model.AsyncUsageInfo,
) (context.Context, func()) {
	return startAsyncUsageClaimRenewalAtInterval(
		ctx,
		info,
		asyncUsageProcessingLease/3,
	)
}

func startAsyncUsageClaimRenewalAtInterval(
	ctx context.Context,
	info *model.AsyncUsageInfo,
	interval time.Duration,
) (context.Context, func()) {
	workCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var stopOnce sync.Once

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				leaseUntil := time.Now().Add(asyncUsageProcessingLease)

				renewed, err := model.RenewAsyncUsageClaim(
					info.ID,
					info.ProcessingToken,
					leaseUntil,
				)
				if err != nil {
					notify.ErrorThrottle(
						"asyncUsageRenewClaim",
						time.Minute*5,
						"renew async usage claim failed",
						err.Error(),
					)

					cancel()

					return
				}

				if !renewed {
					log.Debugf(
						"async usage poll: claim lost id=%d request_id=%s upstream_id=%s",
						info.ID,
						info.RequestID,
						info.UpstreamID,
					)

					cancel()

					return
				}

				info.NextPollAt = leaseUntil
			}
		}
	}()

	return workCtx, func() {
		stopOnce.Do(func() {
			close(done)
			cancel()
		})
	}
}

func completeAsyncUsage(
	ctx context.Context,
	info *model.AsyncUsageInfo,
	usage model.Usage,
	usageContext model.UsageContext,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	usageContext = usageContext.WithFallback(info.UsageContext)

	price := info.Price
	settlementPrepared := info.Amount.UsedAmount > 0
	amount := info.Amount
	if settlementPrepared {
		usage = info.Usage
		usageContext = info.UsageContext
	} else {
		amount = consume.CalculateAmountDetailWithOptions(
			http.StatusOK,
			usage,
			usageContext,
			price,
			model.PriceSelectionOptions{
				DisableResolutionFuzzyMatch: info.DisableResolutionFuzzyMatch,
				RequestAt:                   info.RequestAt,
			},
		)
	}
	selectedPrice := price.SelectConditionalPriceWithOptions(
		usage,
		usageContext,
		model.PriceSelectionOptions{
			DisableResolutionFuzzyMatch: info.DisableResolutionFuzzyMatch,
			RequestAt:                   info.RequestAt,
		},
	)
	if asyncUsageNeedsMetrics(selectedPrice, usage, amount) {
		touchAsyncUsagePollCursor(info)

		return errAsyncUsageMetricsPending
	}
	selectedPrice.ConditionalPrices = nil
	nonReplaySafeDebitAttempted := false
	if amount.UsedAmount > 0 && !settlementPrepared {
		if err := model.PrepareClaimedAsyncUsageSettlement(
			info,
			usage,
			usageContext,
			amount,
		); err != nil {
			return fmt.Errorf("prepare async usage settlement: %w", err)
		}
		info.Usage = usage
		info.UsageContext = usageContext
		info.Amount = amount
	}

	// Persist the finalized usage before notifying an external balance provider.
	// The provider callback may immediately calculate its own settlement from this
	// log, so charging first exposes an incomplete usage snapshot and leaves that
	// settlement pending even though the customer debit succeeds.
	if err := model.UpdateLogUsageByRequestID(
		info.RequestID,
		usage,
		usageContext,
		selectedPrice,
		amount,
		info.PricingCurrency,
		info.PricingVersion,
	); err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			notify.ErrorThrottle(
				"asyncUsageUpdateLog",
				time.Minute*5,
				"update async usage log failed",
				err.Error(),
			)

			return fmt.Errorf("update async usage log: %w", err)
		}
	}

	if amount.UsedAmount > 0 && !info.BalanceConsumed {
		if err := ctx.Err(); err != nil {
			return err
		}
		charged, replaySafe, err := consumeAsyncUsageGroupBalance(ctx, info, amount.UsedAmount)
		nonReplaySafeDebitAttempted = charged && !replaySafe
		if err != nil {
			if errors.Is(err, errAsyncUsageManualSettlement) {
				return err
			}
			if !charged {
				return fmt.Errorf(
					"%w: consume async usage balance before charge: %v",
					errAsyncUsageSettlementPending,
					err,
				)
			}
			if replaySafe {
				return fmt.Errorf(
					"%w: confirm idempotent async usage debit: %v",
					errAsyncUsageSettlementPending,
					err,
				)
			}

			notify.ErrorThrottle(
				"asyncUsageConsumeBalance",
				time.Minute*5,
				"consume async usage balance failed",
				err.Error(),
			)
			recordAsyncUsageConsumeError(info, amount.UsedAmount, err)
			log.Errorf(
				"consume async usage balance failed, complete without retry to avoid duplicate charge: id=%d request_id=%s upstream_id=%s amount=%f err=%v",
				info.ID,
				info.RequestID,
				info.UpstreamID,
				amount.UsedAmount,
				err,
			)
		} else if charged {
			info.BalanceConsumed = true
			if err := markAsyncUsageBalanceConsumed(info); err != nil {
				if replaySafe {
					return fmt.Errorf(
						"%w: persist consumed balance: %v",
						errAsyncUsageSettlementPending,
						err,
					)
				}
				notify.ErrorThrottle(
					"asyncUsageMarkBalanceConsumed",
					time.Minute*5,
					"persist non-replay-safe async usage debit marker",
					err.Error(),
				)
				log.Errorf(
					"persist non-replay-safe async usage debit marker failed; completing with in-memory consumed marker to avoid duplicate charge: id=%d request_id=%s upstream_id=%s amount=%f err=%v",
					info.ID,
					info.RequestID,
					info.UpstreamID,
					amount.UsedAmount,
					err,
				)
			}
		}
	}

	info.Status = model.AsyncUsageStatusCompleted
	info.Usage = usage
	info.UsageContext = usageContext
	info.Amount = amount
	info.Error = ""

	completed, err := completeClaimedAsyncUsageInfo(info, usage, usageContext, amount)
	if err != nil {
		if nonReplaySafeDebitAttempted {
			return fmt.Errorf(
				"%w: non-replay-safe debit succeeded but completion marker failed: %v",
				errAsyncUsageManualSettlement,
				err,
			)
		}

		return fmt.Errorf("update async usage info: %w", err)
	}

	if !completed {
		if nonReplaySafeDebitAttempted {
			return fmt.Errorf(
				"%w: non-replay-safe debit was attempted but async usage claim was lost",
				errAsyncUsageManualSettlement,
			)
		}

		return errors.New("async usage claim lost")
	}

	model.BatchUpdateSummaryOnlyUsage(
		time.Now(),
		info.RequestAt,
		info.GroupID,
		info.ChannelID,
		info.Model,
		info.TokenID,
		info.TokenName,
		usage,
		amount,
		usageContext.ServiceTier,
		model.IsClaudeLongContextSummary(info.Model, usage),
	)

	return nil
}

func asyncUsageNeedsMetrics(
	price model.Price,
	usage model.Usage,
	amount model.Amount,
) bool {
	if price.PerRequestPrice > 0 {
		return false
	}
	if amount.UsedAmount > 0 {
		return price.VideoInputPrice > 0 && usage.VideoInputTokens <= 0 ||
			price.OutputPrice > 0 && usage.OutputTokens <= 0
	}

	return price.InputPrice > 0 ||
		price.ImageInputPrice > 0 ||
		price.AudioInputPrice > 0 ||
		price.VideoInputPrice > 0 ||
		price.OutputPrice > 0 ||
		price.ImageOutputPrice > 0 ||
		price.AudioOutputPrice > 0 ||
		price.ThinkingModeOutputPrice > 0 ||
		price.CachedPrice > 0 ||
		price.CacheCreationPrice > 0 ||
		price.WebSearchPrice > 0
}

func scheduleAsyncUsageRetry(info *model.AsyncUsageInfo, err error) {
	info.RetryCount++
	info.Error = err.Error()
	info.NextPollAt = time.Now().Add(model.AsyncUsageBackoffDelay(info.RetryCount))

	if info.RetryCount >= asyncUsageMaxRetry {
		log.Debugf(
			"async usage poll: fail id=%d request_id=%s upstream_id=%s reason=max_retry retry=%d next_poll_at=%s err=%v",
			info.ID,
			info.RequestID,
			info.UpstreamID,
			info.RetryCount,
			info.NextPollAt.Format(time.RFC3339),
			err,
		)
		markAsyncUsageFailed(info, "max retry exceeded: "+err.Error())

		return
	}

	log.Debugf(
		"async usage poll: retry id=%d request_id=%s upstream_id=%s retry=%d next_poll_at=%s err=%v",
		info.ID,
		info.RequestID,
		info.UpstreamID,
		info.RetryCount,
		info.NextPollAt.Format(time.RFC3339),
		err,
	)

	if updateErr := model.RetryClaimedAsyncUsageInfo(info); updateErr != nil {
		notify.ErrorThrottle(
			"asyncUsageUpdateRetry",
			time.Minute*5,
			"update async usage retry failed",
			updateErr.Error(),
		)
	}
}

func scheduleAsyncUsageSettlementRetry(info *model.AsyncUsageInfo) {
	info.Error = ""
	info.NextPollAt = time.Now().Add(model.AsyncUsageMaxPollDelay)

	if updateErr := model.RetryClaimedAsyncUsageInfo(info); updateErr != nil {
		notify.ErrorThrottle(
			"asyncUsageUpdateSettlementRetry",
			time.Minute*5,
			"update async usage settlement retry failed",
			updateErr.Error(),
		)
	}
}

func parkAsyncUsageSettlement(info *model.AsyncUsageInfo, err error) {
	info.Error = err.Error()
	info.NextPollAt = time.Now().AddDate(100, 0, 0)

	if updateErr := model.RetryClaimedAsyncUsageInfo(info); updateErr != nil {
		notify.ErrorThrottle(
			"asyncUsageParkSettlement",
			time.Minute*5,
			"park non-replay-safe async usage settlement",
			updateErr.Error(),
		)
	}
}

func touchAsyncUsagePollCursor(info *model.AsyncUsageInfo) {
	info.Error = ""
	info.NextPollAt = time.Now().Add(model.AsyncUsageDefaultPollDelay)

	if err := model.TouchClaimedAsyncUsageInfo(info); err != nil {
		notify.ErrorThrottle(
			"asyncUsageTouchPending",
			time.Minute*5,
			"touch pending async usage failed",
			err.Error(),
		)
	}
}

func consumeAsyncUsageGroupBalance(
	ctx context.Context,
	info *model.AsyncUsageInfo,
	amount float64,
) (bool, bool, error) {
	if balance.Default == nil || info.GroupID == "" {
		return false, false, nil
	}

	group, err := model.CacheGetGroup(info.GroupID)
	if err != nil {
		return false, false, fmt.Errorf("get group: %w", err)
	}

	ctx = context.WithValue(ctx, balance.CtxRequestID, info.RequestID)
	ctx = balance.ContextWithPricing(ctx, info.PricingCurrency, info.PricingVersion)

	_, consumer, err := balance.Default.GetGroupRemainBalance(ctx, *group)
	if err != nil {
		return false, false, fmt.Errorf("get group balance: %w", err)
	}

	if consumer == nil {
		return false, false, nil
	}

	replaySafe := false
	if replayable, ok := consumer.(balance.ReplaySafePostGroupConsumer); ok {
		replaySafe = replayable.CanReplayPostGroupConsume(ctx)
	}
	if !replaySafe && info.BalanceConsumeAttempted {
		return false, false, fmt.Errorf(
			"%w: non-replay-safe debit was already attempted",
			errAsyncUsageManualSettlement,
		)
	}
	if !replaySafe && info.ID != 0 && info.ProcessingToken != "" {
		if err := markAsyncUsageBalanceAttempted(info); err != nil {
			return false, false, fmt.Errorf(
				"persist non-replay-safe debit attempt before charge: %w",
				err,
			)
		}
		info.BalanceConsumeAttempted = true
	}

	_, err = consumer.PostGroupConsume(ctx, info.TokenName, amount)

	return true, replaySafe, err
}

func recordAsyncUsageConsumeError(
	info *model.AsyncUsageInfo,
	amount float64,
	err error,
) {
	if err := model.CreateConsumeError(
		info.RequestID,
		info.RequestAt,
		info.GroupID,
		info.TokenName,
		info.Model,
		err.Error(),
		amount,
		info.TokenID,
	); err != nil {
		log.Error("failed to create async usage consume error: " + err.Error())
	}
}

func markAsyncUsageFailed(info *model.AsyncUsageInfo, errMsg string) {
	info.Status = model.AsyncUsageStatusFailed
	info.Error = errMsg

	updated, err := model.FailClaimedAsyncUsageInfo(info)
	if err != nil {
		notify.ErrorThrottle(
			"asyncUsageMarkFailed",
			time.Minute*5,
			"mark async usage failed",
			err.Error(),
		)

		return
	}

	if !updated {
		log.Debugf(
			"async usage poll: skip mark failed after claim lost id=%d request_id=%s upstream_id=%s",
			info.ID,
			info.RequestID,
			info.UpstreamID,
		)

		return
	}

	if err := model.IgnoreNotFound(
		model.UpdateLogAsyncUsageFailedByRequestID(info.RequestID, errMsg),
	); err != nil {
		notify.ErrorThrottle(
			"asyncUsageUpdateLogStatus",
			time.Minute*5,
			"update async usage log failure failed",
			err.Error(),
		)
	}
}

const (
	// KeyRedisConnection is the oncall key prefix for redis connection errors
	KeyRedisConnection = "redis_connection_error"
	// RedisHealthCheckInterval is how often to ping redis
	RedisHealthCheckInterval = 30 * time.Second
	// RedisErrorPersistDuration is how long redis errors must persist before triggering urgent alert
	RedisErrorPersistDuration = 2 * time.Minute
)

// RedisHealthCheckTask monitors Redis connection health
func RedisHealthCheckTask(ctx context.Context) {
	ticker := time.NewTicker(RedisHealthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkRedisHealth()
		}
	}
}

func checkRedisHealth() {
	if !common.RedisEnabled {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := common.RDB.Ping(ctx).Result()
	if err != nil {
		oncall.Alert(
			KeyRedisConnection,
			RedisErrorPersistDuration,
			"Redis Connection Error",
			"Redis ping failed: "+err.Error(),
		)

		return
	}

	// Clear error state if ping succeeds
	oncall.Clear(KeyRedisConnection)
}
