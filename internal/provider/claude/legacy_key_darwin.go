//go:build darwin

package claude

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"github.com/ebitengine/purego"
)

const (
	legacySecurityFramework = "/System/Library/Frameworks/Security.framework/Security"
	legacyCoreFoundation    = "/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation"
	legacyCFUTF8Encoding    = uint32(0x08000100)

	errSecSuccess       int32 = 0
	errSecItemNotFound  int32 = -25300
	legacyMatchLimitOne       = 1

	legacyKeychainService          = "Claude Code"
	legacyKeychainClass            = "generic-password"
	legacyKeychainAuthenticationUI = "fail"
)

var (
	errLegacyAPIKeyPresent     = fmt.Errorf("%w: legacy Claude API key is present", ErrUnsupportedProfile)
	errLegacyAPIKeyUnavailable = fmt.Errorf("%w: legacy Claude API-key absence is unavailable", ErrUnsupportedProfile)
	errLegacyNativeUnavailable = errors.New("legacy keychain native API unavailable")
)

// legacyKeychainQuery is the complete non-secret query contract. There is no
// return-data selector by design: this check proves presence without reading a
// credential value.
type legacyKeychainQuery struct {
	Class            string
	Service          string
	Account          string
	ReturnAttributes bool
	MatchLimit       int
	AuthenticationUI string
}

type legacyKeychainResult struct {
	status  int32
	object  uintptr
	release func()
}

// legacyKeychainAPI is the narrow injected boundary used by the pure checker
// and its tests. Native object ownership is returned with each result.
type legacyKeychainAPI struct {
	copyMatching func(legacyKeychainQuery) (legacyKeychainResult, error)
}

type legacyKeychainPair struct {
	Key      string
	Value    string
	KeyRef   uintptr
	ValueRef uintptr
}

type legacyKeychainConstants struct {
	classKey             uintptr
	classGenericPassword uintptr
	attributeServiceKey  uintptr
	returnAttributesKey  uintptr
	matchLimitKey        uintptr
	matchLimitOne        uintptr
	authenticationUIKey  uintptr
	authenticationUIFail uintptr
	attributeAccountKey  uintptr
	booleanTrue          uintptr
	keyCallbacks         uintptr
	valueCallbacks       uintptr
}

type legacyNativeAPI struct {
	createString           func(string) uintptr
	createDictionary       func([]legacyKeychainPair) uintptr
	copyMatching           func(uintptr, *uintptr) int32
	release                func(uintptr)
	constants              legacyKeychainConstants
	createStringNative     func(uintptr, *byte, uint32) uintptr
	createDictionaryNative func(uintptr, *uintptr, *uintptr, int64, uintptr, uintptr) uintptr
}

// inspectLegacyAPIKeyAbsence proves only that the legacy API-key item is
// absent. It never reads keychain data and masks native diagnostics from the
// caller so account names and OSStatus strings cannot enter errors.
func inspectLegacyAPIKeyAbsence(environment profileEnvironment) (resultErr error) {
	coreFoundation, err := purego.Dlopen(legacyCoreFoundation, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return errLegacyAPIKeyUnavailable
	}
	defer func() {
		if closeErr := purego.Dlclose(coreFoundation); closeErr != nil && resultErr == nil {
			resultErr = errLegacyAPIKeyUnavailable
		}
	}()

	security, err := purego.Dlopen(legacySecurityFramework, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return errLegacyAPIKeyUnavailable
	}
	defer func() {
		if closeErr := purego.Dlclose(security); closeErr != nil && resultErr == nil {
			resultErr = errLegacyAPIKeyUnavailable
		}
	}()

	native, err := bindLegacyNativeAPI(coreFoundation, security)
	if err != nil {
		return errLegacyAPIKeyUnavailable
	}
	return inspectLegacyAPIKeyAbsenceWithAPI(environment, newLegacyKeychainAPI(native))
}

func inspectLegacyAPIKey(environment profileEnvironment) legacyAPIKeyInspection {
	if err := inspectLegacyAPIKeyAbsence(environment); err != nil {
		return legacyAPIKeyInspection{Err: err}
	}
	return legacyAPIKeyInspection{Verified: true}
}

