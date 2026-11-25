//go:build darwin && cgo

package certs

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <Security/Security.h>

// Read only this DER certificate's record; never enumerate a trust store.
static OSStatus byokTrustRecord(const UInt8 *der, CFIndex length, int domain,
                               int selfIssued, int *present, int *ssl) {
    *present = 0;
    *ssl = 0;
    CFDataRef data = CFDataCreate(NULL, der, length);
    if (!data) return errSecAllocate;
    SecCertificateRef certificate = SecCertificateCreateWithData(NULL, data);
    CFRelease(data);
    if (!certificate) return errSecDecode;
    CFArrayRef settings = NULL;
    OSStatus status = SecTrustSettingsCopyTrustSettings(certificate,
        (SecTrustSettingsDomain)domain, &settings);
    CFRelease(certificate);
    if (status == errSecItemNotFound || status == errSecNoTrustSettings) return errSecSuccess;
    if (status != errSecSuccess) return status;
    *present = 1;
    if (!settings) return errSecDecode;
    CFIndex count = CFArrayGetCount(settings);
    if (count == 0 && selfIssued) *ssl = 1;
    for (CFIndex i = 0; i < count; i++) {
        CFTypeRef item = CFArrayGetValueAtIndex(settings, i);
        if (!item || CFGetTypeID(item) != CFDictionaryGetTypeID()) continue;
        CFDictionaryRef rule = (CFDictionaryRef)item;
        // Chromium does not use application/hostname-scoped trust records.
        if (CFDictionaryContainsKey(rule, kSecTrustSettingsApplication) ||
            CFDictionaryContainsKey(rule, kSecTrustSettingsPolicyString)) continue;
        SecPolicyRef policy = (SecPolicyRef)CFDictionaryGetValue(rule, kSecTrustSettingsPolicy);
        if (policy) {
            if (CFGetTypeID(policy) != SecPolicyGetTypeID()) continue;
            CFDictionaryRef properties = SecPolicyCopyProperties(policy);
            if (!properties) continue;
            CFTypeRef oid = CFDictionaryGetValue(properties, kSecPolicyOid);
            Boolean matches = oid && CFEqual(oid, kSecPolicyAppleSSL);
            CFRelease(properties);
            if (!matches) continue;
        }
        int result = kSecTrustSettingsResultTrustRoot;
        CFTypeRef value = CFDictionaryGetValue(rule, kSecTrustSettingsResult);
        if (value && (CFGetTypeID(value) != CFNumberGetTypeID() ||
            !CFNumberGetValue((CFNumberRef)value, kCFNumberIntType, &result))) continue;
        if (result == kSecTrustSettingsResultDeny) { *ssl = -1; break; }
        if (result == kSecTrustSettingsResultTrustAsRoot ||
            (selfIssued && result == kSecTrustSettingsResultTrustRoot)) { *ssl = 1; break; }
    }
    CFRelease(settings);
    return errSecSuccess;
}
*/
import "C"

import (
	"bytes"
	"crypto/x509"
	"fmt"
	"unsafe"
)

func queryDarwinTrust(cert *x509.Certificate, domain darwinTrustDomain) (darwinTrustRecord, error) {
	if cert == nil || len(cert.Raw) == 0 || (domain != darwinUserTrust && domain != darwinAdminTrust) {
		return darwinTrustRecord{}, fmt.Errorf("invalid certificate trust query")
	}
	var present, ssl C.int
	var selfIssued C.int
	if bytes.Equal(cert.RawIssuer, cert.RawSubject) {
		selfIssued = 1
	}
	status := C.byokTrustRecord((*C.UInt8)(unsafe.Pointer(&cert.Raw[0])), C.CFIndex(len(cert.Raw)), C.int(domain), selfIssued, &present, &ssl)
	if status != C.errSecSuccess {
		return darwinTrustRecord{}, fmt.Errorf("read exact CA trust domain %d: status %d", domain, int32(status))
	}
	return darwinTrustRecord{present: present != 0, ssl: int(ssl)}, nil
}
