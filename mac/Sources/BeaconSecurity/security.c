#include "BeaconSecurity.h"

#include <CoreFoundation/CoreFoundation.h>
#include <Security/Security.h>
#include <bsm/libbsm.h>
#include <stdio.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/un.h>

int beacon_peer_match(int fd, uid_t uid, const char *const *requirements, int count, pid_t *pid,
                      char *path, size_t path_size) {
    audit_token_t token;
    socklen_t size = sizeof(token);
    *pid = -1;
    if (path_size > 0) path[0] = '\0';
    if (getsockopt(fd, SOL_LOCAL, LOCAL_PEERTOKEN, &token, &size) != 0 || size != sizeof(token)) return -1;
    if (audit_token_to_euid(token) != uid || audit_token_to_ruid(token) != uid) return -1;
    *pid = audit_token_to_pid(token);
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
    if (status != errSecSuccess || !code) return -1;
    int match = -1;
    for (int i = 0; i < count && match < 0; i++) {
        CFStringRef text = CFStringCreateWithCString(NULL, requirements[i], kCFStringEncodingUTF8);
        SecRequirementRef requirement = NULL;
        if (text && SecRequirementCreateWithString(text, kSecCSDefaultFlags, &requirement) == errSecSuccess &&
            SecCodeCheckValidity(code, kSecCSDefaultFlags, requirement) == errSecSuccess) match = i;
        if (requirement) CFRelease(requirement);
        if (text) CFRelease(text);
    }
    CFURLRef url = NULL;
    if (match >= 0 && path_size > 0 && SecCodeCopyPath(code, kSecCSDefaultFlags, &url) == errSecSuccess && url) {
        if (!CFURLGetFileSystemRepresentation(url, true, (UInt8 *)path, (CFIndex)path_size)) path[0] = '\0';
        CFRelease(url);
    }
    CFRelease(code);
    return match;
}

static CFDictionaryRef self_signing_information(void) {
    SecCodeRef self = NULL;
    if (SecCodeCopySelf(kSecCSDefaultFlags, &self) != errSecSuccess) return NULL;
    SecStaticCodeRef image = NULL;
    OSStatus status = SecCodeCopyStaticCode(self, kSecCSDefaultFlags, &image);
    CFRelease(self);
    if (status != errSecSuccess) return NULL;
    CFDictionaryRef info = NULL;
    status = SecCodeCopySigningInformation(image, kSecCSSigningInformation, &info);
    CFRelease(image);
    return status == errSecSuccess ? info : NULL;
}

bool beacon_self_cdhash_requirement(char *out, size_t size) {
    CFDictionaryRef info = self_signing_information();
    if (!info) return false;
    bool ok = false;
    CFDataRef unique = CFDictionaryGetValue(info, kSecCodeInfoUnique);
    if (unique && size >= 13 + 2 * (size_t)CFDataGetLength(unique)) {
        const UInt8 *bytes = CFDataGetBytePtr(unique);
        int n = snprintf(out, size, "cdhash H\"");
        for (CFIndex i = 0; i < CFDataGetLength(unique); i++) n += snprintf(out + n, size - (size_t)n, "%02x", bytes[i]);
        snprintf(out + n, size - (size_t)n, "\"");
        ok = true;
    }
    CFRelease(info);
    return ok;
}

bool beacon_self_is(const char *identifier) {
    CFDictionaryRef info = self_signing_information();
    if (!info) return false;
    CFStringRef expected = CFStringCreateWithCString(NULL, identifier, kCFStringEncodingUTF8);
    CFStringRef actual = CFDictionaryGetValue(info, kSecCodeInfoIdentifier);
    CFArrayRef certificates = CFDictionaryGetValue(info, kSecCodeInfoCertificates);
    bool ok = expected && actual && CFEqual(expected, actual) && certificates && CFArrayGetCount(certificates) > 0;
    if (expected) CFRelease(expected);
    CFRelease(info);
    return ok;
}

static bool requirement_text(SecStaticCodeRef code, char *out, size_t size) {
    SecRequirementRef requirement = NULL;
    if (SecCodeCopyDesignatedRequirement(code, kSecCSDefaultFlags, &requirement) != errSecSuccess) return false;
    CFStringRef text = NULL;
    OSStatus status = SecRequirementCopyString(requirement, kSecCSDefaultFlags, &text);
    CFRelease(requirement);
    if (status != errSecSuccess || !text) return false;
    bool ok = CFStringGetCString(text, out, (CFIndex)size, kCFStringEncodingUTF8);
    CFRelease(text);
    return ok;
}

bool beacon_self_designated_requirement(char *out, size_t size) {
    SecCodeRef self = NULL;
    if (SecCodeCopySelf(kSecCSDefaultFlags, &self) != errSecSuccess) return false;
    SecStaticCodeRef image = NULL;
    OSStatus status = SecCodeCopyStaticCode(self, kSecCSDefaultFlags, &image);
    CFRelease(self);
    if (status != errSecSuccess) return false;
    bool ok = requirement_text(image, out, size);
    CFRelease(image);
    return ok;
}

