package model

// Inventory is a read-only review source, never a deployment/probe proof. A
// complete safe projection is required; omitting sensitive differences would
// incorrectly permit the application to classify a changed identity as reuse.
import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

const MigrationInventoryMaxRows = 4096
const MigrationInventoryMaxBytes = 16 << 20

type MigrationInventory struct {
	Version            int                         `json:"version"`
	Source             string                      `json:"source"`
	State              string                      `json:"state"`
	Complete           bool                        `json:"complete"`
	ReasonCode         string                      `json:"reasonCode,omitempty"`
	Epoch              string                      `json:"epoch,omitempty"`
	Revision           string                      `json:"revision,omitempty"`
	StorageGeneration  string                      `json:"storageGeneration,omitempty"`
	Features           []string                    `json:"features"`
	ConfigModels       []string                    `json:"configModels"`
	PinHash            string                      `json:"pinHash,omitempty"`
	SourceSnapshotHash string                      `json:"sourceSnapshotHash,omitempty"`
	Channels           []MigrationInventoryChannel `json:"channels"`
	Configs            []MigrationInventoryConfig  `json:"configs"`
}
type MigrationInventoryConfig struct {
	Model   string `json:"model"`
	RawJSON string `json:"rawJson"`
}
type MigrationInventoryChannel struct {
	Identity           ChannelIdentity                      `json:"identity"`
	Routing            MigrationInventoryRouting            `json:"routing"`
	IdentityProjection MigrationInventoryIdentityProjection `json:"identityProjection"`
	ProjectionHash     string                               `json:"projectionHash"`
}
type MigrationInventoryIdentityProjection struct {
	BaseURL       string  `json:"baseUrl"`
	ProxyURL      string  `json:"proxyUrl"`
	SkipTLSVerify bool    `json:"skipTlsVerify"`
	ConfigsJSON   *string `json:"configsJson"`
}
type MigrationInventoryRouting struct {
	ID                     int               `json:"id"`
	Name                   string            `json:"name"`
	Type                   ChannelType       `json:"type"`
	Status                 int               `json:"status"`
	Models                 []string          `json:"models"`
	ModelMapping           map[string]string `json:"modelMapping"`
	Sets                   []string          `json:"sets"`
	Priority               int32             `json:"priority"`
	BackupOnly             bool              `json:"backupOnly"`
	EnabledNoPermissionBan bool              `json:"enabledNoPermissionBan"`
	WarnErrorRate          float64           `json:"warnErrorRate"`
	MaxErrorRate           float64           `json:"maxErrorRate"`
	DeletedAt              *string           `json:"deletedAt"`
}

func inventoryState(state, reason string) MigrationInventory {
	return MigrationInventory{Version: 1, Source: "gateway-persistent-inventory-v1", State: state, ReasonCode: reason}
}

// ReadMigrationInventory performs only SELECTs. PostgreSQL uses a repeatable
// snapshot plus the shared control lock (not the command transaction's UPDATE).
// SQLite's read transaction supplies the corresponding consistent snapshot.
// Features describe this binary, not an all-replica deployment attestation.
func (s *ChannelAdmissionStore) ReadMigrationInventory(ctx context.Context, features []string) MigrationInventory {
	unsupported := inventoryState("unsupported", "inventory_unavailable")
	if s == nil || s.DB == nil || s.LogDB != s.DB {
		return unsupported
	}
	dialect := s.DB.Dialector.Name()
	if dialect != "sqlite" && dialect != "postgres" {
		return unsupported
	}
	options := &sql.TxOptions{}
	if dialect == "postgres" {
		options.Isolation = sql.LevelRepeatableRead
	}
	result := unsupported
	err := s.DB.WithContext(ctx).Session(&gorm.Session{Logger: logger.Discard}).Transaction(func(tx *gorm.DB) error {
		exists, err := channelAdmissionTablesExist(tx)
		if err != nil {
			return err
		}
		if !exists {
			result = inventoryState("legacy", "admission_storage_absent")
			return nil
		}
		if !ChannelAdmissionStorageReady(tx) {
			return ErrChannelAdmissionUnavailable
		}
		var control ChannelAdmissionControl
		q := tx
		if dialect == "postgres" {
			q = q.Clauses(clause.Locking{Strength: "SHARE"})
		}
		if err = q.First(&control, 1).Error; err != nil {
			return err
		}
		result, err = migrationInventoryTx(tx, &control, features)
		return err
	}, options)
	if err != nil {
		return unsupported
	}
	return result
}

