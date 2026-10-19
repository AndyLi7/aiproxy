package model

// Restricted probes are one operation-owned native task, never an enabled route.
// Intent, use and task ownership survive a disconnected caller. No HTTP boolean
// can create a private handle or turn a wallet settlement into migration proof.
import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"github.com/labring/aiproxy/core/common/nativeresult"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type ChannelMigrationProbeRuntimeProtocol struct {
	ID      int `gorm:"primaryKey"`
	Version int `gorm:"not null"`
}
type ChannelMigrationProbeRun struct {
	OwnerHash           string `gorm:"size:64;not null"`
	EvidenceJSON        string `gorm:"type:text;not null"`
	EvidenceTaskHash    string `gorm:"size:64;not null"`
	OperationID         string `gorm:"primaryKey;size:128"`
	Version             int    `gorm:"not null"`
	AuthorizationJSON   string `gorm:"type:text;not null"`
	AuthorizationHash   string `gorm:"size:64;not null"`
	CommandHash         string `gorm:"size:64;not null"`
	ResourceRequestHash string `gorm:"size:64;not null"`
	CheckpointHash      string `gorm:"size:64;not null"`
	StorageGeneration   string `gorm:"size:128;not null"`
	TaskID              string `gorm:"size:128;not null;uniqueIndex"`
	SpecJSON            string `gorm:"type:text;not null"`
	SpecHash            string `gorm:"size:64;not null"`
	UseID               string `gorm:"size:64;not null"`
	TaskHash            string `gorm:"size:64;not null"`
	CreatedAtMs         int64  `gorm:"not null"`
	BoundAtMs           int64  `gorm:"not null"`
}

// Authorization is frozen by authenticated server orchestration. MaxPrepaidMicros
// bounds the initial hold only, not the provider's final bill or negative balance.
type MigrationProbeAuthorization struct {
	Command             ChannelMigrationRequest `json:"command"`
	ResourceRequestHash string                  `json:"resourceRequestHash"`
	CheckpointHash      string                  `json:"checkpointHash"`
	Model               string                  `json:"model"`
	Input               json.RawMessage         `json:"input"`
	GroupID             string                  `json:"groupId"`
	TokenID             int                     `json:"tokenId"`
	MaxPrepaidMicros    int64                   `json:"maxPrepaidMicros"`
}
type MigrationProbeSpec struct {
	TaskID          string          `json:"taskId"`
	GroupID         string          `json:"groupId"`
	TokenID         int             `json:"tokenId"`
	Input           json.RawMessage `json:"input"`
	Contract        json.RawMessage `json:"contract"`
	QuoteJSON       string          `json:"quoteJson"`
	ChannelID       int             `json:"channelId"`
	IdentityHash    string          `json:"identityHash"`
	KeyDigest       string          `json:"keyDigest"`
	Endpoint        string          `json:"endpoint"`
	CredentialScope string          `json:"credentialScope"`
	DeliveryBase    string          `json:"deliveryBase"`
}

// MigrationProbeHandle is deliberately not serializable or constructible by a
// controller decoder. Its only authority is the durable, exact operation record.
type MigrationProbeHandle struct {
	store                                 *ChannelAdmissionStore
	operation, authorization, spec, nonce string
}

func (h *MigrationProbeHandle) Spec() (MigrationProbeSpec, error) {
	var s MigrationProbeSpec
	if h == nil || h.store == nil || json.Unmarshal([]byte(h.spec), &s) != nil {
		return s, ErrChannelAdmissionConflict
	}

	return s, nil
}

