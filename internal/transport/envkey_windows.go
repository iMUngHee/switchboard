//go:build windows

package transport

import "strings"

// envKeyEqual compares environment variable names, which Windows treats
// case-insensitively.
func envKeyEqual(a, b string) bool { return strings.EqualFold(a, b) }
