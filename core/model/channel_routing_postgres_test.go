package model_test

import (
	"context"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"strconv"
	"testing"
	"time"
)

func TestPostgresRoutingCASBoundaries(t *testing.T) {
	for _, scenario := range []string{"contenders", "old-put", "expired-lock", "cancelled-lock", "expired-after-write", "receipt-read-lock"} {
		t.Run(scenario, func(t *testing.T) {
			db := queueLockPostgres(t)
			oldDB, oldLog := model.DB, model.LogDB
			model.DB, model.LogDB = db, db
			t.Cleanup(func() { model.DB, model.LogDB = oldDB, oldLog })
			require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.ChannelTest{}, &model.ModelConfig{}, &model.ChannelRoutingState{}, &model.ChannelRoutingReceipt{}))
			require.NoError(t, db.Create(&model.ModelConfig{Model: "pg-route", Type: mode.ChatCompletions}).Error)
			ch := &model.Channel{Name: "ordinary [shadow v2]", Type: model.ChannelTypeOpenAI, Key: "synthetic-pg-key", Models: []string{"pg-route"}, Sets: []string{"default"}, ModelMapping: map[string]string{}, Priority: 1}
			require.NoError(t, db.Create(ch).Error)
			require.NoError(t, db.Create(&model.ChannelRoutingState{ChannelID: ch.ID, Version: 1, Revision: 1}).Error)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			snap := model.ReadChannelRouting(ctx, ch.ID, "")
			require.Equal(t, "ready", snap.State)
			q := model.ChannelRoutingRequest{Version: 1, RequestID: "pg-request", ExpectedIdentity: snap.Snapshot.Identity, ExpectedRevision: "1", ExpectedRoutingHash: snap.Snapshot.RoutingHash, NotAfterMs: strconv.FormatInt(time.Now().Add(5*time.Second).UnixMilli(), 10), Routing: snap.Snapshot.Routing}
			q.Routing.Priority = 8
			if scenario == "expired-after-write" {
				require.NoError(t, db.Exec("CREATE FUNCTION routing_slow_receipt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(0.35); RETURN NEW; END $$").Error)
				require.NoError(t, db.Exec("CREATE TRIGGER routing_slow BEFORE INSERT ON channel_routing_receipts FOR EACH ROW EXECUTE FUNCTION routing_slow_receipt()").Error)
				q.NotAfterMs = strconv.FormatInt(time.Now().Add(200*time.Millisecond).UnixMilli(), 10)
				got := model.PatchChannelRouting(ctx, ch.ID, q)
				require.Equal(t, "conflict", got.State)
				require.Equal(t, "1", model.ReadChannelRouting(ctx, ch.ID, "").Snapshot.Revision)
				require.Equal(t, "absent", model.ReadChannelRouting(ctx, ch.ID, q.RequestID).State)
				return
			}
			pool, e := db.DB()
			require.NoError(t, e)
			holder, e := pool.BeginTx(context.Background(), nil)
			require.NoError(t, e)
			defer holder.Rollback()
			var id int
			require.NoError(t, holder.QueryRowContext(ctx, "SELECT id FROM channels WHERE id=$1 FOR UPDATE", ch.ID).Scan(&id))
			if scenario == "expired-lock" {
				q.NotAfterMs = strconv.FormatInt(time.Now().Add(200*time.Millisecond).UnixMilli(), 10)
			}
			done := make(chan model.ChannelRoutingResult, 1)
			if scenario == "receipt-read-lock" {
				go func() { done <- model.ReadChannelRouting(ctx, ch.ID, q.RequestID) }()
			} else {
				go func() { done <- model.PatchChannelRouting(ctx, ch.ID, q) }()
			}
			select {
			case got := <-done:
				t.Fatalf("did not wait for channel lock: %s", got.State)
			case <-time.After(80 * time.Millisecond):
			}
			var second chan model.ChannelRoutingResult
			var put chan error
			if scenario == "contenders" {
				second = make(chan model.ChannelRoutingResult, 1)
				q2 := q
				q2.RequestID = "pg-other"
				q2.Routing.Priority = 9
				go func() { second <- model.PatchChannelRouting(ctx, ch.ID, q2) }()
			}
			if scenario == "old-put" {
				put = make(chan error, 1)
				go func() { p := int32(2); put <- model.UpdateChannel(ch, &model.ChannelPatch{Priority: &p}) }()
			}
			if scenario == "expired-lock" {
				time.Sleep(180 * time.Millisecond)
			}
			if scenario == "cancelled-lock" {
				cancel()
				got := <-done
				require.Equal(t, "unavailable", got.State)
				require.NoError(t, holder.Rollback())
				return
			}
			require.NoError(t, holder.Commit())
			got := <-done
			switch scenario {
			case "contenders":
				other := <-second
				require.ElementsMatch(t, []string{"ready", "conflict"}, []string{got.State, other.State})
				require.Equal(t, "2", model.ReadChannelRouting(ctx, ch.ID, "").Snapshot.Revision)
			case "old-put":
				require.NoError(t, <-put)
				require.Contains(t, []string{"ready", "conflict"}, got.State)
				current := model.ReadChannelRouting(ctx, ch.ID, "")
				if got.State == "ready" {
					require.Equal(t, "3", current.Snapshot.Revision)
				} else {
					require.Equal(t, "2", current.Snapshot.Revision)
				}
			case "expired-lock":
				require.Equal(t, "conflict", got.State)
				require.Equal(t, "1", model.ReadChannelRouting(ctx, ch.ID, "").Snapshot.Revision)
			case "receipt-read-lock":
				require.Equal(t, "absent", got.State)
				require.NotNil(t, got.ObservedAtMs)
				n, e := strconv.ParseInt(*got.ObservedAtMs, 10, 64)
				require.NoError(t, e)
				require.GreaterOrEqual(t, n, time.Now().Add(-time.Second).UnixMilli())
			}
		})
	}
}
