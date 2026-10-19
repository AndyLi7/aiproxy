package conv

// BytesToString returns a string that does not alias the caller's mutable bytes.
func BytesToString(b []byte) string { return string(b) }

// StringToBytes returns independently mutable bytes, including for string literals.
func StringToBytes(s string) []byte { return []byte(s) }
