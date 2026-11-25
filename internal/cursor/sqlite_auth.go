package cursor

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// Observed in Cursor's cursorAuthTokenStorageCodec. The backup always retains
// the original representation; plaintext is used only for ownership checks.
const cursorSafeStoragePrefix = "cursor-safe-storage:v1:"

func decodeCursorAuthToken(dbPath, value string) (string, error) {
	return decodeCursorAuthTokenWith(dbPath, value, decryptCursorSafeStorage)
}

func decodeCursorAuthTokenWith(dbPath, value string, decrypt func(string, []byte) (string, error)) (string, error) {
	if !strings.HasPrefix(value, "cursor-safe-storage:") {
		return value, nil
	}
	if !strings.HasPrefix(value, cursorSafeStoragePrefix) {
		return "", fmt.Errorf("unsupported Cursor safeStorage envelope version")
	}
	encrypted, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, cursorSafeStoragePrefix))
	if err != nil {
		return "", fmt.Errorf("invalid Cursor safeStorage envelope")
	}
	return decrypt(dbPath, encrypted)
}
