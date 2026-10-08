package badpkg

// Broken is the failure the open memory row points at: mismatch
// between the declared int return and the string expression.
func Broken() int {
	return "not an int"
}
