package model

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/registryvalidation"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	ChannelRoutingSource          = "gateway-channel-routing-v1"
	routingMaxSafe          int64 = 9007199254740991
	ChannelRoutingBodyLimit       = 512 * 1024
)

type ChannelRouting struct {
	Name             string                                        `json:"name"`
	Models           []string                                      `json:"models"`
	ModelMapping     map[string]string                             `json:"modelMapping"`
	Sets             []string                                      `json:"sets"`
	Priority         int32                                         `json:"priority"`
	BackupOnly       bool                                          `json:"backupOnly"`
	ProviderBindings map[string]registryvalidation.ProviderBinding `json:"providerBindings"`
}
type ChannelRoutingSnapshot struct {
	Identity    ChannelIdentity `json:"identity"`
	Status      int             `json:"status"`
	Revision    string          `json:"revision"`
	RoutingHash string          `json:"routingHash"`
	Routing     ChannelRouting  `json:"routing"`
}
type ChannelRoutingRequest struct {
	Version             int             `json:"version"`
	RequestID           string          `json:"requestId"`
	ExpectedIdentity    ChannelIdentity `json:"expectedIdentity"`
	ExpectedRevision    string          `json:"expectedRevision"`
	ExpectedRoutingHash string          `json:"expectedRoutingHash"`
	NotAfterMs          string          `json:"notAfterMs"`
	Routing             ChannelRouting  `json:"routing"`
}
type ChannelRoutingReceiptView struct {
	RequestID     string                 `json:"requestId"`
	RequestHash   string                 `json:"requestHash"`
	Request       ChannelRoutingRequest  `json:"request"`
	Before        ChannelRoutingSnapshot `json:"before"`
	After         ChannelRoutingSnapshot `json:"after"`
	CommittedAtMs string                 `json:"committedAtMs"`
}
type ChannelRoutingResult struct {
	Version      int                        `json:"version"`
	Source       string                     `json:"source"`
	State        string                     `json:"state"`
	Reason       string                     `json:"reason"`
	ObservedAtMs *string                    `json:"observedAtMs"`
	Snapshot     *ChannelRoutingSnapshot    `json:"snapshot"`
	Receipt      *ChannelRoutingReceiptView `json:"receipt"`
}

func routingResult(state, reason string) ChannelRoutingResult {
	return ChannelRoutingResult{
		Version: 1,
		Source:  ChannelRoutingSource,
		State:   state,
		Reason:  reason,
	}
}

func routingDecimal(s string) (int64, bool) {
	n, e := strconv.ParseInt(s, 10, 64)
	return n, e == nil && n > 0 && n <= routingMaxSafe && strconv.FormatInt(n, 10) == s
}

func routingFields(raw []byte, fields ...string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || len(m) != len(fields) {
		return false
	}

	for _, k := range fields {
		if v, ok := m[k]; !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return false
		}
	}

	return true
}

func ParseChannelRoutingRequest(raw []byte) (q ChannelRoutingRequest, e error) {
	if len(raw) == 0 || len(raw) > ChannelRoutingBodyLimit || migrationUniqueJSON(raw) != nil ||
		!routingFields(
			raw,
			"version",
			"requestId",
			"expectedIdentity",
			"expectedRevision",
			"expectedRoutingHash",
			"notAfterMs",
			"routing",
		) {
		return q, ErrChannelAdmissionConflict
	}

	var m map[string]json.RawMessage

	_ = json.Unmarshal(raw, &m)
	if !routingFields(m["expectedIdentity"], "channelId", "keyDigest", "identityHash") ||
		!routingFields(
			m["routing"],
			"name",
			"models",
			"modelMapping",
			"sets",
			"priority",
			"backupOnly",
			"providerBindings",
		) {
		return q, ErrChannelAdmissionConflict
	}

	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()

	if d.Decode(&q) != nil || d.Decode(new(any)) != io.EOF {
		return q, ErrChannelAdmissionConflict
	}

	if !validRoutingRequest(q) {
		return q, ErrChannelAdmissionConflict
	}

	return q, nil
}

