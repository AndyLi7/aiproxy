package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// ChannelIdentity pins the physical upstream identity, independently of models,
// sets, prices, status and routing-only configs. No plaintext key is persisted
// in admission/barrier records or returned as part of this value.
type ChannelIdentity struct {
	ChannelID    int    `json:"channelId"`
	KeyDigest    string `json:"keyDigest"`
	IdentityHash string `json:"identityHash"`
}

func ChannelIdentityOf(channel *Channel) (ChannelIdentity, error) {
	if channel == nil || channel.ID <= 0 || channelKeyEmpty(channel.Key) {
		return ChannelIdentity{}, errors.New("channel identity unavailable")
	}

	configs := map[string]any{}
	for k, v := range channel.Configs {
		if routingOnlyChannelConfig(k) || v == nil {
			continue
		}

		configs[k] = v
	}

	digest := sha256.Sum256([]byte(channel.Key))
	keyDigest := hex.EncodeToString(digest[:])

	encoded, err := json.Marshal(struct {
		Type          ChannelType    `json:"type"`
		KeyDigest     string         `json:"keyDigest"`
		BaseURL       string         `json:"baseUrl"`
		ProxyURL      string         `json:"proxyUrl"`
		SkipTLSVerify bool           `json:"skipTlsVerify"`
		Configs       map[string]any `json:"configs"`
	}{channel.Type, keyDigest, channel.BaseURL, channel.ProxyURL, channel.SkipTLSVerify, configs})
	if err != nil {
		return ChannelIdentity{}, errors.New("channel identity unavailable")
	}

	identity := sha256.Sum256(encoded)

	return ChannelIdentity{
		ChannelID:    channel.ID,
		KeyDigest:    keyDigest,
		IdentityHash: hex.EncodeToString(identity[:]),
	}, nil
}

func validChannelIdentity(identity ChannelIdentity) bool {
	return identity.ChannelID > 0 && fullIdentityDigest(identity.KeyDigest) &&
		fullIdentityDigest(identity.IdentityHash)
}

func fullIdentityDigest(s string) bool {
	if len(s) != 64 {
		return false
	}

	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}
