// Package nativeresult validates frozen native outputs without coercing them into images.
package nativeresult

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const MaxBytes = 4 << 20
const MaxDepth = 64
const MaxNodes = 100000

var ErrInvalid = errors.New("native result does not match its frozen contract")

type denyLoader struct{}

func (denyLoader) Load(string) (any, error) {
	return nil, errors.New("external schema references disabled")
}

// Validator is created from a server-owned frozen schema, never a request parameter.
// Original JSON bytes are preserved after validation to avoid rounding integers.
type Validator struct{ schema *jsonschema.Schema }

func Compile(rawSchema []byte) (*Validator, error) {
	value, err := decode(rawSchema)
	if err != nil {
		return nil, ErrInvalid
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(denyLoader{})
	const resource = "https://registry.invalid/native-output.json"
	if err := compiler.AddResource(resource, value); err != nil {
		return nil, ErrInvalid
	}
	schema, err := compiler.Compile(resource)
	if err != nil {
		return nil, ErrInvalid
	}
	return &Validator{schema: schema}, nil
}

// Validate returns a detached byte copy. Null and empty collections are valid if
// allowed by the source schema; neither is used as the task completion marker.
// It does not fetch URLs, execute markup, apply defaults or strip unknown fields.
func (v *Validator) Validate(raw []byte) (json.RawMessage, error) {
	if v == nil || v.schema == nil {
		return nil, ErrInvalid
	}
	value, err := decode(raw)
	if err != nil || v.schema.Validate(value) != nil {
		return nil, ErrInvalid
	}
	return append(json.RawMessage(nil), raw...), nil
}

func decode(raw []byte) (any, error) {
	if len(raw) == 0 || len(raw) > MaxBytes || !utf8.Valid(raw) {
		return nil, ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	nodes := 0
	value, err := read(d, 0, &nodes)
	if err != nil {
		return nil, ErrInvalid
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, ErrInvalid
	}
	return value, nil
}
func read(d *json.Decoder, depth int, nodes *int) (any, error) {
	*nodes++
	if depth > MaxDepth || *nodes > MaxNodes {
		return nil, ErrInvalid
	}
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delimiter {
	case '{':
		result := map[string]any{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			name, ok := key.(string)
			if !ok {
				return nil, ErrInvalid
			}
			if _, exists := result[name]; exists {
				return nil, ErrInvalid
			}
			value, err := read(d, depth+1, nodes)
			if err != nil {
				return nil, err
			}
			result[name] = value
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return nil, ErrInvalid
		}
		return result, nil
	case '[':
		result := []any{}
		for d.More() {
			value, err := read(d, depth+1, nodes)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return nil, ErrInvalid
		}
		return result, nil
	default:
		return nil, ErrInvalid
	}
}
