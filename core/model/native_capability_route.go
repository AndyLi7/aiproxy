package model

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/labring/aiproxy/core/relay/mode"
	log "github.com/sirupsen/logrus"
)

// PublicAPIIDFeature is advertised in /api/status while the gateway resolves
// public_api_id and public_capability_aliases. The application writes those
// keys only while it is advertised.
const PublicAPIIDFeature = "public_api_id_v1"

// MaxPublicModelIDLength bounds an alias and the requested ID written to a
// rejected request's log row.
const MaxPublicModelIDLength = 191

// maxSuggestedModels bounds suggested_models in a model error.
const maxSuggestedModels = 10

// PublicCapabilityIdentity is how customers address one capability config.
// CapabilityModel (public_model + "/" + capability) is the internal identity:
// contracts, native tasks, wallets and logs keep it. It always remains callable.
type PublicCapabilityIdentity struct {
	PublicModel     string
	Capability      string
	CapabilityModel string
	// PublicAPIID is a valid public_api_id that differs from CapabilityModel.
	PublicAPIID string
	// Aliases are valid hidden IDs. They are never listed or suggested.
	Aliases []string
}

// Callable is the ID listed in /v1/models and returned in task responses.
func (p PublicCapabilityIdentity) Callable() string {
	if p.PublicAPIID != "" {
		return p.PublicAPIID
	}

	return p.CapabilityModel
}

// Accepted lists every ID that calls this capability in match priority:
// the capability model, public_api_id, then aliases.
func (p PublicCapabilityIdentity) Accepted() []string {
	accepted := []string{p.CapabilityModel}
	if p.PublicAPIID != "" {
		accepted = append(accepted, p.PublicAPIID)
	}

	return append(accepted, p.Aliases...)
}

// Accepts reports whether id calls this capability (exact, case-sensitive).
func (p PublicCapabilityIdentity) Accepts(id string) bool {
	for _, accepted := range p.Accepted() {
		if accepted == id {
			return true
		}
	}

	return false
}

// PublicCapabilityIdentityFromConfig reads a capability config's public IDs.
// It needs the contract version, public_model and capability, with
// public_capability_model equal to public_model + "/" + capability; it does
// not need parameter_schema. public_api_id and aliases are read only from
// native task configs; invalid values are ignored and logged once.
func PublicCapabilityIdentityFromConfig(config ModelConfig) (PublicCapabilityIdentity, bool) {
	version, ok := modelCapabilityContractVersion(
		config.Config[ModelConfigCapabilityContractVersionKey],
	)
	if !ok || version != ModelCapabilityContractVersion {
		return PublicCapabilityIdentity{}, false
	}

	publicModel, ok := modelConfigString(config.Config, ModelConfigPublicModelKey)
	if !ok || strings.Contains(publicModel, modelCapabilityKeySeparator) {
		return PublicCapabilityIdentity{}, false
	}

	capability, ok := modelConfigString(config.Config, ModelConfigCapabilityKey)
	if !ok || strings.ContainsAny(capability, "/:") {
		return PublicCapabilityIdentity{}, false
	}

	capabilityModel, ok := modelConfigString(config.Config, ModelConfigPublicCapabilityModelKey)
	if !ok || capabilityModel != publicModel+"/"+capability {
		return PublicCapabilityIdentity{}, false
	}

	identity := PublicCapabilityIdentity{
		PublicModel:     publicModel,
		Capability:      capability,
		CapabilityModel: capabilityModel,
	}

	_, hasPublicAPIID := config.Config[ModelConfigPublicAPIIDKey]
	_, hasAliases := config.Config[ModelConfigPublicCapabilityAliasesKey]

	if config.Type != mode.NativeTasks {
		if hasPublicAPIID || hasAliases {
			logInvalidPublicID(config.Model, "public IDs", "only native task configs may declare them")
		}

		return identity, true
	}

	if hasPublicAPIID {
		value, isString := config.Config[ModelConfigPublicAPIIDKey].(string)
		switch {
		case isString && value == publicModel:
			identity.PublicAPIID = value
		case isString && value == capabilityModel:
			// Same as the default; nothing to add.
		default:
			logInvalidPublicID(
				config.Model,
				string(ModelConfigPublicAPIIDKey),
				fmt.Sprintf("%.200q must equal public_model or public_capability_model", fmt.Sprint(config.Config[ModelConfigPublicAPIIDKey])),
			)
		}
	}

	if hasAliases {
		identity.Aliases = publicCapabilityAliases(config, identity)
	}

	return identity, true
}

