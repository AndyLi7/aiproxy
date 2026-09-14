package model

import "testing"

// Backend example channels must remain inaccessible to customer tokens, even
// when the caller knows the private model ID or requests it explicitly.
func TestAdminDemoChannelSetDoesNotGrantCustomerEntitlement(t *testing.T) {
	const privateModel = "tp-demo/seedream/example"
	models := map[string][]string{
		"default":       {"public-model"},
		"tp_admin_demo": {privateModel},
	}
	for _, restricted := range []bool{false, true} {
		token := TokenCache{}
		if restricted {
			token.Models = []string{privateModel}
		}
		group := GroupCache{ID: "customer", Status: GroupStatusEnabled}
		token.SetAvailableSets(group.GetAvailableSets())
		token.SetModelsBySet(models)
		if got := token.FindModel(privateModel); got != "" {
			t.Fatalf("customer resolved private model %q (explicit=%v)", got, restricted)
		}
		token.Range(func(name string) bool {
			if name == privateModel {
				t.Fatal("customer model listing exposed private model")
			}
			return true
		})
	}
	operator := TokenCache{}
	operator.SetAvailableSets([]string{"tp_admin_demo"})
	operator.SetModelsBySet(models)
	if got := operator.FindModel(privateModel); got != privateModel {
		t.Fatalf("operator cannot resolve its internal route: %q", got)
	}
	if got := operator.FindModel("public-model"); got != "" {
		t.Fatalf("operator unexpectedly fell back to public channel: %q", got)
	}
}

func TestInternalGroupStatusAloneDoesNotEnableDemoChannelSet(t *testing.T) {
	group := GroupCache{ID: "tp_admin_demo", Status: GroupStatusInternal}
	token := TokenCache{}
	token.SetAvailableSets(group.GetAvailableSets())
	token.SetModelsBySet(map[string][]string{"tp_admin_demo": {"private-model"}})
	if token.FindModel("private-model") != "" {
		t.Fatal("internal status implicitly granted channel membership")
	}
}
