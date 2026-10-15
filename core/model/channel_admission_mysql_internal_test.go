//nolint:testpackage // Reuses the recording MySQL driver; no server is contacted.
package model

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAdmissionMySQLRejectsBeforeAnyWrite(t *testing.T) {
	record := &queueSQLRecorder{}
	db := queueMySQLRecordingDB(t, record)
	store := &ChannelAdmissionStore{DB: db, LogDB: db, LoadDeployment: func(context.Context) (VerifiedDeploymentProof, error) {
		t.Fatal("unsupported backend must be rejected before proof evaluation")
		return VerifiedDeploymentProof{}, nil
	}}
	from := ChannelIdentity{ChannelID: 1, KeyDigest: strings.Repeat("a", 64), IdentityHash: strings.Repeat("b", 64)}
	to := ChannelIdentity{ChannelID: 2, KeyDigest: strings.Repeat("c", 64), IdentityHash: strings.Repeat("d", 64)}
	_, err := store.Seal(context.Background(), ChannelSealRequest{OperationID: "op", Epoch: "test", From: from, To: to})
	require.ErrorIs(t, err, ErrChannelAdmissionUnavailable)
	_, _, err = store.Admit(context.Background(), ChannelAdmissionRequest{RequestID: "id", GroupID: "g", TokenID: 1, Lane: "native", RequestHash: strings.Repeat("e", 64), Identity: from}, nil)
	require.ErrorIs(t, err, ErrChannelAdmissionUnavailable)
	for _, event := range record.events {
		require.Equal(t, "query", event, "no transaction/write may begin on an unsupported backend")
	}
}
