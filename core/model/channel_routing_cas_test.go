//nolint:testpackage // White-box regression exercises private model or runtime invariants.
package model

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/registryvalidation"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func routingFixture(t *testing.T, installed bool) (*gorm.DB, *Channel) {
	t.Helper()
	db := useSnapshotTestDB(t)
	require.NoError(t, db.AutoMigrate(&ChannelTest{}))
	require.NoError(
		t,
		db.Create(&ModelConfig{Model: "route-test", Type: mode.ChatCompletions}).Error,
	)
	ch := &Channel{
		Name:         "fal / credential [shadow v3]",
		Type:         ChannelTypeOpenAI,
		Key:          "synthetic-private-key",
		BaseURL:      "https://example.invalid",
		Models:       []string{"route-test"},
		Sets:         []string{"s"},
		ModelMapping: map[string]string{"route-test": "upstream"},
		Priority:     1,
	}
	require.NoError(t, db.Create(ch).Error)

	if installed {
		installRoutingFixture(t, db)
	}

	return db, ch
}

func installRoutingFixture(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.AutoMigrate(&ChannelRoutingState{}, &ChannelRoutingReceipt{}))
	require.NoError(
		t,
		db.Exec(
			"INSERT INTO channel_routing_states(channel_id,version,revision) SELECT id,1,1 FROM channels",
		).Error,
	)
}

func routingRequest(t *testing.T, ch *Channel, id string) ChannelRoutingRequest {
	t.Helper()

	r := ReadChannelRouting(context.Background(), ch.ID, "")
	require.Equal(t, "ready", r.State)
	s := r.Snapshot

	return ChannelRoutingRequest{
		1,
		id,
		s.Identity,
		s.Revision,
		s.RoutingHash,
		strconv.FormatInt(time.Now().Add(time.Minute).UnixMilli(), 10),
		s.Routing,
	}
}

func TestRoutingCASRawPreservationReceiptAndABA(t *testing.T) {
	db, ch := routingFixture(t, true)
	raw := `{"opaque":900719925474099312345,"nested":{"keep":"not-a-secret"},"x_token_platform_provider_bindings":{}}`
	require.NoError(t, db.Model(ch).UpdateColumn("configs", raw).Error)
	q := routingRequest(t, ch, "first-update")
	q.Routing.Priority = 7
	q.Routing.ProviderBindings = map[string]registryvalidation.ProviderBinding{
		"route-test": {
			Provider:     "fal",
			ID:           "endpoint",
			Revision:     "1",
			ContractHash: "sha256:" + strings.Repeat("a", 64),
		},
	}
	got := PatchChannelRouting(context.Background(), ch.ID, q)
	require.Equal(t, "ready", got.State)
	require.Equal(t, "2", got.Receipt.After.Revision)
	require.Equal(t, ch.ID, got.Snapshot.Identity.ChannelID)

	var saved string
	require.NoError(
		t,
		db.Table("channels").Select("configs").Where("id=?", ch.ID).Scan(&saved).Error,
	)
	require.Contains(t, saved, `"opaque":900719925474099312345`)
	require.Contains(t, saved, `"nested":{"keep":"not-a-secret"}`)

	var stored Channel
	require.NoError(t, db.First(&stored, ch.ID).Error)
	require.Equal(t, ch.Key, stored.Key)
	require.Equal(t, ch.BaseURL, stored.BaseURL)

	priority := int32(8)
	require.NoError(t, UpdateChannel(&stored, &ChannelPatch{Priority: &priority}))
	priority = 7
	require.NoError(t, UpdateChannel(&stored, &ChannelPatch{Priority: &priority}))
	require.NoError(t, UpdateChannel(&stored, &ChannelPatch{Priority: &priority}))

	replay := PatchChannelRouting(context.Background(), ch.ID, q)
	require.Equal(t, "ready", replay.State)
	require.Equal(t, "5", replay.Snapshot.Revision)
	require.Equal(t, "2", replay.Receipt.After.Revision)
	require.Equal(t, got.Receipt, replay.Receipt)

	stale := q
	stale.RequestID = "after-foreign-write"
	stale.ExpectedRevision = "2"
	stale.ExpectedRoutingHash = got.Receipt.After.RoutingHash
	require.Equal(t, "conflict", PatchChannelRouting(context.Background(), ch.ID, stale).State)

	q.Routing.Priority = 99
	require.Equal(t, "conflict", PatchChannelRouting(context.Background(), ch.ID, q).State)
	require.NotContains(t, routingJSON(replay), ch.Key)
}

func TestRoutingCASCompensationDeadlineAndIdentity(t *testing.T) {
	_, ch := routingFixture(t, true)
	q := routingRequest(t, ch, "forward")
	q.Routing.Priority = 9
	first := PatchChannelRouting(context.Background(), ch.ID, q)
	require.Equal(t, "ready", first.State)
	undo := routingRequest(t, ch, "compensate")
	undo.Routing = first.Receipt.Before.Routing
	require.Equal(t, "ready", PatchChannelRouting(context.Background(), ch.ID, undo).State)
	expired := routingRequest(t, ch, "expired")
	expired.NotAfterMs = strconv.FormatInt(time.Now().Add(-time.Second).UnixMilli(), 10)
	require.Equal(t, "conflict", PatchChannelRouting(context.Background(), ch.ID, expired).State)
	require.Equal(t, "absent", ReadChannelRouting(context.Background(), ch.ID, "expired").State)
	bad := routingRequest(t, ch, "wrong-identity")
	bad.ExpectedIdentity.KeyDigest = strings.Repeat("b", 64)
	require.Equal(t, "conflict", PatchChannelRouting(context.Background(), ch.ID, bad).State)

	var n int64
	require.NoError(t, DB.Model(&Channel{}).Count(&n).Error)
	require.EqualValues(t, 1, n)
}

