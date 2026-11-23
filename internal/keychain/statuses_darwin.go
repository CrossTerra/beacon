//go:build darwin && cgo

package keychain

/*
#include <Security/Security.h>
*/
import "C"

// securityRefusals and securityNotFound give the tests the framework's own constants.
func securityRefusals() []int {
	return []int{int(C.errSecAuthFailed), int(C.errSecUserCanceled), int(C.errSecInteractionNotAllowed), int(C.errSecInteractionRequired)}
}

func securityNotFound() int { return int(C.errSecItemNotFound) }