func validRoutingRequest(q ChannelRoutingRequest) bool {
	_, rev := routingDecimal(q.ExpectedRevision)
	_, until := routingDecimal(q.NotAfterMs)

	p := q.Routing
	if invalidRoutingRequestFields(p, q, rev, until) {
		return false
	}

	for _, list := range [][]string{p.Models, p.Sets} {
		seen := map[string]bool{}
		for _, v := range list {
			if v == "" || len(v) > 1024 || seen[v] {
				return false
			}

			seen[v] = true
		}
	}

	for k, v := range p.ModelMapping {
		if k == "" || v == "" || len(k) > 1024 || len(v) > 1024 {
			return false
		}
	}

	for k, v := range p.ProviderBindings {
		if k == "" || len(k) > 1024 || v.Provider == "" || len(v.Provider) > 128 || v.ID == "" ||
			len(v.ID) > 512 ||
			v.Revision == "" ||
			len(v.Revision) > 128 ||
			!registryvalidation.ValidProviderBindingIdentity(v) {
			return false
		}
	}

	return true
}

func rawChannelConfigs(tx *gorm.DB, id int) (string, error) {
	var raw sql.NullString

	e := tx.Table("channels").Select("configs").Where("id=?", id).Row().Scan(&raw)
	return raw.String, e
}

func routingBindings(raw string) (map[string]registryvalidation.ProviderBinding, error) {
	out := map[string]registryvalidation.ProviderBinding{}
	if strings.TrimSpace(raw) == "" || strings.TrimSpace(raw) == "null" {
		return out, nil
	}

	if migrationUniqueJSON([]byte(raw)) != nil {
		return nil, ErrChannelAdmissionUnavailable
	}

	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &m) != nil || m == nil {
		return nil, ErrChannelAdmissionUnavailable
	}

	value, ok := m[ChannelConfigProviderBindings]
	if !ok {
		return out, nil
	}

	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return nil, ErrChannelAdmissionUnavailable
	}

	var bindings map[string]json.RawMessage
	if json.Unmarshal(value, &bindings) != nil || bindings == nil {
		return nil, ErrChannelAdmissionUnavailable
	}

	for _, v := range bindings {
		if !routingFields(v, "provider", "id", "revision", "contractHash") {
			return nil, ErrChannelAdmissionUnavailable
		}
	}

	d := json.NewDecoder(bytes.NewReader(value))
	d.DisallowUnknownFields()

	if d.Decode(&out) != nil {
		return nil, ErrChannelAdmissionUnavailable
	}

	return out, nil
}

// Replace just one top-level value by token offsets, preserving all other raw
// configs bytes, unknown number precision and identity/credential fields.
func replaceRoutingBindings(
	raw string,
	bindings map[string]registryvalidation.ProviderBinding,
) (string, error) {
	replacement := routingJSON(bindings)
	if strings.TrimSpace(raw) == "" || strings.TrimSpace(raw) == "null" {
		return "{\"" + ChannelConfigProviderBindings + "\":" + replacement + "}", nil
	}

	if migrationUniqueJSON([]byte(raw)) != nil {
		return "", ErrChannelAdmissionUnavailable
	}

	d := json.NewDecoder(strings.NewReader(raw))

	tok, e := d.Token()
	if e != nil || tok != json.Delim('{') {
		return "", ErrChannelAdmissionUnavailable
	}

	fields := 0
	for d.More() {
		key, e := d.Token()
		if e != nil {
			return "", e
		}

		start := int(d.InputOffset())
		for start < len(raw) && raw[start] != ':' {
			start++
		}

		start++
		for start < len(raw) && strings.ContainsRune(" \n\r\t", rune(raw[start])) {
			start++
		}

		var value json.RawMessage
		if e = d.Decode(&value); e != nil {
			return "", e
		}

		end := int(d.InputOffset())
		if key == ChannelConfigProviderBindings {
			return raw[:start] + replacement + raw[end:], nil
		}

		fields++
	}

	end := int(d.InputOffset())
	for end < len(raw) && raw[end] != '}' {
		end++
	}

	if end == len(raw) {
		return "", ErrChannelAdmissionUnavailable
	}

	comma := ""
	if fields > 0 {
		comma = ","
	}

	return raw[:end] + comma + routingJSON(
		ChannelConfigProviderBindings,
	) + ":" + replacement + raw[end:], nil
}

