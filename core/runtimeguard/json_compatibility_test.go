//nolint:testpackage // Exercises private signing and ledger encoding invariants.
package runtimeguard

import (
	"encoding/json"
	"testing"

	"github.com/labring/aiproxy/core/model"
)

func TestExecutorWireFieldNamesRemainStable(t *testing.T) {
	raw, err := json.Marshal(
		model.ChannelExecutorIdentity{ServiceID: "s", InstanceID: "i", BuildSHA: "b", Epoch: "e"},
	)
	if err != nil {
		t.Fatal(err)
	}

	if string(raw) != `{"ServiceID":"s","InstanceID":"i","BuildSHA":"b","Epoch":"e"}` {
		t.Fatalf("signed field names changed: %s", raw)
	}

	raw, err = json.Marshal(Reply{Kind: "active", Sequence: 3})
	if err != nil {
		t.Fatal(err)
	}

	if string(raw) != `{"Kind":"active","Sequence":3,"Error":""}` {
		t.Fatalf("protocol field names changed: %s", raw)
	}
}

func TestHashValueRejectsInvalidJSON(t *testing.T) {
	if hash, err := hashValue(json.RawMessage("invalid")); err == nil || hash != "" {
		t.Fatalf("invalid JSON accepted: %q %v", hash, err)
	}

	if hash, err := hashValue(make(chan int)); err == nil || hash != "" {
		t.Fatalf("unsupported value accepted: %q %v", hash, err)
	}
}