bool beacon_designated_requirement(const char *path, char *out, size_t size) {
    CFURLRef url = CFURLCreateFromFileSystemRepresentation(NULL, (const UInt8 *)path, (CFIndex)strlen(path), false);
    if (!url) return false;
    SecStaticCodeRef code = NULL;
    bool ok = false;
    if (SecStaticCodeCreateWithPath(url, kSecCSDefaultFlags, &code) == errSecSuccess) {
        ok = SecStaticCodeCheckValidity(code, kSecCSDefaultFlags, NULL) == errSecSuccess && requirement_text(code, out, size);
        CFRelease(code);
    }
    CFRelease(url);
    return ok;
}

static CFMutableDictionaryRef item_query(CFStringRef service, CFStringRef account) {
    CFMutableDictionaryRef query = CFDictionaryCreateMutable(NULL, 0, &kCFTypeDictionaryKeyCallBacks,
                                                             &kCFTypeDictionaryValueCallBacks);
    if (!query) return NULL;
    CFDictionarySetValue(query, kSecClass, kSecClassGenericPassword);
    CFDictionarySetValue(query, kSecAttrService, service);
    CFDictionarySetValue(query, kSecAttrAccount, account);
    return query;
}

static OSStatus read_key(CFStringRef service, CFStringRef account, uint8_t key[32]) {
    CFMutableDictionaryRef query = item_query(service, account);
    if (!query) return -1;
    CFDictionarySetValue(query, kSecReturnData, kCFBooleanTrue);
    CFDictionarySetValue(query, kSecMatchLimit, kSecMatchLimitOne);
    CFTypeRef result = NULL;
    OSStatus status = SecItemCopyMatching(query, &result);
    CFRelease(query);
    if (status != errSecSuccess) return status;
    if (CFGetTypeID(result) != CFDataGetTypeID() || CFDataGetLength(result) != 32) status = -1;
    else memcpy(key, CFDataGetBytePtr(result), 32);
    CFRelease(result);
    return status;
}

static OSStatus delete_key(CFStringRef service, CFStringRef account) {
    CFMutableDictionaryRef query = item_query(service, account);
    if (!query) return -1;
    OSStatus status = SecItemDelete(query);
    CFRelease(query);
    return status;
}

// Only code meeting this app's designated requirement reads the item without asking.
static OSStatus add_key(CFStringRef service, CFStringRef account, const uint8_t key[32]) {
    SecTrustedApplicationRef self = NULL;
    OSStatus status = SecTrustedApplicationCreateFromPath(NULL, &self);
    if (status != errSecSuccess) return status;
    CFArrayRef apps = CFArrayCreate(NULL, (const void **)&self, 1, &kCFTypeArrayCallBacks);
    SecAccessRef access = NULL;
    status = SecAccessCreate(CFSTR("Beacon vault key"), apps, &access);
    CFRelease(apps);
    CFRelease(self);
    if (status != errSecSuccess) return status;
    CFMutableDictionaryRef item = item_query(service, account);
    CFDataRef data = CFDataCreate(NULL, key, 32);
    if (!item || !data) {
        if (item) CFRelease(item);
        if (data) CFRelease(data);
        CFRelease(access);
        return -1;
    }
    CFDictionarySetValue(item, kSecAttrAccess, access);
    CFDictionarySetValue(item, kSecValueData, data);
    status = SecItemAdd(item, NULL);
    CFRelease(item);
    CFRelease(data);
    CFRelease(access);
    return status;
}

int beacon_vault_key(const char *identifier, const char *service_name, const char *account_name, bool create,
                     uint8_t key[32]) {
    if (!beacon_self_is(identifier)) return -1;
    CFStringRef service = CFStringCreateWithCString(NULL, service_name, kCFStringEncodingUTF8);
    CFStringRef account = CFStringCreateWithCString(NULL, account_name, kCFStringEncodingUTF8);
    OSStatus status = -1;
    if (service && account) {
        if (!create) {
            status = read_key(service, account, key);
        } else {
            // Never adopt: an item that exists before its vault was made elsewhere, or orphaned by a crash.
            status = delete_key(service, account);
            if (status == errSecSuccess || status == errSecItemNotFound) {
                uint8_t fresh[32];
                status = SecRandomCopyBytes(kSecRandomDefault, sizeof(fresh), fresh);
                if (status == errSecSuccess) status = add_key(service, account, fresh);
                if (status == errSecSuccess) memcpy(key, fresh, sizeof(fresh));
                else if (status == errSecDuplicateItem) status = BEACON_VAULT_KEY_EXISTS;
                memset(fresh, 0, sizeof(fresh));
            } else {
                status = BEACON_VAULT_KEY_EXISTS;
            }
        }
    }
    if (status != errSecSuccess) memset(key, 0, 32);
    if (service) CFRelease(service);
    if (account) CFRelease(account);
    return status;
}
