//go:build darwin && cgo

package cursor

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <Security/Security.h>

#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
static OSStatus cursorFindPassword(Boolean allowInteraction, UInt32 *length, void **data) {
	Boolean previous;
	OSStatus status = SecKeychainGetUserInteractionAllowed(&previous);
	if (status != errSecSuccess) return status;
	status = SecKeychainSetUserInteractionAllowed(allowInteraction);
	if (status != errSecSuccess) return status;
	const char service[] = "Cursor Safe Storage";
	const char account[] = "Cursor Key";
	status = SecKeychainFindGenericPassword(NULL, sizeof(service)-1, service,
		sizeof(account)-1, account, length, data, NULL);
	OSStatus restore = SecKeychainSetUserInteractionAllowed(previous);
	return restore == errSecSuccess ? status : restore;
}
static void cursorFreePassword(UInt32 length, void *data) {
	if (data == NULL) return;
	volatile unsigned char *bytes = data;
	while (length-- > 0) *bytes++ = 0;
	SecKeychainItemFreeContent(NULL, data);
}
#pragma clang diagnostic pop
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

// SecKeychain's UI flag is process-wide (the query-only UI flag does not cover
// legacy login-keychain items). Serialize our scoped change, restore the prior
// state on every lookup, and never permit a request to display a security UI.
var cursorKeychainMu sync.Mutex

func readCursorKeychainPasswordWithInteraction(allowInteraction bool) ([]byte, error) {
	cursorKeychainMu.Lock()
	defer cursorKeychainMu.Unlock()
	var length C.UInt32
	var data unsafe.Pointer
	allowed := C.Boolean(0)
	if allowInteraction {
		allowed = C.Boolean(1)
	}
	status := C.cursorFindPassword(allowed, &length, &data)
	defer C.cursorFreePassword(length, data)
	if status != C.errSecSuccess {
		return nil, &CursorKeychainAccessError{Status: int32(status)}
	}
	if data == nil || length == 0 || length > 4096 {
		return nil, fmt.Errorf("invalid Cursor Safe Storage Keychain password length")
	}
	return C.GoBytes(data, C.int(length)), nil
}
