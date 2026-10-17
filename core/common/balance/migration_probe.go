package balance

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/labring/aiproxy/core/model"
)

const migrationProbeBillingPath = "/api/internal/model-ops/migration-probe-billing"

// Transport budget covers the application's bounded current-observation pipeline;
// it is not a task TTL, hold duration, proof lifetime, or provider-cost cap.
const migrationProbeBillingTimeout = 60 * time.Second
const migrationProbeBillingMaxBody = 32 * 1024

type migrationProbeBillingBackend interface {
	ReadMigrationProbeBilling(context.Context, model.MigrationProbeBillingRequest) (model.MigrationProbeBillingEvidence, error)
}

// ReadMigrationProbeBilling uses only the configured internal wallet transport.
// Mock/other backends cannot supply a provenance fallback.
func ReadMigrationProbeBilling(ctx context.Context, r model.MigrationProbeBillingRequest) (model.MigrationProbeBillingEvidence, error) {
	backend, ok := Default.(migrationProbeBillingBackend)
	if !ok {
		return model.MigrationProbeBillingEvidence{}, model.ErrMigrationProbeMissingProvenance
	}
	return backend.ReadMigrationProbeBilling(ctx, r)
}

type migrationProbeBillingEvidenceWire struct {
	Binding                       model.MigrationProbeBillingRequest `json:"binding"`
	EvidenceID                    string                             `json:"evidenceId"`
	ProvenanceHash                string                             `json:"provenanceHash"`
	AccountMembershipEvidenceHash string                             `json:"accountMembershipEvidenceHash"`
	AccountMembershipEvidenceKind string                             `json:"accountMembershipEvidenceKind"`
	Currency                      string                             `json:"currency"`
	CostMicros                    string                             `json:"costMicros"`
	CustomerDueMicros             string                             `json:"customerDueMicros"`
	ChargedMicros                 string                             `json:"chargedMicros"`
	SettledAtMs                   string                             `json:"settledAtMs"`
}

func probeBillingDecimal(s string) (int64, bool) {
	n, e := strconv.ParseInt(s, 10, 64)
	return n, e == nil && n >= 0 && n <= 9007199254740991 && strconv.FormatInt(n, 10) == s
}
func probeBillingDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
func (e *ExternalHTTP) ReadMigrationProbeBilling(ctx context.Context, r model.MigrationProbeBillingRequest) (model.MigrationProbeBillingEvidence, error) {
	empty := model.MigrationProbeBillingEvidence{}
	if e == nil || e.key == "" {
		return empty, model.ErrMigrationProbeMissingProvenance
	}
	u, err := url.Parse(e.url)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return empty, model.ErrChannelAdmissionUnavailable
	}
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > 4096 {
		return empty, model.ErrChannelAdmissionConflict
	}
	ctx, cancel := context.WithTimeout(ctx, migrationProbeBillingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(e.url, "/")+migrationProbeBillingPath, bytes.NewReader(raw))
	if err != nil {
		return empty, model.ErrChannelAdmissionUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := *externalHTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return empty, ctx.Err()
		}
		return empty, model.ErrChannelAdmissionUnavailable
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return empty, model.ErrChannelAdmissionUnavailable
	}
	raw, err = io.ReadAll(io.LimitReader(res.Body, migrationProbeBillingMaxBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return empty, ctx.Err()
		}
		return empty, model.ErrChannelAdmissionUnavailable
	}
	if len(raw) == 0 || len(raw) > migrationProbeBillingMaxBody || !model.ValidMigrationProbeJSON(raw) {
		return empty, model.ErrChannelAdmissionConflict
	}
	var body struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Version  int                                `json:"version"`
			Source   string                             `json:"source"`
			State    string                             `json:"state"`
			Reason   string                             `json:"reason"`
			Evidence *migrationProbeBillingEvidenceWire `json:"evidence"`
		} `json:"data"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&body) != nil || body.Code == nil || *body.Code != 0 || body.Data.Version != 1 || body.Data.Source != "app-migration-probe-billing-v1" || len(body.Data.Reason) > 256 {
		return empty, model.ErrChannelAdmissionConflict
	}
	if body.Data.State != "ready" {
		if body.Data.Evidence != nil {
			return empty, model.ErrChannelAdmissionConflict
		}
		switch body.Data.State {
		case "pending":
			return empty, model.ErrMigrationProbePending
		case "missing_provenance":
			return empty, model.ErrMigrationProbeMissingProvenance
		case "unavailable":
			return empty, model.ErrChannelAdmissionUnavailable
		default:
			return empty, model.ErrChannelAdmissionConflict
		}
	}
	w := body.Data.Evidence
	if w == nil || body.Data.Reason != "" || w.Binding != r || w.EvidenceID == "" || len(w.EvidenceID) > 256 || !probeBillingDigest(w.ProvenanceHash) || !probeBillingDigest(w.AccountMembershipEvidenceHash) || w.AccountMembershipEvidenceKind != "fal-workspace-key-membership-v1" || w.Currency != "USD" {
		return empty, model.ErrChannelAdmissionConflict
	}
	cost, costOK := probeBillingDecimal(w.CostMicros)
	due, dueOK := probeBillingDecimal(w.CustomerDueMicros)
	charged, chargedOK := probeBillingDecimal(w.ChargedMicros)
	settled, settledOK := probeBillingDecimal(w.SettledAtMs)
	if !costOK || !dueOK || !chargedOK || !settledOK || settled == 0 || due != charged {
		return empty, model.ErrChannelAdmissionConflict
	}
	return model.MigrationProbeBillingEvidence{Binding: w.Binding, EvidenceID: w.EvidenceID, ProvenanceHash: w.ProvenanceHash, AccountMembershipEvidenceHash: w.AccountMembershipEvidenceHash, AccountMembershipEvidenceKind: w.AccountMembershipEvidenceKind, Currency: w.Currency, CostMicros: cost, CustomerDueMicros: due, ChargedMicros: charged, SettledAtMs: settled}, nil
}