// migrationInventoryTx keeps the original complete inventory producer inside the
// caller's transaction. It performs no writes and never obtains a second snapshot.
func migrationInventoryTx(tx *gorm.DB, c *ChannelAdmissionControl, features []string) (result MigrationInventory, err error) {
	result = inventoryState("unsupported", "inventory_unavailable")
	control := *c
	err = func() error {
		if control.Version != ChannelAdmissionProtocolVersion || control.Revision < 0 {
			return ErrChannelAdmissionUnavailable
		}
		if control.Epoch == "" {
			never, e := channelAdmissionNeverActivated(tx, &control)
			if e != nil || !never {
				return ErrChannelAdmissionUnavailable
			}
			result = inventoryState("legacy", "admission_never_activated")
			return nil
		}
		if control.StorageGeneration == "" || control.EpochSequence <= 0 || control.ActiveProofID == "" {
			return ErrChannelAdmissionUnavailable
		}
		if err = migrationStorageReady(tx); err != nil {
			return err
		}
		// Inspect raw JSON before GORM's serializer can recursively decode it.
		rawChannels, err := migrationRawRows(tx.Table("channels").Select("id, configs, models, model_mapping, sets").Order("id").Limit(MigrationInventoryMaxRows + 1))
		if err != nil {
			return err
		}
		rows, err := migrationRawRows(tx.Table("model_configs").Order("model").Limit(MigrationInventoryMaxRows + 1))
		if err != nil {
			return err
		}
		if len(rawChannels) > MigrationInventoryMaxRows || len(rows) > MigrationInventoryMaxRows {
			result = inventoryState("unsupported", "inventory_limit_exceeded")
			return nil
		}
		for _, row := range rawChannels {
			if !inventoryReviewText(row["configs"], "channel") {
				result = inventoryState("unsupported", "inventory_unreviewed_config")
				return nil
			}
			for _, column := range []string{"models", "model_mapping", "sets"} {
				if raw := row[column]; raw != nil && raw != "" {
					text, ok := raw.(string)
					if !ok {
						return ErrChannelAdmissionConflict
					}
					if _, ok = inventoryDecode([]byte(text)); !ok {
						result = inventoryState("unsupported", "inventory_unreviewed_config")
						return nil
					}
				}
			}
		}
		var channels []Channel
		if err = tx.Unscoped().Order("id").Limit(MigrationInventoryMaxRows + 1).Find(&channels).Error; err != nil {
			return err
		}
		out := MigrationInventory{Version: 1, Source: "gateway-persistent-inventory-v1", State: "ready", Complete: true, Epoch: control.Epoch, Revision: strconv.FormatInt(control.Revision, 10), StorageGeneration: control.StorageGeneration, Features: append([]string{}, features...), ConfigModels: []string{}, Channels: []MigrationInventoryChannel{}, Configs: []MigrationInventoryConfig{}}
		sort.Strings(out.Features)
		out.PinHash, err = admissionHash(rows)
		if err != nil {
			return err
		}
		if len(rawChannels) != len(channels) {
			return ErrChannelAdmissionConflict
		}
		secrets := make([]string, 0, len(channels))
		for _, ch := range channels {
			if ch.Key != "" {
				secrets = append(secrets, ch.Key)
			}
		}
		for i, ch := range channels {
			identity, e := ChannelIdentityOf(&ch)
			if e != nil {
				return e
			}
			var config *string
			if !inventoryReviewText(rawChannels[i]["configs"], "channel") {
				result = inventoryState("unsupported", "inventory_unreviewed_config")
				return nil
			}
			if v := rawChannels[i]["configs"]; v != nil {
				text, ok := v.(string)
				if !ok {
					return ErrChannelAdmissionConflict
				}
				if text != "" {
					config = &text
				}
			}
			var deleted *string
			if ch.DeletedAt.Valid {
				text := ch.DeletedAt.Time.UTC().Format(time.RFC3339Nano)
				deleted = &text
			}
			// Normalize absent routing collections for the typed review wire only.
			if ch.Models == nil {
				ch.Models = []string{}
			}
			if ch.Sets == nil {
				ch.Sets = []string{}
			}
			if ch.ModelMapping == nil {
				ch.ModelMapping = map[string]string{}
			}
			item := MigrationInventoryChannel{Identity: identity, Routing: MigrationInventoryRouting{ch.ID, ch.Name, ch.Type, ch.Status, ch.Models, ch.ModelMapping, ch.Sets, ch.Priority, ch.BackupOnly, ch.EnabledNoPermissionBan, ch.WarnErrorRate, ch.MaxErrorRate, deleted}, IdentityProjection: MigrationInventoryIdentityProjection{ch.BaseURL, ch.ProxyURL, ch.SkipTLSVerify, config}}
			item.ProjectionHash, e = admissionHash(struct {
				Routing  MigrationInventoryRouting
				Identity MigrationInventoryIdentityProjection
			}{item.Routing, item.IdentityProjection})
			if e != nil {
				return e
			}
			out.Channels = append(out.Channels, item)
		}
		schema := &gorm.Statement{DB: tx}
		if err = schema.Parse(&ModelConfig{}); err != nil {
			return err
		}
		knownColumns := map[string]bool{}
		for _, name := range schema.Schema.DBNames {
			knownColumns[name] = true
		}
		for _, row := range rows {
			for column, value := range row {
				if !knownColumns[column] && !inventoryNumbers(value) {
					result = inventoryState("unsupported", "inventory_unreviewed_config")
					return nil
				}
			}
			if !inventoryReviewText(row["config"], "config") || !inventoryReviewText(row["plugin"], "plugin") {
				result = inventoryState("unsupported", "inventory_unreviewed_config")
				return nil
			}
			name, ok := row["model"].(string)
			if !ok || name == "" {
				return ErrChannelAdmissionConflict
			}
			raw, e := json.Marshal(row)
			if e != nil {
				return e
			}
			out.ConfigModels = append(out.ConfigModels, name)
			out.Configs = append(out.Configs, MigrationInventoryConfig{name, string(raw)})
		}
		encoded, err := json.Marshal(out)
		if err != nil {
			return err
		}
		if len(encoded) > MigrationInventoryMaxBytes {
			result = inventoryState("unsupported", "inventory_limit_exceeded")
			return nil
		}
		if !inventorySafeJSON(encoded, secrets, 0) {
			result = inventoryState("unsupported", "inventory_sensitive_or_ambiguous")
			return nil
		}
		out.SourceSnapshotHash, err = admissionHash(out)
		if err != nil {
			return err
		}
		result = out
		return nil
	}()
	return
}