// publicCapabilityAliases keeps the valid, distinct aliases: 2 or 3 non-empty
// segments without whitespace, no "::", at most MaxPublicModelIDLength
// characters, and none equal to the capability's other accepted IDs.
func publicCapabilityAliases(config ModelConfig, identity PublicCapabilityIdentity) []string {
	var items []any
	switch typed := config.Config[ModelConfigPublicCapabilityAliasesKey].(type) {
	case []any:
		items = typed
	case []string:
		for _, item := range typed {
			items = append(items, item)
		}
	case nil:
		return nil
	default:
		logInvalidPublicID(config.Model, string(ModelConfigPublicCapabilityAliasesKey), "must be a list of strings")
		return nil
	}

	seen := map[string]bool{identity.CapabilityModel: true}
	if identity.PublicAPIID != "" {
		seen[identity.PublicAPIID] = true
	}

	var aliases []string
	for _, item := range items {
		alias, isString := item.(string)
		if !isString || !validPublicAlias(alias) || seen[alias] {
			logInvalidPublicID(
				config.Model,
				string(ModelConfigPublicCapabilityAliasesKey),
				fmt.Sprintf("ignored item %.200q", fmt.Sprint(item)),
			)

			continue
		}

		seen[alias] = true
		aliases = append(aliases, alias)
	}

	return aliases
}

func validPublicAlias(alias string) bool {
	if alias == "" || len(alias) > MaxPublicModelIDLength ||
		strings.Contains(alias, modelCapabilityKeySeparator) {
		return false
	}

	segments := strings.Split(alias, "/")
	if len(segments) < 2 || len(segments) > 3 {
		return false
	}

	for _, segment := range segments {
		if segment == "" || strings.IndexFunc(segment, func(r rune) bool {
			return unicode.IsSpace(r) || unicode.IsControl(r)
		}) >= 0 {
			return false
		}
	}

	return true
}

// invalidPublicIDsLogged keeps a config error from being logged on every request.
var invalidPublicIDsLogged sync.Map

func logInvalidPublicID(configModel, key, reason string) {
	if _, logged := invalidPublicIDsLogged.LoadOrStore(configModel+"\x00"+key+"\x00"+reason, true); logged {
		return
	}

	log.Errorf("model config %s: ignore invalid %s: %s", configModel, key, reason)
}

// NativeRouteMatch is the outcome of resolving a native request's model ID.
type NativeRouteMatch int

const (
	NativeRouteNotFound NativeRouteMatch = iota
	NativeRouteFound
	// NativeRouteAmbiguous means two callable configs claim the same ID: a
	// configuration error, never guessed between.
	NativeRouteAmbiguous
)

// NativeRouteResolution is the config a native model ID resolved to.
type NativeRouteResolution struct {
	// Route is the internal route key (the config's model name).
	Route    string
	Identity PublicCapabilityIdentity
	// HasIdentity is false for a legacy config that has only
	// public_capability_model.
	HasIdentity bool
}

