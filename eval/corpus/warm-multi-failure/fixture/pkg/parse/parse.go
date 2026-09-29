package parse

import "strconv"

// Port parses a port string, returning 0 when it is empty.
func Port(s string) int {
	if s == "" {
		return -1
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return n
}
