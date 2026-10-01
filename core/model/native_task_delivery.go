package model

import (
	"encoding/json"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"gorm.io/gorm"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Each archived artifact is checkpointed before continuing to the next file.
// A crash can replay an upload, but cannot mark a partially archived task ready.
func SaveNativeArtifact(db *gorm.DB, id, group string, token, index int, receipt ownedartifact.Receipt) error {
	task, err := GetNativeTask(db, id, group, token)
	if err != nil {
		return err
	}
	if task.Status != "result_received" {
		return ErrNativeTaskConflict
	}
	var contract nativeresult.TaskContract
	if json.Unmarshal([]byte(task.FrozenContract), &contract) != nil {
		return ErrNativeTaskConflict
	}
	plan, err := nativeresult.PlanArtifacts([]byte(task.NativeOutput), contract.Artifacts)
	if err != nil {
		return err
	}
	if index < 0 || index >= len(plan) || !ownedartifact.ValidReceipt(id, index, receipt) {
		return ErrNativeTaskConflict
	}
	entries := map[int]ownedartifact.Receipt{}
	if task.ArtifactManifest != "" && json.Unmarshal([]byte(task.ArtifactManifest), &entries) != nil {
		return ErrNativeTaskConflict
	}
	if previous, ok := entries[index]; ok {
		if previous == receipt {
			return nil
		}
		return ErrNativeTaskConflict
	}
	entries[index] = receipt
	raw, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	r := db.Model(&NativeTask{}).Where("id = ? AND group_id = ? AND token_id = ? AND status = ? AND artifact_manifest = ?", id, group, token, "result_received", task.ArtifactManifest).Updates(map[string]any{"artifact_manifest": string(raw), "updated_at": time.Now()})
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected != 1 {
		return ErrNativeTaskConflict
	}
	return nil
}
func PrepareNativeDelivery(db *gorm.DB, id, group string, token int) error {
	task, err := GetNativeTask(db, id, group, token)
	if err != nil {
		return err
	}
	if task.Status == "delivery_ready" || task.Status == "completed" {
		return nil
	}
	if task.Status != "result_received" {
		return ErrNativeTaskConflict
	}
	var contract nativeresult.TaskContract
	if json.Unmarshal([]byte(task.FrozenContract), &contract) != nil {
		return ErrNativeTaskConflict
	}
	artifacts, err := nativeresult.PlanArtifacts([]byte(task.NativeOutput), contract.Artifacts)
	if err != nil {
		return err
	}
	entries := map[int]ownedartifact.Receipt{}
	if task.ArtifactManifest != "" && json.Unmarshal([]byte(task.ArtifactManifest), &entries) != nil {
		return ErrNativeTaskConflict
	}
	if len(entries) != len(artifacts) {
		return ErrNativeTaskConflict
	}
	replacements := map[string]string{}
	if len(artifacts) > 0 {
		u, err := url.Parse(task.DeliveryBase)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return ErrNativeTaskConflict
		}
	}
	for index, artifact := range artifacts {
		if !ownedartifact.ValidReceipt(id, index, entries[index]) {
			return ErrNativeTaskConflict
		}
		replacements[artifact.Pointer] = strings.TrimRight(task.DeliveryBase, "/") + "/v1/model-tasks/" + url.PathEscape(id) + "/artifacts/" + strconv.Itoa(index)
	}
	output, err := nativeresult.RewriteArtifacts([]byte(task.NativeOutput), contract.Artifacts, replacements)
	if err != nil {
		return err
	}
	validator, err := nativeresult.Compile(contract.OutputSchema)
	if err != nil {
		return err
	}
	if _, err = validator.Validate(output); err != nil {
		return err
	}
	r := db.Model(&NativeTask{}).Where("id = ? AND group_id = ? AND token_id = ? AND status = ? AND artifact_manifest = ?", id, group, token, "result_received", task.ArtifactManifest).Updates(map[string]any{"delivered_output": string(output), "status": "delivery_ready", "updated_at": time.Now()})
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected != 1 {
		return ErrNativeTaskConflict
	}
	return nil
}
func CompleteNativeTask(db *gorm.DB, id, group string, token int) error {
	r := db.Model(&NativeTask{}).Where("id = ? AND group_id = ? AND token_id = ? AND status = ?", id, group, token, "delivery_ready").Updates(map[string]any{"status": "completed", "updated_at": time.Now()})
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected == 1 {
		return nil
	}
	t, err := GetNativeTask(db, id, group, token)
	if err == nil && t.Status == "completed" {
		return nil
	}
	return ErrNativeTaskConflict
}
