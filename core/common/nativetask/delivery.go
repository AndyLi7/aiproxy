package nativetask

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/model"
	log "github.com/sirupsen/logrus"
)

type ArchiveFunc func(context.Context, string, int, string) (ownedartifact.Receipt, error)

// archiveBudget bounds archiving one task's files: 256 MiB at about half a
// megabyte a second. A stalled transfer ends sooner (ownedartifact's stall
// timeout); an unfinished one is retried by a later recovery pass.
const archiveBudget = ownedartifact.ArchiveBudget

type (
	deliveryLeaseKey struct{}
	deliveryLease    struct {
		owner  string
		parent context.Context //nolint:containedctx // A delivery lease carries the recovery invocation parent so cancellation survives the archive timeout.
	}
)

// Deliver resumes the immutable received result. It never submits a generation.
func (e *Engine) Deliver(
	ctx context.Context,
	id, group string,
	token int,
	archive ArchiveFunc,
) (*model.NativeTask, error) {
	if e == nil || e.DB == nil || e.Wallet == nil {
		return nil, ErrUnavailable
	}

	task, err := model.GetNativeTask(e.DB, id, group, token)
	if err != nil {
		return nil, err
	}

	return e.runTaskExecution(
		ctx,
		task,
		model.NativeExecutionDelivery,
		func() (*model.NativeTask, error) {
			return e.deliverInvocation(ctx, task, id, group, token, archive)
		},
	)
}

//nolint:gocyclo // The delivery state machine keeps ownership checks, per-artifact persistence and billing in their required order.
func (e *Engine) deliverInvocation(
	ctx context.Context,
	task *model.NativeTask,
	id, group string,
	token int,
	archive ArchiveFunc,
) (*model.NativeTask, error) {
	var err error

	if task.Status == "completed" {
		return task, e.SyncBilling(ctx, task)
	}

	if task.Status != "result_received" && task.Status != "delivery_ready" {
		return task, ErrPending
	}
	// RecoverOnce already owns a lease. Direct callers claim the same columns,
	// so they cannot archive or confirm in parallel with a recovery worker.
	lease, recovering := ctx.Value(deliveryLeaseKey{}).(deliveryLease)
	if !recovering {
		var tokenBytes [16]byte
		if _, err := rand.Read(tokenBytes[:]); err != nil {
			return task, err
		}

		lease = deliveryLease{
			owner:  "artifact-" + hex.EncodeToString(tokenBytes[:]),
			parent: ctx,
		}
		if err := model.ClaimNativeDelivery(
			e.DB,
			id,
			group,
			token,
			lease.owner,
			time.Now().Add(archiveBudget+time.Minute),
		); err != nil {
			return task, err
		}
		defer func() {
			if releaseErr := model.ReleaseNativeRecovery(
				e.DB,
				id,
				lease.owner,
				time.Now(),
			); releaseErr != nil {
				log.WithError(releaseErr).
					WithField("task_id", id).
					Warn("native delivery lease release failed")
			}
		}()
	} else if err := model.ExtendNativeRecovery(e.DB, id, lease.owner, time.Now().Add(archiveBudget+time.Minute)); err != nil {
		return task, err
	}

	workCtx, cancel := context.WithTimeout(lease.parent, archiveBudget)
	defer cancel()

	ctx = model.WithNativeRecoveryOwner(workCtx, lease.owner)
	if err := model.CheckNativeRecovery(e.DB.WithContext(ctx), id); err != nil {
		return task, err
	}

	if task.Status == "delivery_ready" {
		return task, e.SyncBilling(ctx, task)
	}

	if err = e.SyncBilling(ctx, task); err != nil {
		return task, err
	}

	var contract nativeresult.TaskContract
	if json.Unmarshal([]byte(task.FrozenContract), &contract) != nil {
		return task, ErrUnavailable
	}

	artifacts, err := nativeresult.PlanArtifacts(
		[]byte(task.NativeOutput),
		contract.Artifacts,
	)
	if permanent(err) {
		return e.rejectResult(ctx, id, group, token)
	}

	if err != nil {
		return task, err
	}

	entries := map[int]ownedartifact.Receipt{}
	if task.ArtifactManifest != "" &&
		json.Unmarshal([]byte(task.ArtifactManifest), &entries) != nil {
		return task, ErrUnavailable
	}

	if archive == nil {
		archive = ownedartifact.Store
	}

	if len(entries) < len(artifacts) {
		for index, a := range artifacts {
			if _, done := entries[index]; done {
				continue
			}

			receipt, err := archive(ctx, id, index, a.Source)
			if ctx.Err() != nil {
				return task, ctx.Err()
			}

			if checkErr := model.CheckNativeRecovery(
				e.DB.WithContext(ctx),
				id,
			); checkErr != nil {
				return task, checkErr
			}

			if permanent(err) {
				fields := log.Fields{
					"lane":     "native",
					"task_id":  id,
					"group":    group,
					"model":    task.Model,
					"artifact": index,
				}
				if errors.Is(err, ownedartifact.ErrTooLarge) {
					log.WithFields(fields).
						Warn("native result file exceeds the archive limit; result rejected and refunded")
				} else {
					log.WithFields(fields).
						WithError(err).
						Warn("native result file cannot be archived; result rejected and refunded")
				}

				return e.rejectResult(ctx, id, group, token)
			}

			if err != nil {
				return task, err
			}

			if err = model.SaveNativeArtifact(
				e.DB.WithContext(ctx),
				id,
				group,
				token,
				index,
				receipt,
			); err != nil {
				return task, err
			}
		}
	}

	if err = model.PrepareNativeDelivery(
		e.DB.WithContext(ctx),
		id,
		group,
		token,
	); err != nil {
		if permanent(err) {
			return e.rejectResult(ctx, id, group, token)
		}
		return task, err
	}

	task, err = model.GetNativeTask(e.DB, id, group, token)
	if err != nil {
		return nil, err
	}

	if err := model.CheckNativeRecovery(e.DB.WithContext(ctx), id); err != nil {
		return task, err
	}

	return task, e.SyncBilling(ctx, task)
}
