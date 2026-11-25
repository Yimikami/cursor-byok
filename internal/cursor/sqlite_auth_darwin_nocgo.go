//go:build darwin && !cgo

package cursor

import "fmt"

func readCursorKeychainPasswordWithInteraction(_ bool) ([]byte, error) {
	return nil, fmt.Errorf("macOS Cursor safeStorage requires a build with Keychain support (CGO_ENABLED=1)")
}
