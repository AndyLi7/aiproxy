package nativetask

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// The public model id the application publishes for the Eleven v4 Turbo
// endpoint (contract-turbo.json).
const (
	turboModel    = "vendor/eleven-v4-turbo/text-to-speech"
	turboEndpoint = "elevenlabs/tts/eleven-v4-turbo"
)

var (
	longEnum  = strings.Repeat("e", 70)
	longConst = strings.Repeat("c", 70)
)

// turboContract follows the Eleven v4 Turbo input schema. textSchema and
// required vary per case; additionalProperties is open as in the provider schema.
func turboContract(textSchema, required string) json.RawMessage {
	return json.RawMessage(`{"version":1,"model":"` + turboModel + `","input_schema":{"type":"object","required":[` + required + `],"properties":{` +
		`"text":` + textSchema + `,"voice":{"type":"string"},"stability":{"type":"number","minimum":0,"maximum":1},` +
		`"language_code":{"anyOf":[{"type":"string"},{"type":"null"}]},"apply_text_normalization":{"type":"string","enum":["auto","on","off"]},` +
		`"preset":{"type":"string","enum":["` + longEnum + `"]},"tag":{"const":"` + longConst + `"},"timestamps":{"type":"boolean"}}},"output_schema":{"type":"object"}}`)
}

var defaultTurboContract = turboContract(`{"type":"string","maxLength":5000}`, `"text"`)

func fixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(raw)
}

func meteredSetup(t *testing.T) (*Engine, Plan, *walletStub, *providerStub) {
	e, _, w, p := setup(t)
	e.InputMeter = true
	// The contract the application publishes for Eleven v4 Turbo: imported
	// request schemas are a local $ref into the contract's own components.
	plan := Plan{KeyFingerprint: model.ImageChannelKeyFingerprint("test-native-key"), Contract: json.RawMessage(fixture(t, "contract-turbo.json")), ChannelID: 48, Endpoint: turboEndpoint,
		CredentialScope: "f1da35c5-782c-4341-9e09-a705b70b6b6b", QuoteJSON: fixture(t, "quote-turbo-max.json"), InputMeter: json.RawMessage(fixture(t, "input-meter-turbo.json"))}
	return e, plan, w, p
}

func turboBody(input string) []byte {
	return []byte(`{"model":"` + turboModel + `","input":` + input + `}`)
}

func textInput(text string) string {
	encoded, _ := json.Marshal(map[string]string{"text": text})
	return string(encoded)
}

func admission(t *testing.T, commands []balance.PrepaymentCommand) balance.PrepaymentCommand {
	t.Helper()
	require.NotEmpty(t, commands)
	require.Equal(t, "admit", commands[0].Action)
	require.NotNil(t, commands[0].PrepaidMicros)
	require.NotNil(t, commands[0].EstimatedMicros)
	return commands[0]
}

// Cross-repository golden case: today's production request (122 characters on
// Eleven v4 Turbo, $0.04 per 1,000 characters) holds 4,880 µ$ instead of
// 200,000. The application writes contract-turbo.json, quote-turbo-max.json
// and input-meter-turbo.json byte for byte, copies
// metered-quote-turbo-122.json and parses it with the wallet's admission schema.
func TestInputMeterGoldenTurbo122(t *testing.T) {
	e, plan, w, p := meteredSetup(t)
	input := fixture(t, "metered-input-turbo-122.json")
	task, err := e.Submit(context.Background(), "turbo-122", "g", 1, turboBody(input), plan)
	require.NoError(t, err)
	require.Equal(t, "queued", task.Status)
	admit := admission(t, w.commands)
	golden := fixture(t, "metered-quote-turbo-122.json")
	require.Equal(t, golden, admit.QuoteJSON, "the rewritten quote must match the golden file byte for byte")
	require.EqualValues(t, 4880, *admit.PrepaidMicros)
	require.EqualValues(t, 4880, *admit.EstimatedMicros)
	saved, err := model.GetNativeTask(e.DB, "turbo-122", "g", 1)
	require.NoError(t, err)
	require.Equal(t, golden, saved.PrepaymentQuoteJSON)
	require.Equal(t, input, string(p.body), "the upstream body is never changed")
	quote, err := model.ParseImagePrepaymentQuote(golden)
	require.NoError(t, err)
	require.Equal(t, "actual-cost-v1", quote.SettlementPolicy)
	require.Equal(t, "request", quote.Routes[0].QuantityMetric)
}

