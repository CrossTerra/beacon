//go:build darwin && cgo

package keychain

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <stdlib.h>
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>

static CFDictionaryRef beacon_query(CFStringRef service, CFStringRef account, int extra, const void **keys, const void **values) {
	const void *k[6] = {kSecClass, kSecAttrService, kSecAttrAccount};
	const void *v[6] = {kSecClassGenericPassword, service, account};
	for (int i = 0; i < extra; i++) {
		k[3 + i] = keys[i];
		v[3 + i] = values[i];
	}
	return CFDictionaryCreate(NULL, k, v, 3 + extra, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
}

static OSStatus beacon_set(const char *service, const char *account, const void *data, long length) {
	CFStringRef s = CFStringCreateWithCString(NULL, service, kCFStringEncodingUTF8);
	CFStringRef a = CFStringCreateWithCString(NULL, account, kCFStringEncodingUTF8);
	CFDataRef d = CFDataCreate(NULL, data, length);
	CFDictionaryRef q = beacon_query(s, a, 0, NULL, NULL);
	const void *uk[1] = {kSecValueData};
	const void *uv[1] = {d};
	CFDictionaryRef u = CFDictionaryCreate(NULL, uk, uv, 1, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	OSStatus status = SecItemUpdate(q, u);
	if (status == errSecItemNotFound) {
		CFDictionaryRef add = beacon_query(s, a, 1, uk, uv);
		status = SecItemAdd(add, NULL);
		CFRelease(add);
	}
	CFRelease(u);
	CFRelease(q);
	CFRelease(d);
	CFRelease(a);
	CFRelease(s);
	return status;
}

static OSStatus beacon_get(const char *service, const char *account, CFDataRef *out) {
	CFStringRef s = CFStringCreateWithCString(NULL, service, kCFStringEncodingUTF8);
	CFStringRef a = CFStringCreateWithCString(NULL, account, kCFStringEncodingUTF8);
	const void *k[2] = {kSecReturnData, kSecMatchLimit};
	const void *v[2] = {kCFBooleanTrue, kSecMatchLimitOne};
	CFDictionaryRef q = beacon_query(s, a, 2, k, v);
	OSStatus status = SecItemCopyMatching(q, (CFTypeRef *)out);
	CFRelease(q);
	CFRelease(a);
	CFRelease(s);
	return status;
}

static OSStatus beacon_delete(const char *service, const char *account) {
	CFStringRef s = CFStringCreateWithCString(NULL, service, kCFStringEncodingUTF8);
	CFStringRef a = CFStringCreateWithCString(NULL, account, kCFStringEncodingUTF8);
	CFDictionaryRef q = beacon_query(s, a, 0, NULL, NULL);
	OSStatus status = SecItemDelete(q);
	CFRelease(q);
	CFRelease(a);
	CFRelease(s);
	return status;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// System is the macOS login keychain, through the Security framework.
type System struct{}

// NewSystem returns the system keychain.
func NewSystem() (Store, error) { return System{}, nil }

func status(op string, s C.OSStatus) error {
	switch s {
	case C.errSecSuccess:
		return nil
	case C.errSecItemNotFound:
		return ErrNotFound
	default:
		return fmt.Errorf("keychain: %s failed with OSStatus %d", op, int(s))
	}
}

// Get implements Store.
func (System) Get(account string) (Secret, error) {
	svc, acct := C.CString(Service), C.CString(account)
	defer C.free(unsafe.Pointer(svc))
	defer C.free(unsafe.Pointer(acct))
	var data C.CFDataRef
	if err := status("read", C.beacon_get(svc, acct, &data)); err != nil {
		return Secret{}, err
	}
	defer C.CFRelease(C.CFTypeRef(data))
	n := C.CFDataGetLength(data)
	value := C.GoBytes(unsafe.Pointer(C.CFDataGetBytePtr(data)), C.int(n))
	if len(value) == 0 {
		return Secret{}, ErrNotFound
	}
	return Secret{value: value}, nil
}

// Set implements Store.
func (System) Set(account string, s Secret) error {
	if s.Empty() {
		return errors.New("keychain: empty secret")
	}
	svc, acct := C.CString(Service), C.CString(account)
	defer C.free(unsafe.Pointer(svc))
	defer C.free(unsafe.Pointer(acct))
	data := C.CBytes(s.value)
	defer C.free(data)
	return status("write", C.beacon_set(svc, acct, data, C.long(len(s.value))))
}

// Delete implements Store.
func (System) Delete(account string) error {
	svc, acct := C.CString(Service), C.CString(account)
	defer C.free(unsafe.Pointer(svc))
	defer C.free(unsafe.Pointer(acct))
	return status("delete", C.beacon_delete(svc, acct))
}
