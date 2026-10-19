package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// VerifiedDeploymentProof is obtained only by a trusted server adapter that
// has already checked issuer/signature, audience and live deployment evidence.
// It must never be decoded from an administrative or customer request body.
// No real adapter or issuer is configured by this foundation.
type ChannelExecutorIdentity struct {
	ServiceID  string `json:"ServiceID"`
	InstanceID string `json:"InstanceID"`
	BuildSHA   string `json:"BuildSHA"`
	Epoch      string `json:"Epoch"`
}

type VerifiedDeploymentProof struct {
	ProofID                string                    `json:"ProofID"`
	Epoch                  string                    `json:"Epoch"`
	Sequence               int64                     `json:"Sequence"`
	StorageGeneration      string                    `json:"StorageGeneration"`
	StorageWatermark       int64                     `json:"StorageWatermark"`
	MembershipHash         string                    `json:"MembershipHash"`
	ExecutorFenceHash      string                    `json:"ExecutorFenceHash"`
	Executors              []ChannelExecutorIdentity `json:"Executors"`
	IssuedAtMs             int64                     `json:"IssuedAtMs"`
	ValidUntilMs           int64                     `json:"ValidUntilMs"`
	RevocationFreshUntilMs int64                     `json:"RevocationFreshUntilMs"`
}

func (p VerifiedDeploymentProof) Fingerprint() (string, error) { return admissionHash(p) }

// LoadDeployment returns a bounded local snapshot of already verified facts,
// outside every database transaction/lock. The future trusted refresh adapter
// fetches external evidence independently, never once per admission. Transactions
// lock the local proof row and recheck time/version before the final CAS.
// A zero snapshot plus an ordinary source error means unavailable. A trusted
// adapter must report an explicit verification rejection as
// ErrChannelAdmissionConflict (or return its nonzero rejected snapshot with
// the error), so frozen-work compatibility cannot mistake it for an outage.
// New admission/Seal always reject every source error regardless of class.
type ChannelAdmissionStore struct {
	controlledRuntime bool
	DB                *gorm.DB
	LogDB             *gorm.DB
	Executor          ChannelExecutorIdentity
	LoadDeployment    func(context.Context) (VerifiedDeploymentProof, error)
	// Server-only probe verifier; never populated from an HTTP request.
	LoadMigrationProbe func(context.Context, ChannelMigrationRequest) (VerifiedMigrationProbe, error)
	// Trusted local runtime feature projection, never supplied by a browser.
	MigrationFeatures []string
}

func (s *ChannelAdmissionStore) deployment(ctx context.Context) (VerifiedDeploymentProof, error) {
	if s == nil || s.DB == nil || s.DB != s.LogDB || s.LoadDeployment == nil {
		return VerifiedDeploymentProof{}, ErrChannelAdmissionUnavailable
	}

	if name := s.DB.Name(); name != "sqlite" && name != "postgres" {
		return VerifiedDeploymentProof{}, ErrChannelAdmissionUnavailable
	}

	return s.LoadDeployment(ctx)
}

// Existing durable work/results never wait for external deployment evidence.
// This preliminary read is only an optimization: the locked transaction still
// validates the stored row. If it disappears meanwhile, no new work is allowed.
func (s *ChannelAdmissionStore) deploymentForNew(
	ctx context.Context,
	table any,
	key, id string,
) (VerifiedDeploymentProof, error) {
	if s == nil || s.DB == nil || s.DB != s.LogDB {
		return VerifiedDeploymentProof{}, ErrChannelAdmissionUnavailable
	}

	if name := s.DB.Name(); name != "sqlite" && name != "postgres" {
		return VerifiedDeploymentProof{}, ErrChannelAdmissionUnavailable
	}

	var count int64
	if err := s.DB.WithContext(ctx).
		Model(table).
		Where(key+" = ?", id).
		Count(&count).
		Error; err != nil {
		return VerifiedDeploymentProof{}, err
	}

	if count > 0 {
		return VerifiedDeploymentProof{}, ErrChannelAdmissionUnavailable
	}

	return s.deployment(ctx)
}

func admissionHash(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", ErrChannelAdmissionConflict
	}

	sum := sha256.Sum256(raw)

	return hex.EncodeToString(sum[:]), nil
}