func TestMeteredCharactersAreCodePointsOfTheUpstreamString(t *testing.T) {
	var frozen nativeresult.TaskContract
	require.NoError(t, json.Unmarshal(defaultTurboContract, &frozen))
	for input, want := range map[string]int64{
		fixture(t, "metered-input-turbo-122.json"): 122,
		`{"text":"\ud83d\ude00"}`:                  1, // one escaped astral character
		`{"text":"😀"}`:                             1,
		`{"text":"👋🏽"}`:                            2, // emoji plus skin-tone modifier
		`{"text":"cafe\u0301"}`:                    5, // combining mark counts on its own
		`{"text":"a\r\nb"}`:                        4,
		`{"text":"[excited] 你好"}`:                  12,
		`{"text":""}`:                              0,
		`{"text":"hi","voice":"` + strings.Repeat("v", 64) + `"}`:                                             2,
		`{"text":"hi","preset":"` + longEnum + `","tag":"` + longConst + `","apply_text_normalization":"on"}`: 2,
	} {
		count, err := meteredCharacters(frozen, json.RawMessage(input), "text")
		require.NoError(t, err, input)
		require.Equal(t, want, count, input)
	}
	for input, want := range map[string]error{
		`{"voice":"Aria"}`:                        errMeterFieldMissing,
		`{"text":12}`:                             errMeterFieldNotText,
		`{"text":null}`:                           errMeterFieldNotText,
		`{"text":["a"]}`:                          errMeterFieldNotText,
		`{"text":{"value":"a"}}`:                  errMeterFieldNotText,
		`{"text":"hi","previous_text":"earlier"}`: errMeterUndeclaredKey,
		`{"text":"hi","voice":"` + strings.Repeat("v", 65) + `"}`: errMeterOtherText,
		`["text"]`: errMeterInput,
	} {
		_, err := meteredCharacters(frozen, json.RawMessage(input), "text")
		require.ErrorIs(t, err, want, input)
	}
	frozen.InputSchema = json.RawMessage(`{"type":"object"}`)
	_, err := meteredCharacters(frozen, json.RawMessage(`{"text":"hi"}`), "text")
	require.ErrorIs(t, err, errMeterSchema)
}