func routingSnapshot(tx *gorm.DB, ch *Channel) (*ChannelRoutingSnapshot, error) {
	var row ChannelRoutingState
	if e := tx.First(&row, "channel_id=?", ch.ID).Error; e != nil {
		return nil, e
	}

	if row.Revision <= 0 || row.Revision > routingMaxSafe {
		return nil, ErrChannelAdmissionUnavailable
	}

	identity, e := ChannelIdentityOf(ch)
	if e != nil {
		return nil, e
	}

	raw, e := rawChannelConfigs(tx, ch.ID)
	if e != nil {
		return nil, e
	}

	bindings, e := routingBindings(raw)
	if e != nil {
		return nil, e
	}

	p := ChannelRouting{
		ch.Name,
		append([]string{}, ch.Models...),
		ch.ModelMapping,
		append([]string{}, ch.Sets...),
		ch.Priority,
		ch.BackupOnly,
		bindings,
	}
	if p.ModelMapping == nil {
		p.ModelMapping = map[string]string{}
	}

	if !inventorySafeJSON([]byte(routingJSON(p)), []string{ch.Key}, 0) {
		return nil, ErrChannelAdmissionUnavailable
	}

	hash, e := admissionHash(struct {
		Status  int
		Routing ChannelRouting
	}{ch.Status, p})
	if e != nil {
		return nil, e
	}

	return &ChannelRoutingSnapshot{
		identity,
		ch.Status,
		strconv.FormatInt(row.Revision, 10),
		hash,
		p,
	}, nil
}

func routingReceiptView(row ChannelRoutingReceipt) (*ChannelRoutingReceiptView, error) {
	var (
		q             ChannelRoutingRequest
		before, after ChannelRoutingSnapshot
	)
	if json.Unmarshal([]byte(row.RequestJSON), &q) != nil || !validRoutingRequest(q) ||
		json.Unmarshal([]byte(row.BeforeJSON), &before) != nil ||
		json.Unmarshal([]byte(row.AfterJSON), &after) != nil {
		return nil, ErrChannelAdmissionUnavailable
	}

	h, _ := admissionHash(q)
	b, bok := routingDecimal(before.Revision)
	a, aok := routingDecimal(after.Revision)
	deadline, _ := routingDecimal(q.NotAfterMs)
	beforeHash, _ := admissionHash(struct {
		Status  int
		Routing ChannelRouting
	}{before.Status, before.Routing})

	afterHash, _ := admissionHash(struct {
		Status  int
		Routing ChannelRouting
	}{after.Status, after.Routing})
	if beforeHash != before.RoutingHash || afterHash != after.RoutingHash {
		return nil, ErrChannelAdmissionUnavailable
	}

	if h != row.RequestHash || row.RequestID != q.RequestID ||
		row.ChannelID != q.ExpectedIdentity.ChannelID ||
		before.Identity != q.ExpectedIdentity ||
		after.Identity != before.Identity ||
		before.Revision != q.ExpectedRevision ||
		before.RoutingHash != q.ExpectedRoutingHash ||
		!bok ||
		!aok ||
		a != b+1 ||
		routingJSON(after.Routing) != routingJSON(q.Routing) ||
		row.CommittedAtMs <= 0 ||
		row.CommittedAtMs >= deadline ||
		after.Status != before.Status {
		return nil, ErrChannelAdmissionUnavailable
	}

	return &ChannelRoutingReceiptView{
		row.RequestID,
		row.RequestHash,
		q,
		before,
		after,
		strconv.FormatInt(row.CommittedAtMs, 10),
	}, nil
}