// The first write takes a database lock before any observation, including on
// SQLite. No process-local mutex is needed. Split DB/LogDB and unsupported
// backends are deliberately refused; pointer equality is conservative, not a
// claim that two connections necessarily name different databases.
func (s *ChannelAdmissionStore) transaction(
	ctx context.Context,
	work func(*gorm.DB, *ChannelAdmissionControl) error,
	allowUnactivated ...bool,
) error {
	return s.controlTransaction(ctx, work, len(allowUnactivated) != 0 && allowUnactivated[0], false)
}

// allowMaxRevision is reserved for readback and exact cutover replay; ordinary
// mutation entry points retain the exhausted-counter refusal.
func (s *ChannelAdmissionStore) controlTransaction(
	ctx context.Context,
	work func(*gorm.DB, *ChannelAdmissionControl) error,
	allowUnactivated, allowMaxRevision bool,
) error {
	if s == nil || s.DB == nil || s.DB != s.LogDB || !ChannelAdmissionStorageReady(s.DB) {
		return ErrChannelAdmissionUnavailable
	}

	if name := s.DB.Name(); name != "sqlite" && name != "postgres" {
		return ErrChannelAdmissionUnavailable
	}

	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked := tx.Model(&ChannelAdmissionControl{}).
			Where("id = ?", 1).
			UpdateColumn("revision", gorm.Expr("revision"))
		if locked.Error != nil {
			return locked.Error
		}

		if locked.RowsAffected != 1 {
			return ErrChannelAdmissionUnavailable
		}

		var control ChannelAdmissionControl
		if err := tx.First(&control, 1).Error; err != nil {
			return err
		}

		if control.Version != ChannelAdmissionProtocolVersion ||
			(control.Epoch == "" && !allowUnactivated) ||
			(control.Revision < 0 || (control.Revision == math.MaxInt64 && !allowMaxRevision)) {
			return ErrChannelAdmissionUnavailable
		}

		return work(tx, &control)
	}, claimTxOptions(s.DB)...)
}

// SQLite's Julian-day floating-point conversion can truncate a millisecond.
// Build milliseconds from integer seconds and the stored fractional digits so
// freshly created tasks are not rejected as coming from the future.
const sqliteAdmissionClockSQL = "(CAST(strftime('%s','now') AS INTEGER) * 1000 + CAST(substr(strftime('%f','now'),4,3) AS INTEGER))"

func channelAdmissionTime(tx *gorm.DB) (time.Time, error) {
	var (
		millis int64
		query  string
	)
	switch tx.Name() {
	case "sqlite":
		query = "SELECT " + sqliteAdmissionClockSQL
	case "postgres":
		query = "SELECT CAST(EXTRACT(EPOCH FROM clock_timestamp()) * 1000 AS BIGINT)"
	default:
		return time.Time{}, ErrChannelAdmissionUnavailable
	}

	if err := tx.Raw(query).Scan(&millis).Error; err != nil {
		return time.Time{}, err
	}

	if millis <= 0 {
		return time.Time{}, ErrChannelAdmissionUnavailable
	}

	return time.UnixMilli(millis).UTC(), nil
}