func probeRuntimeStorageReady(tx *gorm.DB) error {
	for _, m := range []any{&ChannelMigrationProbeRuntimeProtocol{}, &ChannelMigrationProbeRun{}} {
		st := &gorm.Statement{DB: tx}
		if st.Parse(m) != nil {
			return ErrChannelAdmissionUnavailable
		}

		columns := make([]string, 0, len(st.Schema.DBNames))
		for _, name := range st.Schema.DBNames {
			columns = append(columns, st.Quote(clause.Column{Table: st.Schema.Table, Name: name}))
		}

		rows, e := tx.Model(m).Select(strings.Join(columns, ",")).Limit(0).Rows()
		if e != nil {
			return ErrChannelAdmissionUnavailable
		}

		e = rows.Err()

		//nolint:sqlclosecheck // The LIMIT 0 schema probe must close before the next query on this transaction.
		ce := rows.Close()
		if e != nil || ce != nil {
			return ErrChannelAdmissionUnavailable
		}
	}

	var p ChannelMigrationProbeRuntimeProtocol
	if tx.First(&p, 1).Error != nil || p.Version != 1 {
		return ErrChannelAdmissionUnavailable
	}

	return nil
}

func probeOwner(tx *gorm.DB, a MigrationProbeAuthorization) error {
	var (
		g Group
		t Token
	)

	if tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id", "status").
		First(&g, "id=?", a.GroupID).
		Error != nil ||
		g.Status != GroupStatusInternal {
		return ErrChannelAdmissionConflict
	}

	if tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id", "group_id", "status").
		First(&t, a.TokenID).
		Error != nil ||
		t.Status != TokenStatusEnabled ||
		t.GroupID != a.GroupID {
		return ErrChannelAdmissionConflict
	}

	return nil
}

func probeResource(
	tx *gorm.DB,
	c *ChannelAdmissionControl,
	a MigrationProbeAuthorization,
	features []string,
) error {
	if !validMigrationResourceKind(a.Command) {
		return ErrChannelAdmissionConflict
	}

	if reverseMigration(a.Command) {
		return reverseProbeResource(tx, c, a, features)
	}

	r, e := prepareShadowResource(tx, c, a.Command, features)
	if e != nil {
		return e
	}

	if r == nil || r.LifecycleState != "created" || r.RequestHash != a.ResourceRequestHash ||
		r.CheckpointHash != a.CheckpointHash {
		return ErrChannelAdmissionConflict
	}

	if e = probeOwner(tx, a); e != nil {
		return e
	}

	if c.Epoch != a.Command.Seal.Epoch || c.Revision < a.Command.Seal.ExpectedRevision {
		return ErrChannelAdmissionConflict
	}

	family, e := migrationFamilyHash(tx, a.Command.Seal.From.KeyDigest, a.Command.Seal.To.KeyDigest)
	if e != nil || family != a.Command.FamilyHash {
		return ErrChannelAdmissionConflict
	}

	return migrationSnapshotMatches(tx, a.Command, ChannelStatusDisabled)
}

