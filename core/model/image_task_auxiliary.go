package model

import "github.com/labring/aiproxy/core/common/imagecapabilities"

// Source-declared auxiliary assets remain attached to one primary generated image.
// No recursively nested assets or arbitrary diagnostic URLs are supported.
func ValidAuxiliaryImageName(name string) bool {
	return imagecapabilities.ValidAuxiliaryImageName(name)
}
func ValidAuxiliaryImages(output ImageOutput) bool {
	for name, asset := range output.AuxiliaryImages {
		if !ValidAuxiliaryImageName(name) || (asset != nil && len(asset.AuxiliaryImages) != 0) {
			return false
		}
	}
	return true
}

// CloneImageOutputs isolates maps/pointers for CAS checkpoints and public URL
// rewriting. Callers validate the one-level shape before exposing any asset.
func CloneImageOutputs(outputs []ImageOutput) []ImageOutput {
	if outputs == nil {
		return nil
	}
	copy := append([]ImageOutput(nil), outputs...)
	for i := range copy {
		copy[i].Layer = append([]byte(nil), outputs[i].Layer...)
		if outputs[i].AuxiliaryImages == nil {
			continue
		}
		copy[i].AuxiliaryImages = make(map[string]*ImageOutput, len(outputs[i].AuxiliaryImages))
		for name, asset := range outputs[i].AuxiliaryImages {
			if asset == nil {
				copy[i].AuxiliaryImages[name] = nil
				continue
			}
			cloned := *asset
			cloned.Layer = append([]byte(nil), asset.Layer...)
			copy[i].AuxiliaryImages[name] = &cloned
		}
	}
	return copy
}