func lockDeploymentProof(
	tx *gorm.DB,
	c *ChannelAdmissionControl,
	p VerifiedDeploymentProof,
	executor ChannelExecutorIdentity,
) (time.Time, error) {
	if invalidDeploymentProofBinding(c, p) {
		return time.Time{}, ErrChannelAdmissionUnavailable
	}

	if len(p.Executors) == 0 || len(p.Executors) > 1024 {
		return time.Time{}, ErrChannelAdmissionUnavailable
	}

	memberHash, err := admissionHash(p.Executors)
	if err != nil || memberHash != p.MembershipHash {
		return time.Time{}, ErrChannelAdmissionUnavailable
	}

	found := false
	for _, member := range p.Executors {
		if member.ServiceID == "" || len(member.ServiceID) > 128 || member.InstanceID == "" ||
			len(member.InstanceID) > 128 ||
			!fullIdentityDigest(member.BuildSHA) ||
			member.Epoch != p.Epoch {
			return time.Time{}, ErrChannelAdmissionUnavailable
		}

		if member == executor {
			found = true
		}
	}

	if !found {
		return time.Time{}, ErrChannelAdmissionUnavailable
	}

	hash, err := p.Fingerprint()
	if err != nil {
		return time.Time{}, err
	}

	var stored ChannelAdmissionProof
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		First(&stored, "id = ?", p.ProofID).
		Error; err != nil {
		return time.Time{}, err
	}

	if stored.Revoked || stored.FactHash != hash || stored.ConsumedSequence != p.Sequence {
		return time.Time{}, ErrChannelAdmissionUnavailable
	}

	now, err := channelAdmissionTime(tx)
	if err != nil {
		return time.Time{}, err
	}

	if p.IssuedAtMs <= 0 || now.UnixMilli() < p.IssuedAtMs || now.UnixMilli() >= p.ValidUntilMs ||
		now.UnixMilli() >= p.RevocationFreshUntilMs {
		return time.Time{}, ErrChannelAdmissionUnavailable
	}

	return now, nil
}

func admissionClockSQL(tx *gorm.DB) string {
	if tx.Name() == "postgres" {
		return "CAST(EXTRACT(EPOCH FROM clock_timestamp()) * 1000 AS BIGINT)"
	}
	return sqliteAdmissionClockSQL
}

// All needed channel/proof locks are already held. This final persistent CAS
// rechecks the exact verified facts and execution-time database clock. It is
// after the journal/use writes so an expired authorization rolls them back.
func finalizeAdmissionProof(
	tx *gorm.DB,
	c *ChannelAdmissionControl,
	p VerifiedDeploymentProof,
	nextRevision int64,
) error {
	hash, err := p.Fingerprint()
	if err != nil {
		return err
	}

	clock := admissionClockSQL(tx)
	query := tx.Model(&ChannelAdmissionControl{}).
		Where("id = ? AND epoch = ? AND epoch_sequence = ? AND revision = ? AND storage_generation = ? AND active_proof_id = ? AND membership_hash = ? AND executor_fence_hash = ?", 1, p.Epoch, p.Sequence, c.Revision, p.StorageGeneration, p.ProofID, p.MembershipHash, p.ExecutorFenceHash).
		Where("EXISTS (SELECT 1 FROM channel_admission_proofs WHERE id = ? AND fact_hash = ? AND revoked = ?)", p.ProofID, hash, false).
		Where(clock+" >= ? AND "+clock+" < ? AND "+clock+" < ?", p.IssuedAtMs, p.ValidUntilMs, p.RevocationFreshUntilMs)

	result := query.UpdateColumn("revision", nextRevision)
	if result.Error != nil {
		return result.Error
	}

	if result.RowsAffected != 1 {
		return ErrChannelAdmissionUnavailable
	}

	return nil
}

// snapshotRead is internal only: callers must already own the control SHARE
// lock and a consistent read snapshot. Default command callers retain UPDATE.
func storedAdmissionIdentity(
	tx *gorm.DB,
	expected ChannelIdentity,
	snapshotRead ...bool,
) (*Channel, error) {
	if !validChannelIdentity(expected) {
		return nil, ErrChannelAdmissionConflict
	}

	var channel Channel

	q := tx
	if len(snapshotRead) == 0 || !snapshotRead[0] {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}

	if err := q.First(&channel, expected.ChannelID).Error; err != nil {
		return nil, err
	}

	identity, err := ChannelIdentityOf(&channel)
	if err != nil || identity != expected {
		return nil, ErrChannelAdmissionConflict
	}
	// A changed key must not escape the family fence by reusing a retired
	// physical ID under a new digest. Physical identities referenced by the
	// journal stay immutable, including a successor later retired again.
	var drifted int64
	if err := tx.Model(&ChannelAdmissionBarrier{}).
		Where("(from_channel_id = ? AND from_identity_hash <> ?) OR (to_channel_id = ? AND to_identity_hash <> ?)", expected.ChannelID, expected.IdentityHash, expected.ChannelID, expected.IdentityHash).
		Count(&drifted).Error; err != nil {
		return nil, err
	}

	if drifted != 0 {
		return nil, ErrChannelAdmissionConflict
	}

	return &channel, nil
}