func inspectLegacyAPIKeyAbsenceWithAPI(environment profileEnvironment, api legacyKeychainAPI) error {
	account, err := legacyAPIKeyAccount(environment)
	if err != nil || api.copyMatching == nil {
		return errLegacyAPIKeyUnavailable
	}
	// kSecUseAuthenticationUIFail is scoped to this SecItemCopyMatching call;
	// it returns errSecInteractionNotAllowed instead of presenting UI. Avoid
	// the deprecated process-wide interaction setting.
	result, err := api.copyMatching(legacyKeychainQuery{
		Class:            legacyKeychainClass,
		Service:          legacyKeychainService,
		Account:          account,
		ReturnAttributes: true,
		MatchLimit:       legacyMatchLimitOne,
		AuthenticationUI: legacyKeychainAuthenticationUI,
	})
	if result.object != 0 {
		if result.release == nil {
			return errLegacyAPIKeyUnavailable
		}
		result.release()
	}
	if err != nil {
		return errLegacyAPIKeyUnavailable
	}
	switch result.status {
	case errSecItemNotFound:
		return nil
	case errSecSuccess:
		return errLegacyAPIKeyPresent
	default:
		return errLegacyAPIKeyUnavailable
	}
}

func newLegacyKeychainAPI(native legacyNativeAPI) legacyKeychainAPI {
	return legacyKeychainAPI{
		copyMatching: func(query legacyKeychainQuery) (legacyKeychainResult, error) {
			return copyLegacyKeychainMatching(native, query)
		},
	}
}

func copyLegacyKeychainMatching(native legacyNativeAPI, query legacyKeychainQuery) (legacyKeychainResult, error) {
	if !validLegacyKeychainQuery(query) || native.createString == nil || native.createDictionary == nil || native.copyMatching == nil || native.release == nil {
		return legacyKeychainResult{}, errLegacyNativeUnavailable
	}
	account := native.createString(query.Account)
	if account == 0 {
		return legacyKeychainResult{}, errLegacyNativeUnavailable
	}
	defer native.release(account)

	service := native.createString(query.Service)
	if service == 0 {
		return legacyKeychainResult{}, errLegacyNativeUnavailable
	}
	defer native.release(service)

	constants := native.constants
	dictionary := native.createDictionary([]legacyKeychainPair{
		{Key: "kSecClass", Value: "kSecClassGenericPassword", KeyRef: constants.classKey, ValueRef: constants.classGenericPassword},
		{Key: "kSecAttrService", Value: query.Service, KeyRef: constants.attributeServiceKey, ValueRef: service},
		{Key: "kSecReturnAttributes", Value: "kCFBooleanTrue", KeyRef: constants.returnAttributesKey, ValueRef: constants.booleanTrue},
		{Key: "kSecMatchLimit", Value: "kSecMatchLimitOne", KeyRef: constants.matchLimitKey, ValueRef: constants.matchLimitOne},
		{Key: "kSecUseAuthenticationUI", Value: "kSecUseAuthenticationUIFail", KeyRef: constants.authenticationUIKey, ValueRef: constants.authenticationUIFail},
		{Key: "kSecAttrAccount", Value: query.Account, KeyRef: constants.attributeAccountKey, ValueRef: account},
	})
	if dictionary == 0 {
		return legacyKeychainResult{}, errLegacyNativeUnavailable
	}
	defer native.release(dictionary)

	var result uintptr
	status := native.copyMatching(dictionary, &result)
	return legacyKeychainResult{status: status, object: result, release: func() {
		if result != 0 {
			native.release(result)
		}
	}}, nil
}

func validLegacyKeychainQuery(query legacyKeychainQuery) bool {
	return query.Class == legacyKeychainClass &&
		query.Service == legacyKeychainService &&
		validLegacyAPIKeyAccount(query.Account) &&
		query.ReturnAttributes &&
		query.MatchLimit == legacyMatchLimitOne &&
		query.AuthenticationUI == legacyKeychainAuthenticationUI
}

func bindLegacyNativeAPI(coreFoundation, security uintptr) (legacyNativeAPI, error) {
	api, err := bindLegacyNativeFunctions(coreFoundation, security, purego.Dlsym, purego.RegisterFunc)
	if err != nil {
		return legacyNativeAPI{}, err
	}
	constants, err := loadLegacyConstants(coreFoundation, security)
	if err != nil {
		return legacyNativeAPI{}, err
	}
	api.constants = constants
	api.createString = func(value string) uintptr {
		data := append([]byte(value), 0)
		result := api.createStringNative(0, &data[0], legacyCFUTF8Encoding)
		runtime.KeepAlive(data)
		return result
	}
	api.createDictionary = func(pairs []legacyKeychainPair) uintptr {
		if len(pairs) == 0 {
			return 0
		}
		keys := make([]uintptr, len(pairs))
		values := make([]uintptr, len(pairs))
		for index, pair := range pairs {
			keys[index], values[index] = pair.KeyRef, pair.ValueRef
		}
		result := api.createDictionaryNative(0, &keys[0], &values[0], int64(len(pairs)), api.constants.keyCallbacks, api.constants.valueCallbacks)
		runtime.KeepAlive(keys)
		runtime.KeepAlive(values)
		return result
	}
	return api, nil
}

