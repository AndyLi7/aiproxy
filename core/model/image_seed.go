package model

import (
	"encoding/json"
	"strconv"
)

// The decimal sidecar preserves large provider seeds without changing the
// legacy integer column. Public JSON keeps the documented integer type.
func (task ImageTask) MarshalJSON() ([]byte, error) {
	type alias ImageTask
	var seed *json.Number
	if task.SeedExact != "" {
		value := json.Number(task.SeedExact)
		seed = &value
	} else if task.Seed != nil {
		value := json.Number(strconv.FormatInt(*task.Seed, 10))
		seed = &value
	}
	return json.Marshal(struct {
		*alias
		Seed *json.Number `json:"seed,omitempty"`
	}{alias: (*alias)(&task), Seed: seed})
}