func admissionFamily(tx *gorm.DB, digest string) (*ChannelAdmissionFamily, error) {
	var family ChannelAdmissionFamily

	err := tx.First(&family, "key_digest = ?", digest).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	if family.Revision <= 0 || family.OperationID == "" || family.AllowedChannelID < 0 ||
		(family.AllowedChannelID == 0 && family.AllowedIdentityHash != "") ||
		(family.AllowedChannelID > 0 && !fullIdentityDigest(family.AllowedIdentityHash)) {
		return nil, ErrChannelAdmissionUnavailable
	}

	return &family, nil
}

func familyAllows(family *ChannelAdmissionFamily, identity ChannelIdentity) bool {
	return family == nil ||
		(family.AllowedChannelID == identity.ChannelID && family.AllowedIdentityHash == identity.IdentityHash)
}

type ChannelAdmissionRequest struct {
	// ResourceKey is a server-derived logical task key; RequestID identifies one reservation generation.
	ResourceKey string
	RequestID   string
	GroupID     string
	TokenID     int
	Lane        string
	RequestHash string
	Identity    ChannelIdentity
}

func validAdmissionLane(lane string) bool {
	switch lane {
	case "native", "image", "relay", "trial", "probe":
		return true
	default:
		return false
	}
}

// Admit persists the usage fact and optionally the task reservation in one
// transaction. Only server code supplies the bounded DB-only callback; it
// must not perform supplier, deployment-control-plane or other network calls. A repeated id returns
// its original row without executing the callback again; callers must not use
// created=false to perform a second provider send. No client-supplied bypass
// exists, including for trials, probes or explicit channel headers.
func (s *ChannelAdmissionStore) Admit(
	ctx context.Context,
	input ChannelAdmissionRequest,
	reserve func(*gorm.DB) error,
) (use *ChannelAdmissionUse, created bool, err error) {
	return s.admit(ctx, input, reserve, nil)
}