func ReadChannelRouting(ctx context.Context, id int, requestID string) ChannelRoutingResult {
	out := routingResult("unavailable", "routing_storage_unavailable")
	if DB == nil {
		return out
	}

	if DB.Name() != "sqlite" && DB.Name() != "postgres" {
		return routingResult("unsupported", "routing_storage_unsupported")
	}

	e := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// SQLite has no FOR SHARE. Take the writer reservation before ANY read
		// establishes a WAL snapshot; clock+absence must serialize with PATCH.
		if tx.Name() == "sqlite" {
			r := tx.Unscoped().
				Model(&Channel{}).
				Where("id=?", id).
				UpdateColumn("id", gorm.Expr("id"))
			if r.Error != nil {
				return r.Error
			}

			if r.RowsAffected != 1 {
				return ErrChannelAdmissionConflict
			}
		}

		ready, e := channelRoutingStorageReady(tx)
		if e != nil {
			return e
		}

		if !ready {
			return ErrChannelAdmissionUnavailable
		}

		var ch Channel

		e = tx.Unscoped().Clauses(clause.Locking{Strength: "SHARE"}).First(&ch, id).Error
		if e != nil {
			return e
		}

		snap, e := routingSnapshot(tx, &ch)
		if e != nil {
			return e
		}

		out = routingResult("ready", "")

		now, e := channelAdmissionTime(tx)
		if e != nil {
			return e
		}

		observed := strconv.FormatInt(now.UnixMilli(), 10)

		out.ObservedAtMs = &observed
		if !ch.DeletedAt.Valid {
			out.Snapshot = snap
		} else if requestID == "" {
			return ErrChannelAdmissionConflict
		}

		if requestID != "" {
			var row ChannelRoutingReceipt

			e = tx.First(&row, "request_id=?", requestID).Error
			if errors.Is(e, gorm.ErrRecordNotFound) {
				out.State = "absent"
				out.Reason = "request_not_recorded"
				return nil
			}

			if e != nil {
				return e
			}

			if row.ChannelID != id {
				return ErrChannelAdmissionConflict
			}

			out.Receipt, e = routingReceiptView(row)

			return e
		}

		return nil
	})
	if e != nil {
		return routingError(e)
	}

	return out
}

func routingError(e error) ChannelRoutingResult {
	if errors.Is(e, ErrChannelAdmissionConflict) || errors.Is(e, ErrChannelAdmissionRetained) {
		return routingResult("conflict", "routing_changed_or_protected")
	}
	return routingResult("unavailable", "routing_storage_unavailable")
}

