//go:build darwin && cgo

package cursor

/*
#cgo LDFLAGS: -framework SystemConfiguration -framework CoreFoundation
#include <SystemConfiguration/SystemConfiguration.h>
#include <stdlib.h>

static SCPreferencesRef cursorNetworkPreferences(void) {
	return SCPreferencesCreate(NULL, CFSTR("cursor-byok network inventory"), NULL);
}

static char *cursorNetworkServiceName(SCPreferencesRef prefs, const char *uuid) {
	CFStringRef serviceID = CFStringCreateWithCString(NULL, uuid, kCFStringEncodingUTF8);
	if (serviceID == NULL) return NULL;
	SCNetworkServiceRef service = SCNetworkServiceCopy(prefs, serviceID);
	CFRelease(serviceID);
	if (service == NULL) return NULL;
	CFStringRef name = SCNetworkServiceGetName(service);
	char *result = NULL;
	if (name != NULL) {
		CFIndex capacity = CFStringGetMaximumSizeForEncoding(CFStringGetLength(name), kCFStringEncodingUTF8) + 1;
		if (capacity > 0) {
			result = malloc((size_t)capacity);
			if (result != NULL && !CFStringGetCString(name, result, capacity, kCFStringEncodingUTF8)) {
				free(result);
				result = NULL;
			}
		}
	}
	CFRelease(service);
	return result;
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

func readDarwinNativeServiceNames(ids []string) (map[string]string, error) {
	// This preferences session is read-only: no authorization, locking, commit,
	// or apply operation is requested. UUID membership comes from the complete
	// validated inventory; a missing native service/name is not deletion proof.
	prefs := C.cursorNetworkPreferences()
	if prefs == 0 {
		return nil, fmt.Errorf("read native macOS network service names: preferences unavailable; recovery entries retained")
	}
	defer C.CFRelease(C.CFTypeRef(prefs))
	names := make(map[string]string, len(ids))
	for _, id := range ids {
		uuid := C.CString(id)
		name := C.cursorNetworkServiceName(prefs, uuid)
		C.free(unsafe.Pointer(uuid))
		if name != nil {
			names[id] = C.GoString(name)
			C.free(unsafe.Pointer(name))
		}
	}
	return names, nil
}