// ResolveNativeCapabilityRoute maps a native model ID to the route key of the
// one native task config, among the configs the API key may call, that
// accepts it. Matching is exact and case-sensitive, in priority order:
// public_capability_model, then public_api_id, then aliases. Two configs
// matching at the same priority are ambiguous.
func ResolveNativeCapabilityRoute(
	requested string,
	entitled []ModelConfig,
) (NativeRouteResolution, NativeRouteMatch) {
	type candidate struct {
		config      ModelConfig
		identity    PublicCapabilityIdentity
		hasIdentity bool
	}

	candidates := make([]candidate, 0, len(entitled))
	for _, config := range entitled {
		if config.Type != mode.NativeTasks {
			continue
		}

		identity, ok := PublicCapabilityIdentityFromConfig(config)
		candidates = append(candidates, candidate{config: config, identity: identity, hasIdentity: ok})
	}

	tiers := []func(candidate) bool{
		func(c candidate) bool {
			published, _ := c.config.Config[ModelConfigPublicCapabilityModelKey].(string)
			return published == requested
		},
		func(c candidate) bool {
			return c.hasIdentity && c.identity.PublicAPIID == requested
		},
		func(c candidate) bool {
			if !c.hasIdentity {
				return false
			}

			for _, alias := range c.identity.Aliases {
				if alias == requested {
					return true
				}
			}

			return false
		},
	}

	for _, matches := range tiers {
		var found []candidate
		for _, c := range candidates {
			if matches(c) {
				found = append(found, c)
			}
		}

		switch len(found) {
		case 0:
			continue
		case 1:
			return NativeRouteResolution{
				Route:       found[0].config.Model,
				Identity:    found[0].identity,
				HasIdentity: found[0].hasIdentity,
			}, NativeRouteFound
		default:
			routes := make([]string, 0, len(found))
			for _, c := range found {
				routes = append(routes, c.config.Model)
			}

			sort.Strings(routes)
			log.Errorf("native model id %q is claimed by several model configs: %s",
				requested, strings.Join(routes, ", "))

			return NativeRouteResolution{}, NativeRouteAmbiguous
		}
	}

	return NativeRouteResolution{}, NativeRouteNotFound
}

// ListedPublicModelID is the ID /v1/models lists for a config: the callable ID of
// a capability, else a legacy public_capability_model, else the config's own
// model name. Internal route keys ("::") are never listed; "" means none.
func ListedPublicModelID(config ModelConfig) string {
	if identity, ok := PublicCapabilityIdentityFromConfig(config); ok {
		return identity.Callable()
	}

	if published, ok := modelConfigString(config.Config, ModelConfigPublicCapabilityModelKey); ok &&
		!strings.Contains(published, modelCapabilityKeySeparator) {
		return published
	}

	if strings.Contains(config.Model, modelCapabilityKeySeparator) {
		return ""
	}

	return config.Model
}

// NativeModelSuggestion answers a native request whose model ID resolved to
// no native config the key may call.
type NativeModelSuggestion struct {
	// OtherEndpoint: the ID names a model the key may call on another
	// endpoint (400 native_model_unavailable rather than 404 model_not_found).
	OtherEndpoint bool
	// Models are listed IDs to use instead: sorted, at most 10, never an
	// internal route key or a hidden alias.
	Models []string
}

