package cursor

import "fmt"

// CursorKeychainAccessError exposes only an OS status, never a credential.
// Callers can distinguish a Keychain access denial from malformed ciphertext.
type CursorKeychainAccessError struct{ Status int32 }

func (e *CursorKeychainAccessError) Error() string {
	return fmt.Sprintf("Cursor Safe Storage Keychain access is unavailable (status %d)", e.Status)
}

// RequestCursorKeychainAccess may display macOS's security permission dialog.
// Call ONLY from an explicit user action, never a proxy request or automatic
// auth recovery. It accesses the exact Cursor encryption-key item, clears the
// returned bytes immediately and does not modify stored account credentials.
// Other platforms do not need this permission step.
func RequestCursorKeychainAccess() error { return requestCursorKeychainAccess() }
