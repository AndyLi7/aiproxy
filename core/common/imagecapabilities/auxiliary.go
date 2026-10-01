package imagecapabilities

// ValidAuxiliaryImageName is shared by archive validation, signed delivery and
// storage identity. Provider adapters must additionally require a frozen source projection.
func ValidAuxiliaryImageName(name string) bool {
	return name == "mask_image" || name == "transparent_overlay"
}