// Imported contracts reference their request schema (and property schemas)
// inside their own components. The meter follows local references and
// single-branch allOf wrappers like the schema validator; anything it cannot
// resolve holds the published maximum.
func TestMeteredCharactersFollowLocalSchemaReferences(t *testing.T) {
	var published nativeresult.TaskContract
	require.NoError(t, json.Unmarshal([]byte(fixture(t, "contract-turbo.json")), &published))
	require.Contains(t, string(published.InputSchema), `"$ref":"#/components/schemas/TtsElevenV4TurboInput"`)
	count, err := meteredCharacters(published, json.RawMessage(fixture(t, "metered-input-turbo-122.json")), "text")
	require.NoError(t, err)
	require.EqualValues(t, 122, count)
	count, err = meteredCharacters(published, json.RawMessage(`{"text":"hi","voice":"Rachel","stability":0.5,"output_format":"pcm_16000","timestamps":true}`), "text")
	require.NoError(t, err)
	require.EqualValues(t, 2, count)
	_, err = meteredCharacters(published, json.RawMessage(`{"text":"hi","previous_text":"earlier"}`), "text")
	require.ErrorIs(t, err, errMeterUndeclaredKey)
	_, err = meteredCharacters(published, json.RawMessage(`{"text":"hi","voice":"`+strings.Repeat("v", 65)+`"}`), "text")
	require.ErrorIs(t, err, errMeterOtherText)

	components := `"components":{"schemas":{"Input":{"type":"object","properties":{"text":{"type":"string"},"preset":{"$ref":"#/components/schemas/Preset"},` +
		`"style":{"allOf":[{"$ref":"#/components/schemas/Preset"}],"description":"wrapped"},"note":{"$ref":"#/components/schemas/Note"}}},` +
		`"Preset":{"type":"string","enum":["` + longEnum + `"]},"Note":{"type":"string"},"Loop":{"$ref":"#/components/schemas/Loop"},"a/b":{"type":"object","properties":{"text":{"type":"string"}}}}}`
	contract := func(inputSchema string) nativeresult.TaskContract {
		return nativeresult.TaskContract{Version: 1, Model: turboModel, InputSchema: json.RawMessage(inputSchema)}
	}
	referenced := contract(`{"$ref":"#/components/schemas/Input","title":"Input",` + components + `}`)
	count, err = meteredCharacters(referenced, json.RawMessage(`{"text":"hello","preset":"`+longEnum+`","style":"`+longEnum+`"}`), "text")
	require.NoError(t, err, "a referenced enum fixes its value")
	require.EqualValues(t, 5, count)
	_, err = meteredCharacters(referenced, json.RawMessage(`{"text":"hello","note":"`+strings.Repeat("n", 65)+`"}`), "text")
	require.ErrorIs(t, err, errMeterOtherText, "a referenced free string is other text")
	wrapped := contract(`{"allOf":[{"$ref":"#/components/schemas/Input"}],` + components + `}`)
	count, err = meteredCharacters(wrapped, json.RawMessage(`{"text":"hello"}`), "text")
	require.NoError(t, err)
	require.EqualValues(t, 5, count)
	escaped := contract(`{"$ref":"#/components/schemas/a~1b",` + components + `}`)
	count, err = meteredCharacters(escaped, json.RawMessage(`{"text":"hello"}`), "text")
	require.NoError(t, err)
	require.EqualValues(t, 5, count)
	for name, schema := range map[string]string{
		"remote reference":       `{"$ref":"https://example.com/input.json"}`,
		"missing target":         `{"$ref":"#/components/schemas/Missing",` + components + `}`,
		"reference cycle":        `{"$ref":"#/components/schemas/Loop",` + components + `}`,
		"constraining sibling":   `{"$ref":"#/components/schemas/Input","properties":{"extra":{}},` + components + `}`,
		"several allOf branches": `{"allOf":[{"$ref":"#/components/schemas/Input"},{"type":"object"}],` + components + `}`,
		"reference into array":   `{"$ref":"#/components/list/0","components":{"list":[{"type":"object","properties":{"text":{"type":"string"}}}]}}`,
	} {
		_, err := meteredCharacters(contract(schema), json.RawMessage(`{"text":"hello"}`), "text")
		require.ErrorIs(t, err, errMeterSchema, name)
	}
}

func TestInputMeterHoldsThePriceOfTheText(t *testing.T) {
	for name, tc := range map[string]struct {
		input string
		hold  int64
	}{
		"one character":    {textInput("a"), 40},
		"empty":            {textInput(""), 40},
		"longest text":     {textInput(strings.Repeat("a", 5000)), 200000},
		"escaped emoji":    {`{"text":"\ud83d\ude00"}`, 40},
		"short other text": {`{"text":"` + strings.Repeat("a", 1000) + `","voice":"Aria","stability":0.5,"timestamps":true}`, 40000},
	} {
		t.Run(name, func(t *testing.T) {
			e, plan, w, _ := meteredSetup(t)
			plan.Contract = turboContract(`{"type":"string","maxLength":5000}`, ``)
			_, err := e.Submit(context.Background(), "req", "g", 1, turboBody(tc.input), plan)
			require.NoError(t, err)
			admit := admission(t, w.commands)
			require.Equal(t, tc.hold, *admit.PrepaidMicros)
			require.Equal(t, tc.hold, *admit.EstimatedMicros)
			quote, err := model.ParseImagePrepaymentQuote(admit.QuoteJSON)
			require.NoError(t, err)
			require.Equal(t, tc.hold, quote.PrepaidMicros)
		})
	}
}

