package registryvalidation

import (
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ProviderBinding is owned by channel configuration, never by the request body.
type ProviderBinding struct {
	Provider     string `json:"provider"`
	ID           string `json:"id"`
	Revision     string `json:"revision"`
	ContractHash string `json:"contractHash"`
}
type ProviderSpec struct {
	Metering     json.RawMessage `json:"metering,omitempty"`
	ID           string          `json:"id"`
	Revision     string          `json:"revision"`
	ContractHash string          `json:"contractHash"`
	Endpoint     string          `json:"endpoint"`
	Execution    struct {
		Mode                 string `json:"mode"`
		StatusEndpoint       string `json:"statusEndpoint"`
		ResultEndpoint       string `json:"resultEndpoint"`
		SupportsCancellation bool   `json:"supportsCancellation"`
	} `json:"execution"`
	Accepted map[string]any `json:"acceptedInputJsonSchema"`
	Input    map[string]any `json:"inputJsonSchema"`
	Output   map[string]any `json:"outputJsonSchema"`
}
type boundProvider struct {
	Adapter         string            `json:"adapter"`
	InputProjection string            `json:"inputProjection"`
	Mapping         map[string]string `json:"parameterMapping"`
	Fixed           map[string]any    `json:"fixedParameters"`
	Passthrough     []string          `json:"allowedPassthroughParameters"`
	Upstream        *ProviderSpec     `json:"upstream"`
}

var (
	ErrProviderContract = errors.New("provider contract is unavailable or incompatible")
	providerSegment     = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// fal model revisions may contain an internal dot (for example v4.5).
// A segment can never be ".", "..", or contain path/query delimiters.
var (
	providerPathSegment = regexp.MustCompile(`^[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*$`)
	arkModelID          = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)
	providerHash        = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// HasProviderContracts includes invalid/unknown versions so they cannot fall back to legacy execution.
func HasProviderContracts(raw []byte) bool {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return false
	}

	_, exists := obj["provider_contract_version"]

	return exists
}

func bound(raw []byte, b ProviderBinding) (*boundProvider, error) {
	var c struct {
		Version   int                      `json:"provider_contract_version"`
		Providers map[string]boundProvider `json:"providers"`
	}
	if len(raw) > 256*1024 || json.Unmarshal(raw, &c) != nil || c.Version != 1 ||
		b.Provider == "" ||
		b.ID == "" ||
		b.Revision == "" ||
		!providerHash.MatchString(b.ContractHash) {
		return nil, ErrProviderContract
	}

	p, ok := c.Providers[b.Provider]

	s := p.Upstream
	if !ok || s == nil || s.ID != b.ID || s.Revision != b.Revision ||
		s.ContractHash != b.ContractHash ||
		s.Endpoint == "" {
		return nil, ErrProviderContract
	}

	return &p, nil
}

func compileProviderSchema(schema map[string]any) (*jsonschema.Schema, error) {
	if schema == nil {
		return nil, ErrProviderContract
	}

	c := jsonschema.NewCompiler()
	c.UseLoader(denyLoader{})
	c.AssertFormat()

	const uri = "https://registry.invalid/provider.json"
	if c.AddResource(uri, schema) != nil {
		return nil, ErrProviderContract
	}

	s, err := c.Compile(uri)
	if err != nil {
		return nil, ErrProviderContract
	}

	return s, nil
}

func schemaAccepts(schema map[string]any, value any) error {
	s, err := compileProviderSchema(schema)
	if err != nil || s.Validate(value) != nil {
		return ErrProviderContract
	}

	return nil
}

// MapBoundProviderInput never applies provider defaults, drops parameters or changes canonical values.
// Supports fal queue and the explicitly configured Ark synchronous task bridge.
//
//nolint:gocyclo // Provider compatibility and mapping checks stay together as an auditable boundary.
func MapBoundProviderInput(
	raw []byte,
	b ProviderBinding,
	adapter, endpoint, execution string,
	body []byte,
) ([]byte, error) {
	p, err := bound(raw, b)
	if err != nil {
		return nil, err
	}

	s := p.Upstream
	if _, err := compileProviderSchema(s.Output); err != nil {
		return nil, err
	}

	if p.Adapter != adapter || s.Endpoint != endpoint || s.Execution.Mode != execution {
		return nil, ErrProviderContract
	}

	switch adapter {
	case "fal-image":
		if execution != "async" {
			return nil, ErrProviderContract
		}

		if !validFalQueueSpec(s) {
			return nil, ErrProviderContract
		}
	case "volcengine-ark-image":
		if execution != "sync" || len(endpoint) > 256 || !arkModelID.MatchString(endpoint) {
			return nil, ErrProviderContract
		}
	default:
		return nil, ErrProviderContract
	}

	var input map[string]any
	if json.Unmarshal(body, &input) != nil || input == nil {
		return nil, ErrProviderContract
	}

	delete(input, "model")

	if schemaAccepts(s.Accepted, input) != nil {
		return nil, ErrProviderContract
	}
	if p.InputProjection != "" {
		if p.InputProjection != "seedream45_ark_single_v1" || adapter != "volcengine-ark-image" ||
			len(p.Mapping) != 2 || p.Mapping["prompt"] != "prompt" || p.Mapping["image_size"] != "size" ||
			len(p.Fixed) != 2 || p.Fixed["stream"] != false || p.Fixed["response_format"] != "url" ||
			len(p.Passthrough) != 0 || len(input) != 4 || input["n"] != float64(1) || input["max_images"] != float64(1) {
			return nil, ErrProviderContract
		}
		size, ok := input["image_size"].(string)
		if !ok || (size != "auto_2K" && size != "auto_4K") {
			return nil, ErrProviderContract
		}
		prompt, ok := input["prompt"].(string)
		if !ok || prompt == "" {
			return nil, ErrProviderContract
		}
		mapped := map[string]any{
			"prompt": prompt, "size": strings.TrimPrefix(size, "auto_"),
			"stream": false, "response_format": "url",
		}
		if schemaAccepts(s.Input, mapped) != nil {
			return nil, ErrProviderContract
		}
		return json.Marshal(mapped)
	}

	mapped := map[string]any{}
	for key, value := range input {
		target, ok := p.Mapping[key]
		if !ok {
			// Fixed-count contracts keep n as a platform control, not an upstream field.
			var metering ImageMetering
			if key == "n" && json.Unmarshal(s.Metering, &metering) == nil &&
				(metering.Version == 3 || metering.Version == 4 || metering.Version == 5) && metering.OutputCountFixed != nil &&
				*metering.OutputCountFixed == 1 {
				if value != float64(1) {
					return nil, ErrProviderContract
				}
				continue
			}
			if !slices.Contains(p.Passthrough, key) {
				return nil, ErrProviderContract
			}

			target = key
		}

		if target == "" || target == "model" || strings.Contains(target, ".") {
			return nil, ErrProviderContract
		}

		if _, exists := mapped[target]; exists {
			return nil, ErrProviderContract
		}

		mapped[target] = value
	}

	for key, value := range p.Fixed {
		if key == "model" {
			return nil, ErrProviderContract
		}

		if supplied, ok := mapped[key]; ok && !reflect.DeepEqual(supplied, value) {
			return nil, ErrProviderContract
		}

		mapped[key] = value
	}

	if adapter == "volcengine-ark-image" &&
		(mapped["stream"] != false || mapped["response_format"] != "url") {
		return nil, ErrProviderContract
	}

	if schemaAccepts(s.Input, mapped) != nil {
		return nil, ErrProviderContract
	}

	return json.Marshal(mapped)
}

func FreezeProviderBinding(raw []byte, b ProviderBinding) ([]byte, error) {
	if _, err := bound(raw, b); err != nil {
		return nil, err
	}

	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil, ErrProviderContract
	}

	binding, err := json.Marshal(b)
	if err != nil {
		return nil, ErrProviderContract
	}

	obj["selected_provider_binding"] = binding

	frozen, err := json.Marshal(obj)
	if err != nil || len(frozen) > 256*1024 {
		return nil, ErrProviderContract
	}

	return frozen, nil
}

