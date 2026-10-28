//go:build !windows

package transport

// envKeyEqual compares environment variable names; they are case-sensitive here.
func envKeyEqual(a, b string) bool { return a == b }
