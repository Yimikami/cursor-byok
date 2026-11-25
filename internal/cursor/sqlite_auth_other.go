//go:build !windows

package cursor

import "fmt"

func decryptCursorSafeStorage(_ string, _ []byte) (string, error) {
	return "", fmt.Errorf("encrypted Cursor session identification is not supported on this platform")
}
