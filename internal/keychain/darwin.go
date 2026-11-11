//go:build darwin && cgo

package keychain

/*
#cgo CFLAGS: -Wno-deprecated-declarations
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

// beacon_access is the access a new item gets: decrypting it is allowed to code that meets this binary's designated
// requirement (its identifier and signing certificate), not to a path or one build's hash. Any other program asks.
static OSStatus beacon_access(SecAccessRef *out) {
	SecTrustedApplicationRef self = NULL;
	OSStatus status = SecTrustedApplicationCreateFromPath(NULL, &self);
	if (status != errSecSuccess) {
		return status;
	}
	CFArrayRef apps = CFArrayCreate(NULL, (const void **)&self, 1, &kCFTypeArrayCallBacks);
	status = SecAccessCreate(CFSTR("beacon"), apps, out);
	CFRelease(apps);
	CFRelease(self);
	return status;
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
		SecAccessRef access = NULL;
		status = beacon_access(&access);
		if (status == errSecSuccess) {
			const void *ak[2] = {kSecValueData, kSecAttrAccess};
			const void *av[2] = {d, access};
			CFDictionaryRef add = beacon_query(s, a, 2, ak, av);
			status = SecItemAdd(add, NULL);
			CFRelease(add);
			CFRelease(access);
		}
	}
	CFRelease(u);
	CFRelease(q);
	CFRelease(d);
	CFRelease(a);
	CFRelease(s);
	return status;
}

// beacon_accounts returns the account names of every item under service. It asks for attributes only, never data,
// so it does not decrypt an item and does not prompt.
static OSStatus beacon_accounts(const char *service, CFArrayRef *out) {
	CFStringRef s = CFStringCreateWithCString(NULL, service, kCFStringEncodingUTF8);
	const void *k[4] = {kSecClass, kSecAttrService, kSecReturnAttributes, kSecMatchLimit};
	const void *v[4] = {kSecClassGenericPassword, s, kCFBooleanTrue, kSecMatchLimitAll};
	CFDictionaryRef q = CFDictionaryCreate(NULL, k, v, 4, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	OSStatus status = SecItemCopyMatching(q, (CFTypeRef *)out);
	CFRelease(q);
	CFRelease(s);
	return status;
}

static long beacon_count(CFArrayRef items) { return CFArrayGetCount(items); }

// beacon_account_at copies the account name of item i into buf. It returns 0 when there is none or it does not fit.
static int beacon_account_at(CFArrayRef items, long i, char *buf, long size) {
	CFDictionaryRef item = CFArrayGetValueAtIndex(items, i);
	if (CFGetTypeID(item) != CFDictionaryGetTypeID()) {
		return 0;
	}
	CFStringRef a = CFDictionaryGetValue(item, kSecAttrAccount);
	if (a == NULL || CFGetTypeID(a) != CFStringGetTypeID()) {
		return 0;
	}
	return CFStringGetCString(a, buf, size, kCFStringEncodingUTF8) ? 1 : 0;
}

// beacon_signed_with_certificate reports whether this process is signed with a certificate. go build, go run and
// go test sign ad hoc, and an ad-hoc signature has no certificate.
static int beacon_signed_with_certificate(void) {
	SecCodeRef self = NULL;
	if (SecCodeCopySelf(kSecCSDefaultFlags, &self) != errSecSuccess) {
		return 0;
	}
	CFDictionaryRef info = NULL;
	OSStatus status = SecCodeCopySigningInformation((SecStaticCodeRef)self, kSecCSSigningInformation, &info);
	CFRelease(self);
	if (status != errSecSuccess || info == NULL) {
		return 0;
	}
	int ok = 0;
	CFArrayRef certs = CFDictionaryGetValue(info, kSecCodeInfoCertificates);
	CFNumberRef flags = CFDictionaryGetValue(info, kSecCodeInfoFlags);
	uint32_t f = 0;
	if (flags != NULL) {
		CFNumberGetValue(flags, kCFNumberSInt32Type, &f);
	}
	if (certs != NULL && CFArrayGetCount(certs) > 0 && (f & kSecCodeSignatureAdhoc) == 0) {
		ok = 1;
	}
	CFRelease(info);
	return ok;
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
	"os"
	"unsafe"
)

// System is the macOS login keychain, through the Security framework.
type System struct{}

// NewSystem returns the system keychain. A build that is not signed with a certificate gets ErrAdHocBuild and never
// reaches the keychain, so a test or scratch build cannot raise a prompt or add itself to an item. TestKeychainEnv
// set to 1 lets the opt-in keychain test through.
func NewSystem() (Store, error) {
	if os.Getenv(TestKeychainEnv) != "1" && C.beacon_signed_with_certificate() == 0 {
		return nil, ErrAdHocBuild
	}
	return System{}, nil
}

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

// Accounts implements Lister. It reads attributes only, so it never prompts.
func (System) Accounts() ([]string, error) {
	svc := C.CString(Service)
	defer C.free(unsafe.Pointer(svc))
	var items C.CFArrayRef
	if err := status("list", C.beacon_accounts(svc, &items)); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	defer C.CFRelease(C.CFTypeRef(items))
	buf := (*C.char)(C.malloc(1024))
	defer C.free(unsafe.Pointer(buf))
	var out []string
	for i := C.long(0); i < C.beacon_count(items); i++ {
		if C.beacon_account_at(items, i, buf, 1024) == 1 {
			out = append(out, C.GoString(buf))
		}
	}
	return out, nil
}