func ValidateFrozenProviderOutput(raw, output []byte) error {
	if !HasProviderContracts(raw) {
		return nil
	}

	var frozen struct {
		Binding ProviderBinding `json:"selected_provider_binding"`
	}
	if json.Unmarshal(raw, &frozen) != nil {
		return ErrProviderContract
	}

	p, err := bound(raw, frozen.Binding)
	if err != nil {
		return err
	}

	var value any
	if json.Unmarshal(output, &value) != nil {
		return ErrProviderContract
	}
	var metering ImageMetering
	if len(p.Upstream.Metering) != 0 && json.Unmarshal(p.Upstream.Metering, &metering) != nil {
		return ErrProviderContract
	}
	if metering.Version == 4 || (metering.Version == 5 && metering.MaxOutputPixels != nil) {
		if metering.MaxOutputPixels == nil || *metering.MaxOutputPixels < 1 {
			return ErrProviderContract
		}
		var images struct {
			Images []struct {
				Width  int64 `json:"width"`
				Height int64 `json:"height"`
			} `json:"images"`
		}
		if json.Unmarshal(output, &images) != nil || len(images.Images) == 0 {
			return ErrProviderContract
		}
		for _, image := range images.Images {
			if image.Width < 1 || image.Height < 1 || image.Width > *metering.MaxOutputPixels/image.Height {
				return ErrProviderContract
			}
		}
	}

	return schemaAccepts(p.Upstream.Output, value)
}