// Platform controls are part of the body sent upstream; the upstream schema
// declares them, so they do not stop the meter.
func TestInputMeterAcceptsDeclaredPlatformControls(t *testing.T) {
	e, plan, w, p := meteredSetup(t)
	plan.Contract = json.RawMessage(`{"version":1,"model":"` + turboModel + `","input_schema":{"type":"object","required":["text"],"properties":{"text":{"type":"string","maxLength":5000}}},` +
		`"upstream_input_schema":{"type":"object","required":["text"],"properties":{"text":{"type":"string","maxLength":5000},"output_format":{"type":"string","enum":["mp3_44100_128","pcm_16000"]}}},` +
		`"fixed_parameters":{"output_format":"mp3_44100_128"},"output_schema":{"type":"object"}}`)
	_, err := e.Submit(context.Background(), "req", "g", 1, turboBody(textInput("hello")), plan)
	require.NoError(t, err)
	require.EqualValues(t, 200, *admission(t, w.commands).PrepaidMicros)
	require.JSONEq(t, `{"text":"hello","output_format":"mp3_44100_128"}`, string(p.body))
}

// Whenever the meter cannot price the request, the published quote is
// forwarded byte for byte: the hold is today's maximum, never a refusal.
func TestInputMeterKeepsThePublishedMaximum(t *testing.T) {
	short := textInput("hello")
	for name, tc := range map[string]struct {
		input      string
		contract   json.RawMessage
		meter      string
		meterOff   bool
		unmetered  bool
		quoteNoAct bool
	}{
		"engine flag off":          {input: short, meterOff: true},
		"no meter":                 {input: short, unmetered: true},
		"invalid meter":            {input: short, meter: `{"version":2,"metric":"characters","path":["text"],"routes":{}}`},
		"meter not json":           {input: short, meter: `{"version":`},
		"cap differs from quote":   {input: short, meter: `{"version":1,"metric":"characters","path":["text"],"routes":{"b1c4b049-f513-4e59-83ae-ea191ee1704e":{"unitSize":1000,"unitMicros":40000,"maxQuantity":5000,"maxMicros":150000}}}`},
		"not actual-cost":          {input: short, quoteNoAct: true},
		"metered field missing":    {input: `{"voice":"Aria"}`, contract: turboContract(`{"type":"string"}`, ``)},
		"metered field not string": {input: `{"text":42}`, contract: turboContract(`{}`, `"text"`)},
		"over the metered maximum": {input: textInput(strings.Repeat("a", 5001)), contract: turboContract(`{"type":"string"}`, `"text"`)},
		"undeclared key":           {input: `{"text":"hello","previous_text":"earlier"}`},
		"long other text":          {input: `{"text":"hello","voice":"` + strings.Repeat("v", 65) + `"}`},
	} {
		t.Run(name, func(t *testing.T) {
			e, plan, w, _ := meteredSetup(t)
			e.InputMeter = !tc.meterOff
			if tc.contract != nil {
				plan.Contract = tc.contract
			}
			if tc.meter != "" {
				plan.InputMeter = json.RawMessage(tc.meter)
			}
			if tc.unmetered {
				plan.InputMeter = nil
			}
			if tc.quoteNoAct {
				plan.QuoteJSON = strings.Replace(plan.QuoteJSON, `,"settlementPolicy":"actual-cost-v1"`, ``, 1)
			}
			published := plan.QuoteJSON
			task, err := e.Submit(context.Background(), "req", "g", 1, turboBody(tc.input), plan)
			require.NoError(t, err)
			require.Equal(t, "queued", task.Status)
			admit := admission(t, w.commands)
			require.Equal(t, published, admit.QuoteJSON)
			require.EqualValues(t, 200000, *admit.PrepaidMicros)
			require.EqualValues(t, 200000, *admit.EstimatedMicros)
			saved, err := model.GetNativeTask(e.DB, "req", "g", 1)
			require.NoError(t, err)
			require.Equal(t, published, saved.PrepaymentQuoteJSON)
		})
	}
}