// No secret value is included in an error. Raw JSON strings (config/plugin and
// nested plugin specs) are recursively inspected without rewriting their bytes.
// Credential-bearing or query-bearing URLs are unsupported, never redacted.
func inventorySafeJSON(raw []byte, secrets []string, depth int) bool {
	if depth > 32 {
		return false
	}
	value, ok := inventoryDecode(raw)
	if !ok {
		return false
	}
	return inventorySafeValue(value, secrets, depth)
}
func inventorySafeValue(value any, secrets []string, depth int) bool {
	if depth > 32 {
		return false
	}
	switch v := value.(type) {
	case map[string]any:
		for k, x := range v {
			n := strings.ToLower(strings.NewReplacer("_", "", "-", "", ".", "").Replace(k))
			switch n {
			case "key", "apikey", "authorization", "proxyauthorization", "password", "passwd", "secret", "clientsecret", "accesstoken", "refreshtoken", "privatekey", "cookie", "setcookie", "headers", "httpheaders", "xapikey", "token", "auth", "credentials", "credential", "credentialjson", "credentialsecret", "authentication", "accesskey", "secretkey", "sessiontoken":
				return false
			}
			if !inventorySafeValue(x, secrets, depth+1) {
				return false
			}
		}
	case []any:
		for _, x := range v {
			if !inventorySafeValue(x, secrets, depth+1) {
				return false
			}
		}
	case string:
		for _, secret := range secrets {
			if v == secret || (len(secret) >= 8 && strings.Contains(v, secret)) {
				return false
			}
		}
		t := strings.TrimSpace(v)
		lower := strings.ToLower(t)
		if strings.Contains(lower, "bearer ") || strings.Contains(lower, "basic ") || strings.Contains(t, "-----BEGIN ") || strings.HasPrefix(t, "sk-") || strings.HasPrefix(t, "eyJ") {
			return false
		}
		if (strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")) && json.Valid([]byte(t)) {
			return inventorySafeJSON([]byte(t), secrets, depth+1)
		}
		// Public contract descriptions can contain prose around documentation
		// links. Inspect URL tokens, not the entire sentence as a URL.
		for _, token := range inventoryURLTokens.FindAllString(t, -1) {
			u, e := url.Parse(strings.TrimRight(token, ".,);"))
			if e != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return false
			}
		}
	case nil, bool, json.Number:
		return true
	default:
		return false
	}
	return true
}

var inventoryURLTokens = regexp.MustCompile("(?i)[a-z][a-z0-9+.-]*://[^\\s<>\"`]+")
