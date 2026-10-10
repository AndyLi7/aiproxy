// Package imagecapabilities declares reviewed runtime support. Contract data
// cannot grant an adapter new capabilities; new adapters must register here.
package imagecapabilities

// MaxOutputs is the shared metering, result and archive identity ceiling.
const MaxOutputs = 1024

type Capabilities struct {
	Execution        string
	MeasuredBilling  bool
	MeteringVersions []int
}

func Lookup(adapter string) (Capabilities, bool) {
	switch adapter {
	case "fal-image":
		return Capabilities{Execution: "async", MeasuredBilling: true, MeteringVersions: []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}}, true
	case "volcengine-ark-image":
		return Capabilities{Execution: "sync"}, true
	default:
		return Capabilities{}, false
	}
}
func SupportsMeasuredBilling(adapter string) bool {
	c, ok := Lookup(adapter)
	return ok && c.MeasuredBilling
}
func SupportsMetering(adapter, execution string, version int) bool {
	c, ok := Lookup(adapter)
	if !ok || c.Execution != execution {
		return false
	}
	for _, v := range c.MeteringVersions {
		if v == version {
			return true
		}
	}
	return false
}
