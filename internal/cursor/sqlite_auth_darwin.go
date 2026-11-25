//go:build darwin

package cursor

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1"
	"crypto/subtle"
	"fmt"
	"unicode/utf8"
)

// Cursor 3.32.4 ships Electron 43.8.9's synchronous OSCrypt v10 format:
// PBKDF2-HMAC-SHA1(password, "saltysalt", 1003, 16), AES-CBC, space IV.
// The service/account identify only Cursor's encryption password; never search
// other applications' Keychain entries or create a replacement encryption key.
const cursorKeychainService = "Cursor Safe Storage"
const cursorKeychainAccount = "Cursor Key"

func decryptCursorSafeStorage(_ string, encrypted []byte) (string, error) {
	return decryptCursorSafeStorageDarwin(encrypted, func() ([]byte, error) {
		return readCursorKeychainPasswordWithInteraction(false)
	})
}

func requestCursorKeychainAccess() error {
	return requestCursorKeychainAccessWith(readCursorKeychainPasswordWithInteraction)
}

func requestCursorKeychainAccessWith(read func(bool) ([]byte, error)) error {
	password, err := read(true)
	defer clear(password)
	if err != nil {
		return err
	}
	if len(password) == 0 {
		return fmt.Errorf("Cursor safeStorage Keychain password is empty")
	}
	return nil
}

// readPassword returns an owned buffer, which this function clears. Keeping the
// reader injectable lets normal tests use fixtures without touching Keychain.
func decryptCursorSafeStorageDarwin(encrypted []byte, readPassword func() ([]byte, error)) (string, error) {
	if !bytes.HasPrefix(encrypted, []byte("v10")) {
		return "", fmt.Errorf("unsupported Cursor safeStorage encryption version")
	}
	ciphertext := encrypted[3:]
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return "", fmt.Errorf("truncated Cursor safeStorage token")
	}
	password, err := readPassword()
	defer clear(password)
	if err != nil {
		return "", err
	}
	if len(password) == 0 {
		return "", fmt.Errorf("Cursor safeStorage Keychain password is empty")
	}
	key, err := pbkdf2.Key(sha1.New, string(password), []byte("saltysalt"), 1003, aes.BlockSize)
	if err != nil {
		return "", fmt.Errorf("derive Cursor safeStorage encryption key")
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("invalid Cursor safeStorage encryption key size")
	}
	plaintext := make([]byte, len(ciphertext))
	defer clear(plaintext)
	cipher.NewCBCDecrypter(block, bytes.Repeat([]byte{' '}, aes.BlockSize)).CryptBlocks(plaintext, ciphertext)
	padding := int(plaintext[len(plaintext)-1])
	valid := subtle.ConstantTimeLessOrEq(1, padding) & subtle.ConstantTimeLessOrEq(padding, aes.BlockSize)
	for i := 0; i < aes.BlockSize; i++ {
		match := subtle.ConstantTimeByteEq(plaintext[len(plaintext)-1-i], byte(padding))
		valid &= subtle.ConstantTimeSelect(subtle.ConstantTimeLessOrEq(i+1, padding), match, 1)
	}
	if valid != 1 {
		return "", fmt.Errorf("invalid Cursor safeStorage token padding")
	}
	plaintext = plaintext[:len(plaintext)-padding]
	if !utf8.Valid(plaintext) {
		return "", fmt.Errorf("invalid Cursor safeStorage token encoding")
	}
	return string(plaintext), nil
}