// The same body and release always give the same quote bytes, so a retry of a
// reservation the wallet never confirmed is not a request ID conflict.
func TestInputMeterQuoteIsDeterministicAcrossRetries(t *testing.T) {
	e, plan, _, p := meteredSetup(t)
	body := turboBody(fixture(t, "metered-input-turbo-122.json"))
	ambiguous := &scriptedWallet{errs: map[string]error{"admit": errors.New("prepayment outcome unknown")}}
	e.Wallet = ambiguous
	_, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.Error(t, err)
	reserved, err := model.GetNativeTask(e.DB, "req", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "reserved", reserved.Status)
	require.Equal(t, fixture(t, "metered-quote-turbo-122.json"), reserved.PrepaymentQuoteJSON)

	retry := &walletStub{}
	e.Wallet = retry
	task, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.NoError(t, err, "a reserved retry must not conflict with its own quote")
	require.Equal(t, "queued", task.Status)
	require.Equal(t, ambiguous.commands[0].QuoteJSON, admission(t, retry.commands).QuoteJSON)
	require.EqualValues(t, 4880, *admission(t, retry.commands).PrepaidMicros)
	require.Equal(t, 1, p.calls)

	var frozen nativeresult.TaskContract
	require.NoError(t, json.Unmarshal(plan.Contract, &frozen))
	quote, err := model.ParseImagePrepaymentQuote(plan.QuoteJSON)
	require.NoError(t, err)
	first, _, err := sizeQuoteToInput(plan.QuoteJSON, quote, plan.InputMeter, frozen, json.RawMessage(fixture(t, "metered-input-turbo-122.json")))
	require.NoError(t, err)
	second, _, err := sizeQuoteToInput(plan.QuoteJSON, quote, plan.InputMeter, frozen, json.RawMessage(fixture(t, "metered-input-turbo-122.json")))
	require.NoError(t, err)
	require.Equal(t, first, second)
}

// A refused admission of a sized hold still leaves no reservation behind, and
// the same request may be retried once the customer can pay.
func TestInputMeterInsufficientBalanceReleasesReservation(t *testing.T) {
	e, plan, _, p := meteredSetup(t)
	body := turboBody(fixture(t, "metered-input-turbo-122.json"))
	refusing := &scriptedWallet{errs: map[string]error{"admit": balance.ErrPrepaymentInsufficientBalance}}
	e.Wallet = refusing
	_, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.ErrorIs(t, err, balance.ErrPrepaymentInsufficientBalance)
	require.EqualValues(t, 4880, *admission(t, refusing.commands).PrepaidMicros)
	_, err = model.GetNativeTask(e.DB, "req", "g", 1)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	require.Zero(t, p.calls)

	paying := &walletStub{}
	e.Wallet = paying
	task, err := e.Submit(context.Background(), "req", "g", 1, body, plan)
	require.NoError(t, err)
	require.Equal(t, "queued", task.Status)
	require.EqualValues(t, 4880, *admission(t, paying.commands).PrepaidMicros)
	require.Equal(t, 1, p.calls)
}

// Text inside another top-level list or object may be billed as well (a
// dialogue turn, a context block). Short nested settings still meter; nested
// text longer than otherTextLimit in total holds the published maximum.
func TestMeteredCharactersTreatLongNestedTextAsOtherText(t *testing.T) {
	frozen := nativeresult.TaskContract{InputSchema: json.RawMessage(`{"type":"object","properties":{` +
		`"text":{"type":"string"},"dialogue":{"type":"array"},"voice_setting":{"type":"object"}}}`)}
	count, err := meteredCharacters(frozen, json.RawMessage(
		`{"text":"hi","voice_setting":{"voice_id":"Wise_Woman","speed":1,"emotion":"happy"},"dialogue":[]}`), "text")
	require.NoError(t, err)
	require.Equal(t, int64(2), count)
	for _, input := range []string{
		`{"text":"hi","dialogue":[{"text":"` + strings.Repeat("话", 65) + `"}]}`,
		`{"text":"hi","dialogue":["` + strings.Repeat("a", 40) + `","` + strings.Repeat("b", 25) + `"]}`,
		`{"text":"hi","voice_setting":{"note":{"deep":"` + strings.Repeat("c", 65) + `"}}}`,
	} {
		_, err := meteredCharacters(frozen, json.RawMessage(input), "text")
		require.ErrorIs(t, err, errMeterOtherText, input)
	}
}