func probeSpec(tx *gorm.DB, a MigrationProbeAuthorization, id string) (MigrationProbeSpec, error) {
	var raw string
	if e := tx.Table("model_configs").
		Select("config").
		Where("model=?", a.Model).
		Scan(&raw).
		Error; e != nil {
		return MigrationProbeSpec{}, e
	}

	rewritten, changed, e := migrationRewriteConfig(raw, a.Command)
	if e != nil || !changed {
		return MigrationProbeSpec{}, ErrChannelAdmissionConflict
	}

	var config map[string]json.RawMessage
	if json.Unmarshal([]byte(rewritten), &config) != nil {
		return MigrationProbeSpec{}, ErrChannelAdmissionConflict
	}

	var binding struct {
		Version         int             `json:"version"`
		ChannelID       int             `json:"channelId"`
		Endpoint        string          `json:"endpoint"`
		CredentialScope string          `json:"credentialScope"`
		KeyFingerprint  string          `json:"keyFingerprint"`
		DeliveryBase    string          `json:"deliveryBase"`
		Contract        json.RawMessage `json:"contract"`
	}
	if json.Unmarshal(config["x_token_platform_native_result_v1"], &binding) != nil ||
		binding.Version != 1 ||
		binding.ChannelID != a.Command.Seal.To.ChannelID ||
		binding.CredentialScope != a.Command.ToCredentialID ||
		binding.KeyFingerprint != a.Command.Seal.To.KeyDigest {
		return MigrationProbeSpec{}, ErrChannelAdmissionConflict
	}

	quoteRaw := string(config[string(ImagePrepaymentConfigKey)])

	q, e := ParseImagePrepaymentQuote(quoteRaw)
	if e != nil || q.PrepaidMicros > a.MaxPrepaidMicros {
		return MigrationProbeSpec{}, ErrChannelAdmissionConflict
	}

	route := q.Route(binding.ChannelID, binding.Endpoint)
	if route == nil || route.Provider != "fal" ||
		route.CredentialScope != binding.CredentialScope ||
		(route.QuantityMetric != "request" && route.QuantityMetric != "bounded_request") {
		return MigrationProbeSpec{}, ErrChannelAdmissionUnavailable
	}

	ch, e := storedAdmissionIdentity(tx, a.Command.Seal.To)
	if e != nil || ch.Type != ChannelTypeFal || !probeDeliveryURL(binding.DeliveryBase) ||
		ch.ModelMapping[a.Model] != binding.Endpoint ||
		!shadowModelsExist(ch.Models, []string{a.Model}) ||
		strings.TrimRight(ch.BaseURL, "/") != "https://queue.fal.run" {
		return MigrationProbeSpec{}, ErrChannelAdmissionUnavailable
	}

	contract, e := nativeresult.CompileTaskContract(binding.Contract)
	if e != nil {
		return MigrationProbeSpec{}, ErrChannelAdmissionConflict
	}

	input, e := contract.ValidateRequest(a.Input)
	if e != nil {
		return MigrationProbeSpec{}, ErrChannelAdmissionConflict
	}

	if _, e = contract.PrepareUpstreamInput(input); e != nil {
		return MigrationProbeSpec{}, ErrChannelAdmissionConflict
	}

	return MigrationProbeSpec{
		id,
		a.GroupID,
		a.TokenID,
		a.Input,
		binding.Contract,
		quoteRaw,
		binding.ChannelID,
		a.Command.Seal.To.IdentityHash,
		binding.KeyFingerprint,
		binding.Endpoint,
		binding.CredentialScope,
		binding.DeliveryBase,
	}, nil
}

func loadProbeRun(
	tx *gorm.DB,
	op string,
	snapshot ...bool,
) (*ChannelMigrationProbeRun, MigrationProbeAuthorization, MigrationProbeSpec, error) {
	var (
		r    ChannelMigrationProbeRun
		a    MigrationProbeAuthorization
		spec MigrationProbeSpec
	)

	q := tx
	if len(snapshot) == 0 || !snapshot[0] {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}

	if e := q.First(&r, "operation_id=?", op).Error; e != nil {
		return nil, a, spec, e
	}

	if (r.EvidenceJSON == "" && r.EvidenceTaskHash != "") ||
		(r.EvidenceJSON != "" && !fullIdentityDigest(r.EvidenceTaskHash)) ||
		r.Version != 1 ||
		json.Unmarshal([]byte(r.AuthorizationJSON), &a) != nil ||
		json.Unmarshal([]byte(r.SpecJSON), &spec) != nil {
		return nil, a, spec, ErrChannelAdmissionConflict
	}

	ah, _ := admissionHash(a)
	sh, _ := admissionHash(spec)

	ch, _ := admissionHash(a.Command)
	if invalidStoredProbeRun(a, ah, ch, r, sh, spec) {
		return nil, a, spec, ErrChannelAdmissionConflict
	}

	return &r, a, spec, nil
}

