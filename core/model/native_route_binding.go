package model

import "encoding/json"

const NativeResultConfigKey ModelConfigKey = "x_token_platform_native_result_v1"

type NativeRouteBinding struct {
	Version         int             `json:"version"`
	Contract        json.RawMessage `json:"contract"`
	ChannelID       int             `json:"channelId"`
	Endpoint        string          `json:"endpoint"`
	CredentialScope string          `json:"credentialScope"`
	KeyFingerprint  string          `json:"keyFingerprint"`
	DeliveryBase    string          `json:"deliveryBase"`
}

func ParseNativeRouteBinding(mc ModelConfig) (NativeRouteBinding, bool) {
	raw := mc.Config[NativeResultConfigKey]
	if raw == nil {
		return NativeRouteBinding{}, false
	}

	encoded, err := json.Marshal(raw)
	if err != nil {
		return NativeRouteBinding{}, false
	}

	var binding NativeRouteBinding
	if json.Unmarshal(encoded, &binding) != nil || binding.Version != 1 || binding.ChannelID <= 0 {
		return NativeRouteBinding{}, false
	}

	return binding, true
}
