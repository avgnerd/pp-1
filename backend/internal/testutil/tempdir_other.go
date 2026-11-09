//go:build !windows

package testutil

func isDirNotEmpty(error) bool {
	return false
}