//nolint:gocyclo // Probe creation checks bounded authorization, reviewed resource, and replay identity before persisting its private handle.
func (s *ChannelAdmissionStore) BeginMigrationProbe(
	ctx context.Context,
	a MigrationProbeAuthorization,
) (handle *MigrationProbeHandle, err error) {
	if !validProbeCommand(a.Command) ||
		!ValidMigrationReadOperationID(a.Command.Seal.OperationID) ||
		!fullIdentityDigest(a.ResourceRequestHash) ||
		!fullIdentityDigest(a.CheckpointHash) ||
		a.Model == "" ||
		len(a.Model) > 256 ||
		a.GroupID == "" ||
		len(a.GroupID) > 64 ||
		a.TokenID <= 0 ||
		a.MaxPrepaidMicros < 0 ||
		a.MaxPrepaidMicros > maximumSafeMicros ||
		len(a.Input) == 0 ||
		len(a.Input) > 256*1024 ||
		!probeJSONValid(a.Input) {
		return nil, ErrChannelAdmissionConflict
	}

	nonce, err := newAdmissionGeneration()
	if err != nil {
		return nil, err
	}

	authHash, _ := admissionHash(a)
	proof, proofErr := s.deployment(ctx)
	err = s.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		tx = tx.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
		if e := probeRuntimeStorageReady(tx); e != nil {
			return e
		}

		if e := probeResource(tx, c, a, s.MigrationFeatures); e != nil {
			return e
		}

		existing, _, _, e := loadProbeRun(tx, a.Command.Seal.OperationID)
		if e == nil {
			if existing.AuthorizationHash != authHash ||
				existing.StorageGeneration != c.StorageGeneration {
				return ErrChannelAdmissionConflict
			}

			handle = &MigrationProbeHandle{
				s,
				existing.OperationID,
				authHash,
				existing.SpecJSON,
				nonce,
			}

			return nil
		}

		if !errors.Is(e, gorm.ErrRecordNotFound) {
			return e
		}

		if proofErr != nil {
			return proofErr
		}

		now, e := lockDeploymentProof(tx, c, proof, s.Executor)
		if e != nil {
			return e
		}

		taskID := probeOperationTaskID(
			c.StorageGeneration,
			a.Command.Seal.OperationID,
			a.CheckpointHash,
		)

		var taskCount, useCount int64
		if e = tx.Model(&NativeTask{}).Where("id=?", taskID).Count(&taskCount).Error; e != nil {
			return e
		}

		if e = tx.Model(&ChannelAdmissionUse{}).
			Where("id=?", probeOperationUseID(taskID)).
			Count(&useCount).
			Error; e != nil {
			return e
		}

		if taskCount != 0 || useCount != 0 {
			return ErrChannelAdmissionConflict
		}

		spec, e := probeSpec(tx, a, taskID)
		if e != nil {
			return e
		}

		encoded, _ := json.Marshal(a)
		frozen, _ := json.Marshal(spec)
		specHash, _ := admissionHash(spec)
		commandHash, _ := admissionHash(a.Command)

		r := ChannelMigrationProbeRun{
			OperationID:         a.Command.Seal.OperationID,
			Version:             1,
			AuthorizationJSON:   string(encoded),
			AuthorizationHash:   authHash,
			CommandHash:         commandHash,
			ResourceRequestHash: a.ResourceRequestHash,
			CheckpointHash:      a.CheckpointHash,
			StorageGeneration:   c.StorageGeneration,
			TaskID:              spec.TaskID,
			SpecJSON:            string(frozen),
			SpecHash:            specHash,
			CreatedAtMs:         now.UnixMilli(),
		}
		if e = tx.Create(&r).Error; e != nil {
			return e
		}

		if e = finalizeAdmissionProof(tx, c, proof, c.Revision); e != nil {
			return e
		}

		handle = &MigrationProbeHandle{s, r.OperationID, authHash, r.SpecJSON, nonce}

		return nil
	})

	return handle, err
}

