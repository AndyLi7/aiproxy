package model_test

import (
	"testing"

	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// A rejected key is still named in the request log, so its rejection counts as
// that key's call. The token never authorizes the request.
func TestGetAndValidateTokenReturnsRejectedKeyForAttribution(t *testing.T) {
	oldRedis := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = oldRedis })

	disabled := &model.TokenCache{ID: 51, Key: "attribution-disabled", Name: "off", Group: "g", Status: model.TokenStatusDisabled}
	exhausted := &model.TokenCache{ID: 52, Key: "attribution-exhausted", Name: "spent", Group: "g", Status: model.TokenStatusEnabled, Quota: 1, UsedAmount: 2}

	for _, token := range []*model.TokenCache{disabled, exhausted} {
		require.NoError(t, model.CacheSetToken(token))

		key := token.Key
		t.Cleanup(func() { require.NoError(t, model.CacheDeleteToken(key)) })
	}

	got, err := model.GetAndValidateToken(disabled.Key)
	require.ErrorIs(t, err, model.ErrTokenDisabled)
	require.NotNil(t, got)
	require.Equal(t, 51, got.ID)
	require.Equal(t, "g", got.Group)

	got, err = model.GetAndValidateToken(exhausted.Key)
	require.ErrorIs(t, err, model.ErrTokenQuotaExhausted)
	require.NotNil(t, got)
	require.Equal(t, 52, got.ID)
}