//nolint:gocyclo // Routing CAS validates request, immutable identity, access ownership, updated snapshot, and replay receipt within one transaction.
func PatchChannelRouting(
	ctx context.Context,
	id int,
	q ChannelRoutingRequest,
) ChannelRoutingResult {
	if !validRoutingRequest(q) || q.ExpectedIdentity.ChannelID != id {
		return routingResult("conflict", "routing_request_mismatch")
	}

	if DB == nil {
		return routingError(ErrChannelAdmissionUnavailable)
	}

	if DB.Name() != "sqlite" && DB.Name() != "postgres" {
		return routingResult("unsupported", "routing_storage_unsupported")
	}

	out := routingResult("unavailable", "routing_write_unknown")

	e := routingWriteTransaction(ctx, func(tx *gorm.DB) error {
		ready, e := channelRoutingStorageReady(tx)
		if e != nil {
			return e
		}

		if !ready {
			return ErrChannelAdmissionUnavailable
		}

		ch, e := lockRoutingChannel(tx, id)
		if e != nil {
			return e
		}

		snap, e := routingSnapshot(tx, ch)
		if e != nil {
			return e
		}

		h, _ := admissionHash(q)

		var old ChannelRoutingReceipt

		e = tx.First(&old, "request_id=?", q.RequestID).Error
		if e == nil {
			if old.ChannelID != id || old.RequestHash != h {
				return ErrChannelAdmissionConflict
			}

			receipt, e := routingReceiptView(old)
			if e != nil {
				return e
			}

			out = routingResult("ready", "")
			out.Snapshot = snap
			out.Receipt = receipt

			now, e := channelAdmissionTime(tx)
			if e != nil {
				return e
			}

			observed := strconv.FormatInt(now.UnixMilli(), 10)
			out.ObservedAtMs = &observed

			return nil
		}

		if !errors.Is(e, gorm.ErrRecordNotFound) {
			return e
		}

		if snap.Identity != q.ExpectedIdentity || snap.Revision != q.ExpectedRevision ||
			snap.RoutingHash != q.ExpectedRoutingHash {
			return ErrChannelAdmissionConflict
		}

		deadline, _ := routingDecimal(q.NotAfterMs)

		now, e := channelAdmissionTime(tx)
		if e != nil {
			return e
		}

		if now.UnixMilli() >= deadline {
			return ErrChannelAdmissionConflict
		}

		if e = guardPreparedChannelMutation(tx, []int{id}); e != nil {
			return e
		}
		// Only explicitly provisioned access configurations may return to an
		// empty routing set (e.g. compensation of their first publication).
		if len(q.Routing.Models) == 0 &&
			(len(q.Routing.ModelMapping) != 0 || len(q.Routing.ProviderBindings) != 0 || !accessConfigurationOwnsChannel(tx, id)) {
			return ErrChannelAdmissionConflict
		}

		if !config.DisableModelConfig {
			var count int64
			if e = tx.Model(&ModelConfig{}).
				Where("model IN ?", q.Routing.Models).
				Count(&count).
				Error; e != nil {
				return e
			}

			if count != int64(len(q.Routing.Models)) {
				return ErrChannelAdmissionConflict
			}
		}

		raw, e := rawChannelConfigs(tx, id)
		if e != nil {
			return e
		}

		next, e := replaceRoutingBindings(raw, q.Routing.ProviderBindings)
		if e != nil {
			return e
		}

		if e = bumpChannelRoutingRevision(tx, id); e != nil {
			return e
		}

		p := q.Routing
		if e = tx.Model(&Channel{}).
			Where("id=?", id).
			Updates(map[string]any{"name": p.Name, "models": routingJSON(p.Models), "model_mapping": routingJSON(p.ModelMapping), "sets": routingJSON(p.Sets), "priority": p.Priority, "backup_only": p.BackupOnly, "configs": next}).
			Error; e != nil {
			return e
		}

		if e = tx.First(ch, id).Error; e != nil {
			return e
		}

		after, e := routingSnapshot(tx, ch)
		if e != nil {
			return e
		}

		if after.Identity != q.ExpectedIdentity {
			return ErrChannelAdmissionConflict
		}

		now, e = channelAdmissionTime(tx)
		if e != nil {
			return e
		}

		if now.UnixMilli() >= deadline {
			return ErrChannelAdmissionConflict
		}

		row := ChannelRoutingReceipt{
			q.RequestID,
			id,
			h,
			routingJSON(q),
			routingJSON(snap),
			routingJSON(after),
			now.UnixMilli(),
		}
		if e = tx.Create(&row).Error; e != nil {
			return e
		}

		now, e = channelAdmissionTime(tx)
		if e != nil {
			return e
		}

		if now.UnixMilli() >= deadline {
			return ErrChannelAdmissionConflict
		}

		receipt, e := routingReceiptView(row)
		if e != nil {
			return e
		}

		out = routingResult("ready", "")
		out.Snapshot = after
		out.Receipt = receipt
		observed := strconv.FormatInt(now.UnixMilli(), 10)
		out.ObservedAtMs = &observed

		return nil
	})
	if e != nil {
		return routingError(e)
	}

	rebuildModelCachesAfterWrite()

	return out
}

func invalidRoutingRequestFields(
	p ChannelRouting,
	q ChannelRoutingRequest,
	rev, until bool,
) bool {
	return q.Version != 1 || !ValidMigrationReadOperationID(q.RequestID) ||
		!validChannelIdentity(q.ExpectedIdentity) ||
		!rev ||
		!until ||
		!fullIdentityDigest(q.ExpectedRoutingHash) ||
		len(p.Name) == 0 ||
		utf8.RuneCountInString(p.Name) > 64 ||
		p.Models == nil ||
		len(p.Models) > 1024 ||
		p.ModelMapping == nil ||
		len(p.ModelMapping) > 1024 ||
		p.Sets == nil ||
		len(p.Sets) > 128 ||
		p.ProviderBindings == nil ||
		len(p.ProviderBindings) > 1024
}