func TestRoutingCASInstallationAndLegacyWrites(t *testing.T) {
	for _, state := range []string{"absent", "partial", "missing-row"} {
		t.Run(state, func(t *testing.T) {
			db, ch := routingFixture(t, false)
			if state != "absent" {
				require.NoError(t, db.AutoMigrate(&ChannelRoutingState{}))

				if state == "missing-row" {
					require.NoError(t, db.AutoMigrate(&ChannelRoutingReceipt{}))
				}
			}

			require.Equal(
				t,
				"unavailable",
				ReadChannelRouting(context.Background(), ch.ID, "").State,
			)

			priority := int32(9)

			err := UpdateChannel(ch, &ChannelPatch{Priority: &priority})
			if state == "absent" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.Error(
					t,
					UpdateChannelStatusByIDContext(
						context.Background(),
						ch.ID,
						ChannelStatusDisabled,
					),
				)
				require.Error(t, DeleteChannelByID(ch.ID))
				require.Error(
					t,
					BatchInsertChannels(
						[]*Channel{
							{
								Name:   "new",
								Type:   ChannelTypeOpenAI,
								Key:    "synthetic-new",
								Models: []string{"route-test"},
							},
						},
					),
				)
			}
		})
	}
}

func TestRoutingCASStrictJSONAndCorruption(t *testing.T) {
	db, ch := routingFixture(t, true)
	q := routingRequest(t, ch, "strict-request")
	raw := routingJSON(q)
	_, e := ParseChannelRoutingRequest([]byte(raw))
	require.NoError(t, e)

	for _, bad := range []string{strings.Replace(raw, `"version":1`, `"version":1,"version":1`, 1), strings.Replace(raw, `"version":1`, `"version":1,"key":"secret"`, 1), strings.Replace(raw, `"expectedRevision":"1"`, `"expectedRevision":"01"`, 1), strings.Replace(raw, `"providerBindings":{}`, `"providerBindings":{"m":{"provider":"fal","id":"x","revision":"1","contractHash":"sha256:`+strings.Repeat("a", 64)+`","extra":1}}`, 1)} {
		_, e := ParseChannelRoutingRequest([]byte(bad))
		require.Error(t, e)
	}

	require.NoError(
		t,
		db.Model(ch).
			UpdateColumn("configs", `{"x_token_platform_provider_bindings":{"m":{"provider":"fal","id":"x","revision":"1","contractHash":"sha256:`+strings.Repeat("a", 64)+`","future":1}}}`).
			Error,
	)
	require.Equal(t, "unavailable", ReadChannelRouting(context.Background(), ch.ID, "").State)
}

func TestRoutingCASDeletedHistoricalReceipt(t *testing.T) {
	_, ch := routingFixture(t, true)
	q := routingRequest(t, ch, "deleted-receipt")
	require.Equal(t, "ready", PatchChannelRouting(context.Background(), ch.ID, q).State)
	require.NoError(t, DeleteChannelByID(ch.ID))
	require.Equal(t, "conflict", ReadChannelRouting(context.Background(), ch.ID, "").State)
	r := ReadChannelRouting(context.Background(), ch.ID, q.RequestID)
	require.Equal(t, "ready", r.State)
	require.Nil(t, r.Snapshot)
	require.NotNil(t, r.Receipt)
}

func TestRoutingCASRawBindingsReplacement(t *testing.T) {
	for _, raw := range []string{`{}`, ` { "big" : 900719925474099312345 } `, `{"a":[1,{"b":2}],"x_token_platform_provider_bindings": { },"z":-900719925474099312345}`} {
		next, e := replaceRoutingBindings(raw, map[string]registryvalidation.ProviderBinding{})
		require.NoError(t, e)

		var obj map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(next), &obj))
		require.JSONEq(t, `{}`, string(obj[ChannelConfigProviderBindings]))

		if strings.Contains(raw, "900719925474099312345") {
			require.Contains(t, next, "900719925474099312345")
		}
	}
}

func TestRoutingCASSQLiteReceiptSerializesBeforeClock(t *testing.T) {
	db, ch := routingFixture(t, true)
	require.NoError(t, db.Exec("PRAGMA journal_mode=WAL").Error)
	q := routingRequest(t, ch, "concurrent-receipt")
	q.Routing.Priority = 5
	wrote := make(chan struct{})

	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	require.NoError(
		t,
		db.Callback().
			Create().
			After("gorm:create").
			Register("routing_test_hold", func(tx *gorm.DB) {
				if tx.Statement.Table == "channel_routing_receipts" {
					close(wrote)
					<-release
				}
			}),
	)
	defer func() {
		if err := db.Callback().Create().Remove("routing_test_hold"); err !=
			nil {
			t.
				Error(err)
		}
	}()

	writeDone := make(chan ChannelRoutingResult, 1)
	go func() { writeDone <- PatchChannelRouting(context.Background(), ch.ID, q) }()

	<-wrote

	readDone := make(chan ChannelRoutingResult, 1)
	go func() { readDone <- ReadChannelRouting(context.Background(), ch.ID, q.RequestID) }()

	select {
	case got := <-readDone:
		require.Equal(
			t,
			"unavailable",
			got.State,
			"must never observe old absent after a pending commit",
		)
		close(release)
		require.Equal(t, "ready", (<-writeDone).State)

		return
	case <-time.After(80 * time.Millisecond):
	}

	close(release)
	require.Equal(t, "ready", (<-writeDone).State)

	got := <-readDone
	require.Equal(t, "ready", got.State)
	require.NotNil(t, got.Receipt)
	require.NotNil(t, got.ObservedAtMs)
}