// compatibility is used only by persisted task entry points. It runs under the
// same control lock as activation/sealing and may handle inactive deployments
// or an already-frozen task without treating it as a new admission.
//
//nolint:gocyclo // Keeps compatibility, replay, admission persistence, and post-write proof rechecks in one locked transaction.
func (s *ChannelAdmissionStore) admit(
	ctx context.Context,
	input ChannelAdmissionRequest,
	reserve func(*gorm.DB) error,
	compatibility func(*gorm.DB, *ChannelAdmissionControl) (bool, error),
	onAdmit ...func(*gorm.DB, *ChannelAdmissionControl, *ChannelAdmissionUse) error,
) (use *ChannelAdmissionUse, created bool, err error) {
	valid := input.RequestID != "" && len(input.RequestID) <= 256 && input.GroupID != "" &&
		len(input.GroupID) <= 128 &&
		input.TokenID > 0 &&
		validAdmissionLane(input.Lane) &&
		fullIdentityDigest(input.RequestHash) &&
		validChannelIdentity(input.Identity) &&
		(input.ResourceKey == "" || fullIdentityDigest(input.ResourceKey))
	if !valid && compatibility == nil {
		return nil, false, ErrChannelAdmissionConflict
	}

	id, _ := admissionHash([]any{input.Lane, input.GroupID, input.TokenID, input.RequestID})
	proof, proofErr := s.deploymentForNew(ctx, &ChannelAdmissionUse{}, "id", id)

	err = s.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		if compatibility != nil {
			handled, err := compatibility(tx, c)
			if handled || err != nil {
				return err
			}
		}

		if !valid {
			return ErrChannelAdmissionConflict
		}

		var existing ChannelAdmissionUse

		read := tx.First(&existing, "id = ?", id).Error
		if read == nil {
			if existing.ResourceKey != input.ResourceKey ||
				existing.RequestHash != input.RequestHash ||
				existing.ChannelID != input.Identity.ChannelID ||
				existing.KeyDigest != input.Identity.KeyDigest ||
				existing.IdentityHash != input.Identity.IdentityHash ||
				existing.Lane != input.Lane ||
				existing.Epoch == "" ||
				existing.AdmittedAtMs <= 0 ||
				(existing.State != "open" && existing.State != "unknown" && existing.State != "closed") ||
				(existing.State == "closed" && (existing.ClosedAtMs <= 0 || !fullIdentityDigest(existing.ClosureHash))) ||
				(existing.State != "closed" && (existing.ClosedAtMs != 0 || existing.ClosureHash != "")) {
				return ErrChannelAdmissionConflict
			}
			// The row is an already admitted task, not a grant of new work. It remains
			// recoverable when deployment proof expires or the family has been sealed.
			use = &existing

			return nil
		}

		if !errors.Is(read, gorm.ErrRecordNotFound) {
			return read
		}

		if proofErr != nil {
			return proofErr
		}

		channel, err := storedAdmissionIdentity(tx, input.Identity)
		if err != nil {
			return err
		}

		if channel.Status != ChannelStatusEnabled {
			return ErrChannelAdmissionSealed
		}

		family, err := admissionFamily(tx, input.Identity.KeyDigest)
		if err != nil {
			return err
		}

		if !familyAllows(family, input.Identity) {
			return ErrChannelAdmissionSealed
		}

		now, err := lockDeploymentProof(tx, c, proof, s.Executor)
		if err != nil {
			return err
		}

		row := ChannelAdmissionUse{
			ResourceKey:  input.ResourceKey,
			ID:           id,
			RequestHash:  input.RequestHash,
			Epoch:        c.Epoch,
			ChannelID:    input.Identity.ChannelID,
			KeyDigest:    input.Identity.KeyDigest,
			IdentityHash: input.Identity.IdentityHash,
			Lane:         input.Lane,
			State:        "open",
			AdmittedAtMs: now.UnixMilli(),
		}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}

		if reserve != nil {
			if err := reserve(tx); err != nil {
				return err
			}
		}

		for _, record := range onAdmit {
			if err := record(tx, c, &row); err != nil {
				return err
			}
		}

		if err := finalizeAdmissionProof(tx, c, proof, c.Revision); err != nil {
			return err
		}

		use = &row
		created = true

		return nil
	}, compatibility != nil)
	if err != nil {
		return nil, false, err
	}

	return use, created, nil
}

type ChannelSealRequest struct {
	OperationID      string          `json:"operationId"`
	Epoch            string          `json:"epoch"`
	ExpectedRevision int64           `json:"expectedRevision"`
	From             ChannelIdentity `json:"from"`
	To               ChannelIdentity `json:"to"`
	RevertOf         string          `json:"revertOf,omitempty"`
}

// Seal retires a physical generation while granting precisely its successor.
// It only controls new admission: existing open/unknown records and task rows
// are neither ended nor refunded. Alias channels sharing the digest do not
// inherit the successor grant. The returned time is read under the same lock.
func (s *ChannelAdmissionStore) Seal(
	ctx context.Context,
	input ChannelSealRequest,
) (*ChannelAdmissionBarrier, error) {
	return s.seal(ctx, input, nil)
}

func (s *ChannelAdmissionStore) seal(
	ctx context.Context,
	input ChannelSealRequest,
	prepared *migrationAuthorization,
) (barrier *ChannelAdmissionBarrier, err error) {
	if s.controlledRuntime {
		return nil, ErrChannelAdmissionUnavailable
	}

	if input.OperationID == "" || len(input.OperationID) > 128 || input.Epoch == "" ||
		len(input.Epoch) > 128 ||
		input.ExpectedRevision < 0 ||
		!validChannelIdentity(input.From) ||
		!validChannelIdentity(input.To) ||
		input.From.ChannelID == input.To.ChannelID ||
		len(input.RevertOf) > 128 {
		return nil, ErrChannelAdmissionConflict
	}

	hash, err := admissionHash(input)
	if err != nil {
		return nil, err
	}

	proof, proofErr := s.deploymentForNew(
		ctx,
		&ChannelAdmissionBarrier{},
		"operation_id",
		input.OperationID,
	)
	if prepared != nil {
		proof = prepared.deployment
		proofErr = nil
	}

	err = s.transaction(ctx, func(tx *gorm.DB, c *ChannelAdmissionControl) error {
		barrier, err = s.sealTx(tx, c, input, prepared, proof, proofErr, hash)
		return err
	})
	if err != nil {
		return nil, err
	}

	return barrier, nil
}