// ReserveMigrationProbeTask is called only by the existing native engine with a
// private handle. A bound run never reserves again, even if its task was deleted
// by a normal reservation refund. It remains unknown and requires reconciliation.
//
//nolint:gocyclo // Probe reservation retains its handle, frozen contract, budget, task identity, and final proof checks in one transaction.
func ReserveMigrationProbeTask(
	ctx context.Context,
	db *gorm.DB,
	h *MigrationProbeHandle,
	task NativeTask,
) (saved *NativeTask, created bool, err error) {
	if h == nil || h.store == nil || h.store.DB != db || h.store.LogDB != db ||
		!fullIdentityDigest(h.nonce) {
		return nil, false, ErrChannelAdmissionConflict
	}

	s := h.store
	proof, proofErr := s.deployment(ctx)

	err = s.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		tx = tx.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
		if e := probeRuntimeStorageReady(tx); e != nil {
			return e
		}

		var original MigrationProbeAuthorization

		if json.Unmarshal([]byte(h.spec), &MigrationProbeSpec{}) != nil {
			return ErrChannelAdmissionConflict
		}
		// Lock ordering stays control → owned resource → probe run → owner/task/use.
		if _, e := migrationTargetByOperation(tx, h.operation, false); e != nil {
			return e
		}

		r, a, spec, e := loadProbeRun(tx, h.operation)
		if e != nil {
			return e
		}

		original = a

		if r.AuthorizationHash != h.authorization || r.SpecJSON != h.spec ||
			r.StorageGeneration != c.StorageGeneration ||
			r.UseID != "" {
			return ErrChannelAdmissionConflict
		}

		if e = probeResource(tx, c, original, s.MigrationFeatures); e != nil {
			return e
		}

		if proofErr != nil {
			return proofErr
		}

		now, e := lockDeploymentProof(tx, c, proof, s.Executor)
		if e != nil {
			return e
		}

		contract, e := nativeresult.CompileTaskContract(spec.Contract)
		if e != nil {
			return ErrChannelAdmissionConflict
		}

		input, e := contract.ValidateRequest(spec.Input)
		if e != nil {
			return e
		}

		nativeInput, e := contract.PrepareUpstreamInput(input)
		if e != nil {
			return e
		}

		var frozen nativeresult.TaskContract
		if json.Unmarshal(spec.Contract, &frozen) != nil {
			return ErrChannelAdmissionConflict
		}

		if task.ID != spec.TaskID || task.GroupID != spec.GroupID || task.TokenID != spec.TokenID ||
			task.Model != frozen.Model ||
			task.Fingerprint != shadowDigest(string(spec.Input)) ||
			task.OutputSchema != string(frozen.OutputSchema) ||
			task.FrozenContract != string(spec.Contract) ||
			task.NativeInput != string(nativeInput) ||
			task.ChannelID != spec.ChannelID ||
			task.Endpoint != spec.Endpoint ||
			task.CredentialScope != spec.CredentialScope ||
			task.PrepaymentQuoteJSON != spec.QuoteJSON ||
			task.BillingOperationID != "native:"+spec.TaskID ||
			task.DeliveryBase != spec.DeliveryBase ||
			task.KeyFingerprint != spec.KeyDigest ||
			task.AdmissionIdentityHash != spec.IdentityHash {
			return ErrChannelAdmissionConflict
		}

		useID := probeOperationUseID(task.ID)

		use := ChannelAdmissionUse{
			ResourceKey:  nativeAdmissionResource(task.ID, task.GroupID, task.TokenID),
			ID:           useID,
			RequestHash:  task.Fingerprint,
			Epoch:        c.Epoch,
			ChannelID:    spec.ChannelID,
			KeyDigest:    spec.KeyDigest,
			IdentityHash: spec.IdentityHash,
			Lane:         "native",
			State:        "open",
			AdmittedAtMs: now.UnixMilli(),
		}
		if e = tx.Create(&use).Error; e != nil {
			return e
		}

		saved, created, e = reserveNativeTask(tx, task)
		if e != nil {
			return e
		}

		if !created {
			return ErrChannelAdmissionConflict
		}

		res := tx.Model(r).
			Where("use_id='' AND bound_at_ms=0").
			Updates(map[string]any{"use_id": use.ID, "owner_hash": shadowDigest(h.operation + ":" + h.authorization + ":" + h.nonce), "task_hash": nativeExecutionTaskHash(saved), "bound_at_ms": now.UnixMilli()})
		if res.Error != nil {
			return res.Error
		}

		if res.RowsAffected != 1 {
			return ErrChannelAdmissionConflict
		}

		observed, e := probeObservationTx(tx, c, h.operation)
		if e != nil || observed.Observation == nil || observed.Observation.UseID != use.ID {
			return ErrChannelAdmissionConflict
		}

		durable, _, _, e := loadProbeRun(tx, h.operation)
		if e != nil ||
			durable.OwnerHash != shadowDigest(h.operation+":"+h.authorization+":"+h.nonce) {
			return ErrChannelAdmissionConflict
		}

		if e = probeResource(tx, c, original, s.MigrationFeatures); e != nil {
			return e
		}

		if e = finalizeAdmissionProof(tx, c, proof, c.Revision); e != nil {
			return e
		}

		saved.AdmissionGeneration = use.ID

		return nil
	})
	if err != nil {
		return nil, false, err
	}

	return saved, created, err
}

