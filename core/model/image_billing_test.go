//nolint:testpackage // These fixtures verify internal metering and persistence boundaries.
package model

import (
	"encoding/json"
	"os"
	"testing"
)

func TestImageBillingFixtures(t *testing.T) {
	b, e := os.ReadFile("testdata/media-billing-v1.json")
	if e != nil {
		t.Fatal(e)
	}

	var fs []struct {
		Name         string          `json:"name"`
		Policy       json.RawMessage `json:"policy"`
		Usage        json.RawMessage `json:"usage"`
		AmountMicros *int64          `json:"amountMicros"`
		State        string          `json:"state"`
		Invalid      bool            `json:"invalid"`
		DefaultRate  json.RawMessage `json:"defaultRate"`
	}
	if e = json.Unmarshal(b, &fs); e != nil {
		t.Fatal(e)
	}

	for _, f := range fs {
		t.Run(f.Name, func(t *testing.T) {
			var p ImageBillingPolicy

			err := json.Unmarshal(f.Policy, &p)

			var u *ImageUsage
			if err == nil {
				if ue := json.Unmarshal(f.Usage, &u); ue != nil {
					u = nil
				}
			}

			var rate ImageBillingRate
			if err == nil {
				err = json.Unmarshal(f.DefaultRate, &rate)
			}

			var r ImageBillingResult
			if err == nil {
				r, err = EvaluateImageBilling(&p, u, rate)
			}

			if f.Invalid {
				if err == nil {
					t.Fatal("expected rejection")
				}
				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if r.State != f.State {
				t.Fatalf("state %s want %s", r.State, f.State)
			}

			if (r.AmountMicros == nil) != (f.AmountMicros == nil) {
				t.Fatal("amount presence")
			}

			if r.AmountMicros != nil && *r.AmountMicros != *f.AmountMicros {
				t.Fatalf("amount %d want %d", *r.AmountMicros, *f.AmountMicros)
			}
		})
	}
}

func TestImageBillingDetachedSnapshot(t *testing.T) {
	var p ImageBillingPolicy
	if err := json.Unmarshal(
		[]byte(
			`{"version":1,"scenario":"generation","outputPixelTiers":[{"maxPixels":null,"amountMicros":30,"unitQuantity":1}]}`,
		),
		&p,
	); err != nil {
		t.Fatal(err)
	}

	var u ImageUsage
	if err := json.Unmarshal(
		[]byte(
			`{"version":1,"state":"complete","scenario":"generation","outputs":[{"index":0,"width":1,"height":1}]}`,
		),
		&u,
	); err != nil {
		t.Fatal(err)
	}

	r, err := EvaluateImageBilling(&p, &u, ImageBillingRate{})
	if err != nil {
		t.Fatal(err)
	}

	p.OutputPixelTiers[0].AmountMicros = 99
	u.Outputs[0].Index = 99

	if *r.AmountMicros != 30 || r.Lines[0].Rate.AmountMicros != 30 ||
		r.Lines[0].OutputIndexes[0] != 0 {
		t.Fatal("result aliases input")
	}

	r.Lines[0].OutputIndexes[0] = 5

	if u.Outputs[0].Index != 99 {
		t.Fatal("input aliases result")
	}
}
