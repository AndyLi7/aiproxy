package model

import (
	"context"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestAccessConfigurationIdempotentCreationAndEmptyRouting(t *testing.T) {
	db, _ := routingFixture(t, true)
	require.NoError(t, db.AutoMigrate(&ChannelAccessConfiguration{}))
	makeChannel := func() *Channel {
		return &Channel{Name: strings.Repeat("测", 64), Type: ChannelTypeOpenAI, Key: "synthetic-access-key", BaseURL: "https://example.invalid", Status: ChannelStatusEnabled, Models: []string{}, ModelMapping: map[string]string{}, Sets: []string{"default"}}
	}
	receipt, e := EnsureAccessConfiguration(context.Background(), "access-1", makeChannel())
	require.NoError(t, e)
	replay, e := EnsureAccessConfiguration(context.Background(), "access-1", makeChannel())
	require.NoError(t, e)
	require.Equal(t, receipt, replay)
	var n int64
	require.NoError(t, db.Model(&ChannelAccessConfiguration{}).Count(&n).Error)
	require.EqualValues(t, 1, n)
	require.NoError(t, db.Model(&Channel{}).Count(&n).Error)
	require.EqualValues(t, 2, n) // fixture + exactly one connection
	q := routingRequest(t, &Channel{ID: receipt.Identity.ChannelID}, "first-model")
	q.Routing.Models = []string{"route-test"}
	q.Routing.ModelMapping = map[string]string{"route-test": "upstream"}
	added := PatchChannelRouting(context.Background(), receipt.Identity.ChannelID, q)
	require.Equal(t, "ready", added.State)
	replay, e = EnsureAccessConfiguration(context.Background(), "access-1", makeChannel())
	require.NoError(t, e)
	require.Equal(t, receipt, replay)
	live := ReadChannelRouting(context.Background(), receipt.Identity.ChannelID, "")
	require.Equal(t, []string{"route-test"}, live.Snapshot.Routing.Models)
	undo := routingRequest(t, &Channel{ID: receipt.Identity.ChannelID}, "undo-first-model")
	undo.Routing = added.Receipt.Before.Routing
	restored := PatchChannelRouting(context.Background(), receipt.Identity.ChannelID, undo)
	require.Equal(t, "ready", restored.State)
	require.Empty(t, restored.Snapshot.Routing.Models)
	bad := makeChannel()
	bad.Key = "different-synthetic-key"
	_, e = EnsureAccessConfiguration(context.Background(), "access-1", bad)
	require.Error(t, e)
	require.NoError(t, db.Model(&Channel{}).Where("id=?", receipt.Identity.ChannelID).UpdateColumn("status", ChannelStatusDisabled).Error)
	_, e = EnsureAccessConfiguration(context.Background(), "access-1", makeChannel())
	require.NoError(t, e)
	var stopped Channel
	require.NoError(t, db.First(&stopped, receipt.Identity.ChannelID).Error)
	require.Equal(t, ChannelStatusDisabled, stopped.Status)
	require.NoError(t, db.Delete(&stopped).Error)
	_, e = EnsureAccessConfiguration(context.Background(), "access-1", makeChannel())
	require.Error(t, e) // never resurrect deleted IDs
}
func TestAccessConfigurationFailsClosedWithoutStorageAndRejectsLegacyClear(t *testing.T) {
	db, ch := routingFixture(t, true)
	_, e := EnsureAccessConfiguration(context.Background(), "new-access", &Channel{Name: "test", Type: ChannelTypeOpenAI, Key: "synthetic-key", Status: 1})
	require.Error(t, e)
	var n int64
	require.NoError(t, db.Model(&Channel{}).Count(&n).Error)
	require.EqualValues(t, 1, n)
	require.NoError(t, db.AutoMigrate(&ChannelAccessConfiguration{}))
	q := routingRequest(t, ch, "legacy-empty")
	q.Routing.Models = []string{}
	q.Routing.ModelMapping = map[string]string{}
	require.Equal(t, "conflict", PatchChannelRouting(context.Background(), ch.ID, q).State)
}
