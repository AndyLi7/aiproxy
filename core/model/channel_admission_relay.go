package model

import (
	"context"

	"gorm.io/gorm"
)

// RelayAdmissionScope describes the authenticated, server-built request. It
// never changes billing ownership. Administrative channel tests and internal
// relay tokens have no customer group/token; their identity is still subject
// to exactly the same physical-channel admission rule.
type RelayAdmissionScope struct {
	GroupID            string
	TokenID            int
	BillingOperationID string
	RequestID          string
	Mode               int
	Model              string
}

// AdmitRelayAttempt records use before a synchronous/streaming/generic-async
// handler can send upstream. Every attempt gets a fresh server nonce: replaying
// a customer request ID must not replay an old admission after a family seal.
// Returning a nil use means only the never-installed/never-activated legacy
// path. Open uses survive unknown acceptance, crashes and absent async rows.
func AdmitRelayAttempt(ctx context.Context, channel *Channel, scope RelayAdmissionScope) (*ChannelAdmissionUse, error) {
	use, _, err := AdmitRelayExecution(ctx, channel, scope)
	return use, err
}

// AdmitRelayExecution records the active call atomically with new admission
// when the reviewed execution extension is installed. Nil handle on v1-only
// storage grants no completion/coverage fact; that use remains open.
func AdmitRelayExecution(ctx context.Context, channel *Channel, scope RelayAdmissionScope, intents ...*MeasuredRelayIntent) (*ChannelAdmissionUse, *RelayExecutionHandle, error) {
	if len(intents) > 1 {
		return nil, nil, ErrChannelAdmissionConflict
	}
	var execution *RelayExecutionHandle
	store, err := channelAdmissionRuntimeStore(LogDB)
	if err != nil || store == nil {
		return nil, nil, err
	}
	generation, err := newAdmissionGeneration()
	if err != nil {
		return nil, nil, err
	}
	identity, identityErr := ChannelIdentityOf(channel)
	requestHash, err := admissionHash([]any{"relay-attempt-v1", scope, identity})
	if err != nil {
		return nil, nil, err
	}
	resource, err := admissionHash([]any{"relay", scope.GroupID, scope.TokenID, scope.BillingOperationID})
	if err != nil {
		return nil, nil, err
	}
	// The admission ID namespace is separate from customer ownership fields.
	// Hash the complete authenticated scope so internal token ID zero needs no
	// invented customer token, and cannot collide with a customer group name.
	actor, err := admissionHash([]any{"relay-actor-v1", scope.GroupID, scope.TokenID})
	if err != nil {
		return nil, nil, err
	}
	input := ChannelAdmissionRequest{RequestID: generation, ResourceKey: resource,
		GroupID: actor, TokenID: 1, Lane: "relay", RequestHash: requestHash, Identity: identity}
	use, created, err := store.admit(ctx, input, nil, func(tx *gorm.DB, control *ChannelAdmissionControl) (bool, error) {
		inactive, err := channelAdmissionNeverActivated(tx, control)
		if err != nil || inactive {
			return inactive, err
		}
		if identityErr != nil || scope.BillingOperationID == "" || len(scope.BillingOperationID) > 256 ||
			len(scope.GroupID) > 128 || scope.TokenID < 0 {
			return false, ErrChannelAdmissionConflict
		}
		return false, nil
	}, func(tx *gorm.DB, c *ChannelAdmissionControl, use *ChannelAdmissionUse) error {
		nonce, e := newAdmissionGeneration()
		if e != nil {
			return e
		}
		execution, e = store.startRelayExecution(tx, c, use, nonce)
		if e == nil && len(intents) == 1 && intents[0] != nil {
			e = intents[0].start(tx, c, execution, use, scope)
		}
		return e
	})
	if err != nil {
		return nil, nil, err
	}
	if use != nil && !created {
		return nil, nil, ErrChannelAdmissionConflict
	}
	return use, execution, nil
}
