//go:build windows

package cursor

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func decryptCursorSafeStorage(dbPath string, encrypted []byte) (string, error) {
	if !bytes.HasPrefix(encrypted, []byte("v10")) {
		return "", fmt.Errorf("unsupported Cursor safeStorage encryption version")
	}
	// state.vscdb lives at <user-data-dir>/User/globalStorage/state.vscdb.
	localState := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(dbPath))), "Local State")
	data, err := os.ReadFile(localState)
	if err != nil {
		return "", fmt.Errorf("read Cursor encryption metadata: %w", err)
	}
	var metadata struct {
		OSCrypt struct {
			EncryptedKey string `json:"encrypted_key"`
		} `json:"os_crypt"`
	}
	if json.Unmarshal(data, &metadata) != nil {
		return "", fmt.Errorf("invalid Cursor encryption metadata")
	}
	wrapped, err := base64.StdEncoding.DecodeString(metadata.OSCrypt.EncryptedKey)
	if err != nil || !bytes.HasPrefix(wrapped, []byte("DPAPI")) || len(wrapped) <= 5 {
		return "", fmt.Errorf("unsupported Cursor encryption key")
	}
	input := windows.DataBlob{Size: uint32(len(wrapped) - 5), Data: &wrapped[5]}
	var output windows.DataBlob
	// UI_FORBIDDEN ensures this never triggers a security/credential prompt.
	if err := windows.CryptUnprotectData(&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return "", fmt.Errorf("decrypt Cursor encryption key: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	key := unsafe.Slice(output.Data, output.Size)
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("invalid Cursor encryption key size")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(encrypted) < 3+gcm.NonceSize()+gcm.Overhead() {
		return "", fmt.Errorf("truncated Cursor safeStorage token")
	}
	plaintext, err := gcm.Open(nil, encrypted[3:3+gcm.NonceSize()], encrypted[3+gcm.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("Cursor safeStorage token authentication failed")
	}
	defer clear(plaintext)
	return string(plaintext), nil
}