// SuggestNativeModels applies the suggestion rules, among the configs the API
// key may call only:
//  0. the ID is a non-native config's capability ID, model group ID or model
//     name: that model is on another endpoint; suggest its listed IDs;
//  1. the ID matches a native config's accepted ID ignoring case: suggest
//     that config's callable ID;
//  2. the ID is a native model group ID (public_model, ignoring case): suggest
//     the group's callable IDs;
//  3. the ID has 3 segments whose first two are a native model group ID:
//     suggest the group's callable IDs;
//  4. otherwise nothing.
func SuggestNativeModels(requested string, entitled []ModelConfig) NativeModelSuggestion {
	type native struct {
		identity PublicCapabilityIdentity
		legacy   string
	}

	var natives []native

	other := newSuggestedModels()
	for _, config := range entitled {
		if config.Type == mode.NativeTasks {
			identity, ok := PublicCapabilityIdentityFromConfig(config)
			if ok {
				natives = append(natives, native{identity: identity})
			} else if listed := ListedPublicModelID(config); listed != "" {
				natives = append(natives, native{legacy: listed})
			}

			continue
		}

		identity, ok := PublicCapabilityIdentityFromConfig(config)
		published, _ := config.Config[ModelConfigPublicCapabilityModelKey].(string)
		if requested == config.Model || (published != "" && requested == published) ||
			(ok && (requested == identity.CapabilityModel || requested == identity.PublicModel)) {
			other.matched = true
			other.add(ListedPublicModelID(config))
		}
	}

	if other.matched {
		return NativeModelSuggestion{OtherEndpoint: true, Models: other.list()}
	}

	rules := []func(native) bool{
		func(n native) bool {
			if n.legacy != "" {
				return strings.EqualFold(n.legacy, requested)
			}

			for _, accepted := range n.identity.Accepted() {
				if strings.EqualFold(accepted, requested) {
					return true
				}
			}

			return false
		},
		func(n native) bool {
			return n.legacy == "" && strings.EqualFold(n.identity.PublicModel, requested)
		},
		func(n native) bool {
			if n.legacy != "" || strings.Count(requested, "/") != 2 {
				return false
			}

			return strings.EqualFold(n.identity.PublicModel, requested[:strings.LastIndex(requested, "/")])
		},
	}

	for _, rule := range rules {
		suggested := newSuggestedModels()
		for _, n := range natives {
			if !rule(n) {
				continue
			}

			if n.legacy != "" {
				suggested.add(n.legacy)
			} else {
				suggested.add(n.identity.Callable())
			}
		}

		if len(suggested.ids) > 0 {
			return NativeModelSuggestion{Models: suggested.list()}
		}
	}

	return NativeModelSuggestion{Models: []string{}}
}

// CapabilityGroupSuggestions lists the capability IDs of the configs, among
// those given, whose model group ID (public_model) is requested.
func CapabilityGroupSuggestions(requested string, configs []ModelConfig) []string {
	suggested := newSuggestedModels()
	for _, config := range configs {
		if identity, ok := PublicCapabilityIdentityFromConfig(config); ok &&
			identity.PublicModel == requested {
			suggested.add(identity.Callable())
		}
	}

	return suggested.list()
}

type suggestedModels struct {
	ids     map[string]bool
	matched bool
}

func newSuggestedModels() *suggestedModels {
	return &suggestedModels{ids: map[string]bool{}}
}

func (s *suggestedModels) add(id string) {
	if id == "" || strings.Contains(id, modelCapabilityKeySeparator) {
		return
	}

	s.ids[id] = true
}

func (s *suggestedModels) list() []string {
	list := make([]string, 0, len(s.ids))
	for id := range s.ids {
		list = append(list, id)
	}

	sort.Strings(list)
	if len(list) > maxSuggestedModels {
		list = list[:maxSuggestedModels]
	}

	return list
}

// NativeCallableModelID maps a native task's capability ID (the contract
// model it was frozen with) to the ID customers call it by today, read from
// the enabled native config of its route key. Without such a config, the
// capability ID is returned unchanged.
func NativeCallableModelID(capabilityModel string, configs map[string]ModelConfig) string {
	index := strings.LastIndex(capabilityModel, "/")
	if index <= 0 || index == len(capabilityModel)-1 {
		return capabilityModel
	}

	config, ok := configs[capabilityModel[:index]+modelCapabilityKeySeparator+capabilityModel[index+1:]]
	if !ok || config.Type != mode.NativeTasks {
		return capabilityModel
	}

	identity, ok := PublicCapabilityIdentityFromConfig(config)
	if !ok || identity.CapabilityModel != capabilityModel {
		return capabilityModel
	}

	return identity.Callable()
}