// FrozenImagePixelLimit is read from the reservation snapshot during failover.
func FrozenImagePixelLimit(raw []byte) (int64, error) {
	if !HasProviderContracts(raw) {
		return 0, nil
	}
	var frozen struct {
		Binding ProviderBinding `json:"selected_provider_binding"`
	}
	if json.Unmarshal(raw, &frozen) != nil {
		return 0, ErrProviderContract
	}
	p, err := bound(raw, frozen.Binding)
	if err != nil {
		return 0, err
	}
	var m ImageMetering
	if len(p.Upstream.Metering) == 0 {
		return 0, nil
	}
	if json.Unmarshal(p.Upstream.Metering, &m) != nil {
		return 0, ErrProviderContract
	}
	if m.Version == 4 || (m.Version == 5 && m.MaxOutputPixels != nil) {
		if m.MaxOutputPixels == nil || *m.MaxOutputPixels < 1 || *m.MaxOutputPixels > 100000000 {
			return 0, ErrProviderContract
		}
		return *m.MaxOutputPixels, nil
	}
	return 0, nil
}

// Only paths within the pinned model (or the legacy application root) are allowed.
// Cancellation is an upstream capability, not permission to expose a public cancel API.
func validFalQueueSpec(s *ProviderSpec) bool {
	parts := strings.Split(s.Endpoint, "/")
	if len(parts) < 2 || s.Execution.Mode != "async" {
		return false
	}

	for _, part := range parts {
		if !providerPathSegment.MatchString(part) {
			return false
		}
	}

	legacy := strings.Join(parts[:2], "/") + "/requests/{request_id}"
	full := s.Endpoint + "/requests/{request_id}"
	root := s.Execution.ResultEndpoint

	return (root == legacy || root == full) && s.Execution.StatusEndpoint == root+"/status"
}

// FrozenFalQueuePaths returns validated relative paths from the task snapshot.
// It never trusts response URLs, mutable current configuration, or an arbitrary host.
func FrozenFalQueuePaths(raw []byte, endpoint, requestID string) (string, string, error) {
	var frozen struct {
		Binding ProviderBinding `json:"selected_provider_binding"`
	}
	if json.Unmarshal(raw, &frozen) != nil || !providerSegment.MatchString(requestID) {
		return "", "", ErrProviderContract
	}

	p, err := bound(raw, frozen.Binding)
	if err != nil {
		return "", "", err
	}

	s := p.Upstream
	if p.Adapter != "fal-image" || s.Endpoint != endpoint || !validFalQueueSpec(s) {
		return "", "", ErrProviderContract
	}

	return strings.ReplaceAll(
			s.Execution.StatusEndpoint,
			"{request_id}",
			requestID,
		), strings.ReplaceAll(
			s.Execution.ResultEndpoint,
			"{request_id}",
			requestID,
		), nil
}
