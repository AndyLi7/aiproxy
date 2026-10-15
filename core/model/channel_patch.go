package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// ChannelPatch separates optional writes from stored channel values.
// Nil pointers leave columns unchanged; non-nil zero values are persisted.
type ChannelPatch struct {
	Type                    *ChannelType       `json:"type,omitempty"`
	Name                    *string            `json:"name,omitempty"`
	Remark                  *string            `json:"remark,omitempty"`
	Key                     *string            `json:"key,omitempty"`
	BaseURL                 *string            `json:"base_url,omitempty"`
	ProxyURL                *string            `json:"proxy_url,omitempty"`
	Models                  *[]string          `json:"models,omitempty"                     gorm:"serializer:fastjson;type:text"`
	ModelMapping            *map[string]string `json:"model_mapping,omitempty"              gorm:"serializer:fastjson;type:text"`
	Configs                 *ChannelConfigs    `json:"configs,omitempty"                    gorm:"serializer:fastjson;type:text"`
	Priority                *int32             `json:"priority,omitempty"`
	BackupOnly              *bool              `json:"backup_only,omitempty"`
	Sets                    *[]string          `json:"sets,omitempty"                       gorm:"serializer:fastjson;type:text"`
	EnabledAutoBalanceCheck *bool              `json:"enabled_auto_balance_check,omitempty"`
	SkipTLSVerify           *bool              `json:"skip_tls_verify,omitempty"`
	EnabledNoPermissionBan  *bool              `json:"enabled_no_permission_ban,omitempty"`
	WarnErrorRate           *float64           `json:"warn_error_rate,omitempty"`
	MaxErrorRate            *float64           `json:"max_error_rate,omitempty"`
	BalanceThreshold        *float64           `json:"balance_threshold,omitempty"`
}

// Channel writes refused for their content; the admin API answers 400.
var (
	// ErrChannelModelsEmpty refuses an explicit empty models list: a channel
	// without models serves every default model of its type
	// (initializeChannelModels), the widest routing there is.
	ErrChannelModelsEmpty = errors.New(
		"models must not be an empty list: an empty list makes the channel serve every default model of its type;" +
			" omit models to keep the current list, or disable the channel",
	)
	// ErrChannelKeyEmpty refuses a write that would newly leave a channel
	// enabled without a key: every call routed to it would fail.
	ErrChannelKeyEmpty = errors.New(
		"channel key is empty: an enabled channel needs a key; set a key first or keep the channel disabled",
	)
)

// IsChannelWriteRefused reports whether err refuses a channel write for its
// content (a client error) rather than failing to store it.
func IsChannelWriteRefused(err error) bool {
	return errors.Is(err, ErrChannelModelsEmpty) || errors.Is(err, ErrChannelKeyEmpty)
}

func channelKeyEmpty(key string) bool {
	return strings.TrimSpace(key) == ""
}

// Configs keys the application owns for routing. Writing them never changes
// which upstream account, host or TLS trust a call uses, so they are not part
// of a channel's identity. Every other configs key is: adaptors read configs
// for upstream identity (Vertex derives its host from the region, for
// example). The rule is generic, with no per-provider list.
const (
	// ChannelConfigProviderBindings is the application's per-model provider
	// binding map (controller.providerBindingsConfig).
	ChannelConfigProviderBindings = "x_token_platform_provider_bindings"
	// ChannelConfigOutboundPolicy is the outbound address policy
	// (relay/utils.OutboundPolicyFromConfigs); the application always writes
	// public_only.
	ChannelConfigOutboundPolicy = "outbound_policy"
)

func routingOnlyChannelConfig(key string) bool {
	return key == ChannelConfigProviderBindings || key == ChannelConfigOutboundPolicy
}

// validate refuses patch content no channel may receive. Omitted (or null)
// models stay unchanged; only an explicit empty list is refused.
func (p *ChannelPatch) validate() error {
	if p.Models != nil && len(*p.Models) == 0 {
		return ErrChannelModelsEmpty
	}

	return nil
}

// touchesCredentials reports whether the patch writes a field a provider call
// depends on: key, type, base URL, proxy, TLS verification or configs.
func (p *ChannelPatch) touchesCredentials() bool {
	return p.Key != nil || p.Type != nil || p.BaseURL != nil || p.ProxyURL != nil ||
		p.SkipTLSVerify != nil || p.Configs != nil
}

// changesCredentials reports whether the patch changes one of those fields of
// the stored channel (for configs, any key but the routing-only ones).
// Accepted image and native tasks are polled and settled with the channel's
// current identity, so such a change needs the retention check.
func (p *ChannelPatch) changesCredentials(stored *Channel) bool {
	return (p.Key != nil && *p.Key != stored.Key) ||
		(p.Type != nil && *p.Type != stored.Type) ||
		(p.BaseURL != nil && *p.BaseURL != stored.BaseURL) ||
		(p.ProxyURL != nil && *p.ProxyURL != stored.ProxyURL) ||
		(p.SkipTLSVerify != nil && *p.SkipTLSVerify != stored.SkipTLSVerify) ||
		(p.Configs != nil && !sameIdentityConfigs(*p.Configs, stored.Configs))
}

// emptiesKeyOfEnabled reports whether the patch replaces the key of an
// enabled channel with an empty one. Rewriting a key that is already empty
// changes nothing and is not refused.
func (p *ChannelPatch) emptiesKeyOfEnabled(stored *Channel) bool {
	return p.Key != nil && *p.Key != stored.Key && channelKeyEmpty(*p.Key) &&
		stored.Status == ChannelStatusEnabled
}

// sameIdentityConfigs compares every configs key except the routing-only ones;
// a missing key equals an explicit null. Values compare as canonical JSON
// (sorted keys), so a stored value decoded by another JSON library still
// equals the same value in a request. An unencodable value counts as changed.
func sameIdentityConfigs(next, stored ChannelConfigs) bool {
	for _, configs := range []ChannelConfigs{next, stored} {
		for key := range configs {
			if !routingOnlyChannelConfig(key) && !sameConfigValue(next[key], stored[key]) {
				return false
			}
		}
	}

	return true
}

func sameConfigValue(a, b any) bool {
	encodedA, errA := json.Marshal(a)
	encodedB, errB := json.Marshal(b)

	return errA == nil && errB == nil && bytes.Equal(encodedA, encodedB)
}