type (
	legacySymbolLookup      func(uintptr, string) (uintptr, error)
	legacyFunctionRegistrar func(any, uintptr)
)

func bindLegacyNativeFunctions(coreFoundation, security uintptr, lookup legacySymbolLookup, register legacyFunctionRegistrar) (legacyNativeAPI, error) {
	if lookup == nil || register == nil {
		return legacyNativeAPI{}, errLegacyNativeUnavailable
	}
	var api legacyNativeAPI
	for _, binding := range []struct {
		handle uintptr
		name   string
		target any
	}{
		{security, "SecItemCopyMatching", &api.copyMatching},
		{coreFoundation, "CFStringCreateWithCString", &api.createStringNative},
		{coreFoundation, "CFDictionaryCreate", &api.createDictionaryNative},
		{coreFoundation, "CFRelease", &api.release},
	} {
		symbol, err := lookup(binding.handle, binding.name)
		if err != nil || symbol == 0 {
			return legacyNativeAPI{}, errLegacyNativeUnavailable
		}
		register(binding.target, symbol)
	}
	if api.copyMatching == nil || api.createStringNative == nil || api.createDictionaryNative == nil || api.release == nil {
		return legacyNativeAPI{}, errLegacyNativeUnavailable
	}
	return api, nil
}

func loadLegacyConstants(coreFoundation, security uintptr) (legacyKeychainConstants, error) {
	var constants legacyKeychainConstants
	var err error
	for _, binding := range []struct {
		handle uintptr
		name   string
		target *uintptr
	}{
		{security, "kSecClass", &constants.classKey},
		{security, "kSecClassGenericPassword", &constants.classGenericPassword},
		{security, "kSecAttrService", &constants.attributeServiceKey},
		{security, "kSecReturnAttributes", &constants.returnAttributesKey},
		{security, "kSecMatchLimit", &constants.matchLimitKey},
		{security, "kSecMatchLimitOne", &constants.matchLimitOne},
		{security, "kSecUseAuthenticationUI", &constants.authenticationUIKey},
		{security, "kSecUseAuthenticationUIFail", &constants.authenticationUIFail},
		{security, "kSecAttrAccount", &constants.attributeAccountKey},
		{coreFoundation, "kCFBooleanTrue", &constants.booleanTrue},
	} {
		*binding.target, err = lookupLegacyCFObject(binding.handle, binding.name)
		if err != nil {
			return legacyKeychainConstants{}, err
		}
	}
	constants.keyCallbacks, err = lookupLegacyAddress(coreFoundation, "kCFTypeDictionaryKeyCallBacks")
	if err != nil {
		return legacyKeychainConstants{}, err
	}
	constants.valueCallbacks, err = lookupLegacyAddress(coreFoundation, "kCFTypeDictionaryValueCallBacks")
	if err != nil {
		return legacyKeychainConstants{}, err
	}
	return constants, nil
}

func lookupLegacyAddress(handle uintptr, name string) (uintptr, error) {
	symbol, err := purego.Dlsym(handle, name)
	if err != nil || symbol == 0 {
		return 0, errLegacyNativeUnavailable
	}
	return symbol, nil
}

func lookupLegacyCFObject(handle uintptr, name string) (uintptr, error) {
	symbol, err := lookupLegacyAddress(handle, name)
	if err != nil {
		return 0, err
	}
	value, err := readLegacyPointer(symbol)
	if err != nil {
		return 0, err
	}
	if value == 0 {
		return 0, errLegacyNativeUnavailable
	}
	return value, nil
}

func readLegacyPointer(address uintptr) (uintptr, error) {
	memcpySymbol, err := purego.Dlsym(purego.RTLD_DEFAULT, "memcpy")
	if err != nil || memcpySymbol == 0 {
		return 0, errLegacyNativeUnavailable
	}
	var memcpy func(*uintptr, uintptr, uintptr) uintptr
	purego.RegisterFunc(&memcpy, memcpySymbol)
	var value uintptr
	if memcpy(&value, address, unsafe.Sizeof(value)) == 0 {
		return 0, errLegacyNativeUnavailable
	}
	return value, nil
}
