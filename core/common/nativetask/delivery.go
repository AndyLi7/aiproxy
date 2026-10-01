package nativetask

import (
	"context"
	"encoding/json"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/model"
)

type ArchiveFunc func(context.Context, string, int, string) (ownedartifact.Receipt, error)

// Deliver resumes the immutable received result. It never submits a generation.
func (e *Engine) Deliver(ctx context.Context, id, group string, token int, archive ArchiveFunc) (*model.NativeTask, error) {
	if e == nil || e.DB == nil || e.Wallet == nil {
		return nil, ErrUnavailable
	}
	task, err := model.GetNativeTask(e.DB, id, group, token)
	if err != nil {
		return nil, err
	}
	if task.Status == "completed" || task.Status == "delivery_ready" {
		return task, e.SyncBilling(ctx, task)
	}
	if task.Status != "result_received" {
		return task, ErrPending
	}
	if err = e.SyncBilling(ctx, task); err != nil {
		return task, err
	}
	var contract nativeresult.TaskContract
	if json.Unmarshal([]byte(task.FrozenContract), &contract) != nil {
		return task, ErrUnavailable
	}
	artifacts, err := nativeresult.PlanArtifacts([]byte(task.NativeOutput), contract.Artifacts)
	if err != nil {
		return task, err
	}
	entries := map[int]ownedartifact.Receipt{}
	if task.ArtifactManifest != "" && json.Unmarshal([]byte(task.ArtifactManifest), &entries) != nil {
		return task, ErrUnavailable
	}
	if archive == nil {
		archive = ownedartifact.Store
	}
	for index, a := range artifacts {
		if _, done := entries[index]; done {
			continue
		}
		receipt, err := archive(ctx, id, index, a.Source)
		if err != nil {
			return task, err
		}
		if err = model.SaveNativeArtifact(e.DB, id, group, token, index, receipt); err != nil {
			return task, err
		}
	}
	if err = model.PrepareNativeDelivery(e.DB, id, group, token); err != nil {
		return task, err
	}
	task, err = model.GetNativeTask(e.DB, id, group, token)
	if err != nil {
		return nil, err
	}
	return task, e.SyncBilling(ctx, task)
}
