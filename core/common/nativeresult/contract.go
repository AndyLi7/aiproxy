package nativeresult

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

var ErrRequest = errors.New("native request does not match the selected model")

// TaskContract describes the public envelope independently from provider routing.
// The registry must freeze and hash it together with its private provider binding.
type TaskContract struct {
	UpstreamInputSchema json.RawMessage            `json:"upstream_input_schema,omitempty"`
	FixedParameters     map[string]json.RawMessage `json:"fixed_parameters,omitempty"`
	Artifacts           []ArtifactBinding          `json:"artifacts,omitempty"`
	Version             int                        `json:"version"`
	Model               string                     `json:"model"`
	InputSchema         json.RawMessage            `json:"input_schema"`
	OutputSchema        json.RawMessage            `json:"output_schema"`
}
type CompiledTask struct {
	fixed    map[string]json.RawMessage
	upstream *Validator
	model    string
	input    *Validator
	output   *Validator
}

func CompileTaskContract(raw []byte) (*CompiledTask, error) {
	if _, err := decode(raw); err != nil {
		return nil, ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	var c TaskContract
	if d.Decode(&c) != nil || c.Version != 1 || c.Model == "" || len(c.Model) > 256 {
		return nil, ErrInvalid
	}
	if len(c.Artifacts) > 64 {
		return nil, ErrInvalid
	}
	for _, binding := range c.Artifacts {
		if len(binding.Path) == 0 || len(binding.Path) > MaxDepth {
			return nil, ErrInvalid
		}
	}
	input, err := Compile(c.InputSchema)
	if err != nil {
		return nil, err
	}
	output, err := Compile(c.OutputSchema)
	if err != nil {
		return nil, err
	}
	var upstream *Validator
	if len(c.FixedParameters) > 64 || (len(c.FixedParameters) > 0 && len(c.UpstreamInputSchema) == 0) {
		return nil, ErrInvalid
	}
	for key, raw := range c.FixedParameters {
		if key == "" || len(key) > 128 || key == "__proto__" || key == "constructor" || key == "prototype" {
			return nil, ErrInvalid
		}
		value, err := decode(raw)
		if err != nil {
			return nil, ErrInvalid
		}
		switch value.(type) {
		case map[string]any, []any:
			return nil, ErrInvalid
		}
	}
	if len(c.UpstreamInputSchema) > 0 {
		upstream, err = Compile(c.UpstreamInputSchema)
		if err != nil {
			return nil, err
		}
	}
	return &CompiledTask{model: c.Model, input: input, output: output, upstream: upstream, fixed: c.FixedParameters}, nil
}

// ValidateRequest accepts only the public model plus the native input object.
// There is no client-supplied provider/credential/URL override. The returned input
// is unchanged; omission, defaults, nested fields and exact numbers remain native.
func (c *CompiledTask) ValidateRequest(raw []byte) (json.RawMessage, error) {
	return c.ValidateRequestAs(raw, "")
}

// ValidateRequestAs is ValidateRequest that also accepts accepted as the
// request's model: another ID the gateway resolved to this contract's model
// (its public_api_id or an alias). The contract and the body are unchanged.
func (c *CompiledTask) ValidateRequestAs(raw []byte, accepted string) (json.RawMessage, error) {
	if c == nil {
		return nil, ErrRequest
	}
	if _, err := decode(raw); err != nil {
		return nil, ErrRequest
	}
	var request struct {
		Model string          `json:"model"`
		Input json.RawMessage `json:"input"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&request) != nil || (request.Model != c.model && (accepted == "" || request.Model != accepted)) || len(request.Input) == 0 {
		return nil, ErrRequest
	}
	if len(c.fixed) > 0 {
		var fields map[string]json.RawMessage
		if json.Unmarshal(request.Input, &fields) != nil || fields == nil {
			return nil, ErrRequest
		}
		for key := range c.fixed {
			if _, exists := fields[key]; exists {
				return nil, ErrRequest
			}
		}
	}
	result, err := c.input.Validate(request.Input)
	if err != nil {
		return nil, ErrRequest
	}
	return result, nil
}
func (c *CompiledTask) ValidateOutput(raw []byte) (json.RawMessage, error) {
	if c == nil {
		return nil, ErrInvalid
	}
	return c.output.Validate(raw)
}

// PrepareUpstreamInput inserts only registry-frozen platform controls. It never
// inserts model defaults or coerces native parameters. ValidateRequest must run
// first; revalidating here also makes the method safe for independent callers.
func (c *CompiledTask) PrepareUpstreamInput(raw []byte) (json.RawMessage, error) {
	if c == nil {
		return nil, ErrRequest
	}
	if _, err := c.input.Validate(raw); err != nil {
		return nil, ErrRequest
	}
	result := append(json.RawMessage(nil), raw...)
	if len(c.fixed) > 0 {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			return nil, ErrRequest
		}
		for key, value := range c.fixed {
			if _, exists := fields[key]; exists {
				return nil, ErrRequest
			}
			fields[key] = value
		}
		var err error
		result, err = json.Marshal(fields)
		if err != nil {
			return nil, ErrRequest
		}
	}
	if c.upstream != nil {
		validated, err := c.upstream.Validate(result)
		if err != nil {
			return nil, ErrRequest
		}
		return validated, nil
	}
	return result, nil
}

// CustomerIssues drops the issues that name a registry-frozen platform control:
// the customer cannot send or change one (ValidateRequest refuses it) and its
// name is not public. Nil when none remains; such a rejection is the
// platform's, not the customer's input error.
func (c *CompiledTask) CustomerIssues(issues []ParameterIssue) []ParameterIssue {
	var kept []ParameterIssue
	for _, issue := range issues {
		name := issue.Field
		if end := strings.IndexAny(name, ".["); end >= 0 {
			name = name[:end]
		}
		if c != nil {
			if _, fixed := c.fixed[name]; fixed {
				continue
			}
		}
		kept = append(kept, issue)
	}
	return kept
}
