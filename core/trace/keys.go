package trace

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
)

var serviceKeyID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ParseTrustedKeys parses an optional server-only key ring. Never log raw input.
func ParseTrustedKeys(raw string) (map[string][]byte, error) {
	if raw == "" {
		return nil, nil
	}
	invalid := errors.New("invalid request trace service key configuration")
	if len(raw) > 8192 {
		return nil, invalid
	}
	var encoded map[string]string
	if json.Unmarshal([]byte(raw), &encoded) != nil || len(encoded) == 0 || len(encoded) > 8 {
		return nil, invalid
	}
	keys := make(map[string][]byte, len(encoded))
	for id, value := range encoded {
		key, err := base64.StdEncoding.Strict().DecodeString(value)
		if !serviceKeyID.MatchString(id) || err != nil || len(key) < 32 || len(key) > 64 {
			return nil, invalid
		}
		keys[id] = key
	}
	return keys, nil
}
