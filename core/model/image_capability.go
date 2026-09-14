package model

import "strings"

// ResolveImageCapabilityRoute only aliases registered, identity-checked products.
// Authorization must still be applied to the returned route by the distributor.
// Legacy model names and private admin-demo routes are left unchanged.
func ResolveImageCapabilityRoute(publicID string, lookup func(string) (map[ModelConfigKey]any, bool)) (string, string) {
	if strings.Contains(publicID, "::") {
		return publicID, ""
	}
	index := strings.LastIndex(publicID, "/")
	if index <= 0 {
		return publicID, ""
	}
	parent, capability := publicID[:index], publicID[index+1:]
	if capability != "text-to-image" && capability != "edit" {
		return publicID, ""
	}
	route := parent + "::" + capability
	config, ok := lookup(route)
	if !ok {
		return publicID, ""
	}
	version, valid := modelCapabilityContractVersion(config["capability_contract_version"])
	if !valid || version != ModelCapabilityContractVersion || config["public_model"] != parent || config["capability"] != capability || config["public_capability_model"] != publicID {
		return publicID, ""
	}
	return route, capability
}
