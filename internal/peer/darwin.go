//go:build darwin && cgo

package peer

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation -lbsm
#include <stdlib.h>
#include <stdio.h>
#include <unistd.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <bsm/libbsm.h>
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>

// 0: the peer runs as this user and its code satisfies requirement. 1: it does not. -1: the check failed.
static int beacon_peer_check(int fd, const char *requirement) {
	audit_token_t token;
	socklen_t size = sizeof(token);
	if (getsockopt(fd, SOL_LOCAL, LOCAL_PEERTOKEN, &token, &size) != 0 || size != sizeof(token)) return -1;
	if (audit_token_to_euid(token) != getuid() || audit_token_to_ruid(token) != getuid()) return 1;
	CFDataRef audit = CFDataCreate(NULL, (const UInt8 *)&token, sizeof(token));
	if (!audit) return -1;
	const void *keys[] = {kSecGuestAttributeAudit};
	const void *values[] = {audit};
	CFDictionaryRef attributes = CFDictionaryCreate(NULL, keys, values, 1, &kCFTypeDictionaryKeyCallBacks,
		&kCFTypeDictionaryValueCallBacks);
	CFRelease(audit);
	if (!attributes) return -1;
	SecCodeRef code = NULL;
	OSStatus status = SecCodeCopyGuestWithAttributes(NULL, attributes, kSecCSDefaultFlags, &code);
	CFRelease(attributes);
	if (status != errSecSuccess || !code) return 1;
	CFStringRef text = CFStringCreateWithCString(NULL, requirement, kCFStringEncodingUTF8);
	SecRequirementRef req = NULL;
	int result = -1;
	if (text && SecRequirementCreateWithString(text, kSecCSDefaultFlags, &req) == errSecSuccess) {
		result = SecCodeCheckValidity(code, kSecCSDefaultFlags, req) == errSecSuccess ? 0 : 1;
	}
	if (req) CFRelease(req);
	if (text) CFRelease(text);
	CFRelease(code);
	return result;
}

// The running code's cdhash as a requirement, for tests that connect to themselves.
static int beacon_self_requirement(char *out, size_t size) {
	SecCodeRef self = NULL;
	if (SecCodeCopySelf(kSecCSDefaultFlags, &self) != errSecSuccess) return -1;
	SecStaticCodeRef image = NULL;
	OSStatus status = SecCodeCopyStaticCode(self, kSecCSDefaultFlags, &image);
	CFRelease(self);
	if (status != errSecSuccess) return -1;
	CFDictionaryRef info = NULL;
	status = SecCodeCopySigningInformation(image, kSecCSSigningInformation, &info);
	CFRelease(image);
	if (status != errSecSuccess || !info) return -1;
	int ok = -1;
	CFDataRef unique = CFDictionaryGetValue(info, kSecCodeInfoUnique);
	if (unique && size >= 13 + 2 * (size_t)CFDataGetLength(unique)) {
		const UInt8 *bytes = CFDataGetBytePtr(unique);
		int n = snprintf(out, size, "cdhash H\"");
		for (CFIndex i = 0; i < CFDataGetLength(unique); i++) n += snprintf(out + n, size - (size_t)n, "%02x", bytes[i]);
		snprintf(out + n, size - (size_t)n, "\"");
		ok = 0;
	}
	CFRelease(info);
	return ok;
}
*/
import "C"

import (
	"errors"
	"unsafe"
)

func check(fd int, requirement string) error {
	text := C.CString(requirement)
	defer C.free(unsafe.Pointer(text))
	switch C.beacon_peer_check(C.int(fd), text) {
	case 0:
		return nil
	case 1:
		return ErrRefused
	default:
		return errors.New("peer: the listener's identity could not be checked")
	}
}

// SelfRequirement is this process's own cdhash requirement. Tests use it to admit themselves.
func SelfRequirement() (string, error) {
	buf := make([]byte, 256)
	if C.beacon_self_requirement((*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf))) != 0 {
		return "", errors.New("peer: this process has no code signature")
	}
	return C.GoString((*C.char)(unsafe.Pointer(&buf[0]))), nil
}
