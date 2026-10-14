package registry

import (
	"strings"
	"testing"

	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
)

type stubAdaptor struct {
	adaptor.Adaptor
	name string
}

func TestRegisterPanicsOnDuplicateChannelType(t *testing.T) {
	// A number no real channel uses; cleaned up so other tests never see it.
	const testType = model.ChannelType(-424242)

	t.Cleanup(func() {
		mu.Lock()
		delete(adaptors, testType)
		mu.Unlock()
	})

	first := &stubAdaptor{name: "first"}
	Register(testType, first)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("second Register for the same channel type did not panic")
		}

		msg, _ := r.(string)
		if !strings.Contains(msg, "already registered") {
			t.Fatalf("unexpected panic: %v", r)
		}

		got, ok := Get(testType)
		if !ok || got != first {
			t.Fatalf("duplicate Register replaced the first adaptor: %#v", got)
		}
	}()

	Register(testType, &stubAdaptor{name: "second"})
}