// sealTx runs under the caller's existing control transaction. Never opens a
// nested public command transaction; the caller owns rollback of all effects.
//
//nolint:gocyclo // Keeps retirement, source/target family locks, replay checks, and final barrier CAS in their required order.
func (s *ChannelAdmissionStore) sealTx(
	tx *gorm.DB,
	c *ChannelAdmissionControl,
	input ChannelSealRequest,
	prepared *migrationAuthorization,
	proof VerifiedDeploymentProof,
	proofErr error,
	hash string,
) (barrier *ChannelAdmissionBarrier, err error) {
	err = func() error {
		if prepared != nil {
			if err := validateMigrationAuthorization(
				tx,
				c,
				prepared,
				"seal",
				s.MigrationFeatures,
			); err != nil {
				return err
			}

			if _, err := lockDeploymentProof(tx, c, prepared.deployment, s.Executor); err != nil {
				return err
			}
		}

		if prepared == nil {
			if err := guardPreparedChannelMutation(
				tx,
				[]int{input.From.ChannelID, input.To.ChannelID},
			); err != nil {
				return err
			}
		}

		var old ChannelAdmissionBarrier

		read := tx.First(&old, "operation_id = ?", input.OperationID).Error
		if read == nil {
			if old.RequestHash != hash || old.Version != ChannelAdmissionProtocolVersion ||
				old.Epoch != input.Epoch ||
				(prepared == nil && old.Revision != input.ExpectedRevision+1) ||
				old.RecordedAtMs <= 0 ||
				old.FromChannelID != input.From.ChannelID ||
				old.FromKeyDigest != input.From.KeyDigest ||
				old.FromIdentityHash != input.From.IdentityHash ||
				old.ToChannelID != input.To.ChannelID ||
				old.ToKeyDigest != input.To.KeyDigest ||
				old.ToIdentityHash != input.To.IdentityHash {
				return ErrChannelAdmissionConflict
			}

			barrier = &old

			return nil
		}

		if !errors.Is(read, gorm.ErrRecordNotFound) {
			return read
		}

		if prepared != nil {
			input.ExpectedRevision = c.Revision
		}

		if input.Epoch != c.Epoch || input.ExpectedRevision != c.Revision {
			return ErrChannelAdmissionConflict
		}

		if proofErr != nil {
			return proofErr
		}

		for _, identity := range []ChannelIdentity{input.From, input.To} {
			ch, err := storedAdmissionIdentity(tx, identity)
			if err != nil {
				return err
			}

			wantStatus := ChannelStatusEnabled
			if prepared != nil && identity.ChannelID == input.To.ChannelID {
				wantStatus = migrationTargetStatus(prepared.request)
			}

			if ch.Status != wantStatus {
				return ErrChannelAdmissionConflict
			}
		}

		fromFamily, err := admissionFamily(tx, input.From.KeyDigest)
		if err != nil {
			return err
		}

		if !familyAllows(fromFamily, input.From) {
			return ErrChannelAdmissionConflict
		}

		toFamily, err := admissionFamily(tx, input.To.KeyDigest)
		if err != nil {
			return err
		}
		// Retirement belongs to the physical identity, not just its digest.
		// A same-key return must not masquerade as an ordinary new successor.
		// Only the target's latest retirement can still be reversed: replaying
		// an older exact pair after another generation must not reopen it.
		var retirement ChannelAdmissionBarrier

		retirementErr := tx.Where("from_channel_id = ?", input.To.ChannelID).
			Order("revision DESC").First(&retirement).Error

		reverting := input.RevertOf != ""
		switch {
		case retirementErr == nil:
			if !reverting || input.RevertOf != retirement.OperationID ||
				retirement.Version != ChannelAdmissionProtocolVersion || retirement.Revision <= 0 || retirement.Revision > c.Revision || retirement.RecordedAtMs <= 0 || !fullIdentityDigest(retirement.RequestHash) ||
				retirement.FromChannelID != input.To.ChannelID || retirement.FromKeyDigest != input.To.KeyDigest || retirement.FromIdentityHash != input.To.IdentityHash ||
				retirement.ToChannelID != input.From.ChannelID || retirement.ToKeyDigest != input.From.KeyDigest || retirement.ToIdentityHash != input.From.IdentityHash {
				return ErrChannelAdmissionConflict
			}
		case !errors.Is(retirementErr, gorm.ErrRecordNotFound):
			return retirementErr
		case reverting:
			return ErrChannelAdmissionConflict
		}

		if input.From.KeyDigest != input.To.KeyDigest && toFamily != nil &&
			!familyAllows(toFamily, input.To) &&
			!reverting {
			return ErrChannelAdmissionConflict
		}

		now, err := lockDeploymentProof(tx, c, proof, s.Executor)
		if err != nil {
			return err
		}

		revision := c.Revision + 1

		fromGrant := ChannelAdmissionFamily{
			KeyDigest:   input.From.KeyDigest,
			Revision:    revision,
			OperationID: input.OperationID,
		}
		if input.From.KeyDigest == input.To.KeyDigest {
			fromGrant.AllowedChannelID = input.To.ChannelID
			fromGrant.AllowedIdentityHash = input.To.IdentityHash
		}

		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key_digest"}}, UpdateAll: true}).
			Create(&fromGrant).
			Error; err != nil {
			return err
		}

		if input.From.KeyDigest != input.To.KeyDigest && (toFamily != nil || prepared != nil) {
			grant := ChannelAdmissionFamily{
				KeyDigest:           input.To.KeyDigest,
				AllowedChannelID:    input.To.ChannelID,
				AllowedIdentityHash: input.To.IdentityHash,
				Revision:            revision,
				OperationID:         input.OperationID,
			}
			if err := tx.Save(&grant).Error; err != nil {
				return err
			}
		}

		row := ChannelAdmissionBarrier{
			OperationID:      input.OperationID,
			RequestHash:      hash,
			Epoch:            c.Epoch,
			Version:          ChannelAdmissionProtocolVersion,
			Revision:         revision,
			FromChannelID:    input.From.ChannelID,
			FromKeyDigest:    input.From.KeyDigest,
			FromIdentityHash: input.From.IdentityHash,
			ToChannelID:      input.To.ChannelID,
			ToKeyDigest:      input.To.KeyDigest,
			ToIdentityHash:   input.To.IdentityHash,
			RecordedAtMs:     now.UnixMilli(),
		}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}

		if prepared != nil {
			update := tx.Model(&ChannelMigrationPrepared{}).
				Where("operation_id = ? AND state = ?", input.OperationID, "prepared").
				Updates(map[string]any{"state": "sealed", "seal_revision": revision})
			if update.Error != nil {
				return update.Error
			}

			if update.RowsAffected != 1 {
				return ErrChannelAdmissionConflict
			}

			if err := finalizeMigrationProbe(tx, prepared.probe); err != nil {
				return err
			}
		}

		if err := finalizeAdmissionProof(tx, c, proof, revision); err != nil {
			return err
		}

		if prepared != nil {
			if _, err := lockDeploymentProof(tx, c, proof, s.Executor); err != nil {
				return err
			}

			if err := finalizeMigrationProbe(tx, prepared.probe); err != nil {
				return err
			}
		}

		barrier = &row

		return nil
	}()

	return barrier, err
}

func invalidDeploymentProofBinding(c *ChannelAdmissionControl, p VerifiedDeploymentProof) bool {
	return p.ProofID == "" || len(p.ProofID) > 128 || p.Epoch != c.Epoch || p.Sequence <= 0 ||
		p.Sequence != c.EpochSequence ||
		p.StorageGeneration == "" ||
		p.StorageGeneration != c.StorageGeneration ||
		p.StorageWatermark < 0 ||
		c.Revision < p.StorageWatermark ||
		!fullIdentityDigest(p.MembershipHash) ||
		p.MembershipHash != c.MembershipHash ||
		!fullIdentityDigest(p.ExecutorFenceHash) ||
		p.ExecutorFenceHash != c.ExecutorFenceHash ||
		c.ActiveProofID != p.ProofID
}