func probeJSONValid(raw []byte) bool { _, ok := inventoryDecode(raw); return ok }
func validProbeCommand(r ChannelMigrationRequest) bool {
	return validMigrationResourceKind(r) && r.Seal.Epoch != "" && r.Seal.ExpectedRevision >= 0 &&
		validChannelIdentity(r.Seal.From) &&
		validChannelIdentity(r.Seal.To) &&
		r.Seal.From.ChannelID != r.Seal.To.ChannelID &&
		r.FromCredentialID != "" &&
		r.ToCredentialID != "" &&
		len(r.FromCredentialID) <= 128 &&
		len(r.ToCredentialID) <= 128 &&
		fullIdentityDigest(r.ExpectedPinHash) &&
		fullIdentityDigest(r.ProjectionHash) &&
		fullIdentityDigest(r.FamilyHash) &&
		(r.Seal.From.KeyDigest == r.Seal.To.KeyDigest) == (r.FromCredentialID == r.ToCredentialID)
}

func probeDeliveryURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && len(raw) <= 2048 && u.Scheme == "https" && u.Host != "" && u.User == nil &&
		u.RawQuery == "" &&
		u.Fragment == ""
}

func probeOperationTaskID(storage, op, checkpoint string) string {
	h, _ := admissionHash([]string{"migration-probe-v1", storage, op, checkpoint})
	return "probe_" + h
}

// ValidMigrationProbeJSON is a bounded duplicate-key check for private HTTP input.
func ValidMigrationProbeJSON(raw []byte) bool { return probeJSONValid(raw) }

func probeOperationUseID(taskID string) string {
	h, _ := admissionHash([]string{"migration-probe-use-v1", taskID})
	return h
}

func invalidStoredProbeRun(
	a MigrationProbeAuthorization,
	ah, ch string,
	r ChannelMigrationProbeRun,
	sh string,
	spec MigrationProbeSpec,
) bool {
	return ah != r.AuthorizationHash || sh != r.SpecHash || ch != r.CommandHash ||
		r.OperationID != a.Command.Seal.OperationID ||
		r.TaskID != spec.TaskID ||
		(r.UseID != "" && r.UseID != probeOperationUseID(r.TaskID)) ||
		r.TaskID != probeOperationTaskID(r.StorageGeneration, r.OperationID, r.CheckpointHash) ||
		r.ResourceRequestHash != a.ResourceRequestHash ||
		r.CheckpointHash != a.CheckpointHash ||
		r.CreatedAtMs <= 0 ||
		(r.BoundAtMs != 0 && r.BoundAtMs < r.CreatedAtMs) ||
		r.StorageGeneration == "" ||
		(!fullIdentityDigest(r.UseID) && r.UseID != "") ||
		((r.UseID == "") != (r.BoundAtMs == 0)) ||
		((r.UseID == "") != (r.TaskHash == "")) ||
		(r.UseID == "" && r.OwnerHash != "") ||
		(r.UseID != "" && !fullIdentityDigest(r.OwnerHash))
}
