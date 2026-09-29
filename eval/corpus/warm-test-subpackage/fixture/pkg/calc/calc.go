package calc

// Divide returns a/b, or 0 when b is 0.
func Divide(a, b int) int {
	if b == 0 {
		return -1
	}
	return a / b
}
