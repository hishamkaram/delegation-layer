//go:build darwin

package claude

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestInspectLegacyAPIKeyAbsenceUsesExactAttributesOnlyQuery(t *testing.T) {
	const object = uintptr(90)
	var query legacyKeychainQuery
	var released []uintptr
	api := legacyKeychainAPI{
		copyMatching: func(got legacyKeychainQuery) (legacyKeychainResult, error) {
			query = got
			return legacyKeychainResult{status: errSecItemNotFound, object: object, release: func() { released = append(released, object) }}, nil
		},
	}
	if err := inspectLegacyAPIKeyAbsenceWithAPI(profileEnvironment{Values: []string{"USER=fixture-account"}}, api); err != nil {
		t.Fatal(err)
	}
	want := legacyKeychainQuery{Class: "generic-password", Service: "Claude Code", Account: "fixture-account", ReturnAttributes: true, MatchLimit: 1, AuthenticationUI: "fail"}
	if query != want {
		t.Fatalf("wrong keychain query: got=%+v want=%+v", query, want)
	}
	if !slices.Equal(released, []uintptr{object}) {
		t.Fatalf("returned native object was not released exactly once: %v", released)
	}
}

func TestInspectLegacyAPIKeyAbsenceRefusesPresenceAndUncertainStatuses(t *testing.T) {
	for _, status := range []int32{errSecSuccess, -25293} {
		status := status
		released := 0
		api := legacyKeychainAPI{
			copyMatching: func(legacyKeychainQuery) (legacyKeychainResult, error) {
				return legacyKeychainResult{status: status, object: 7, release: func() { released++ }}, nil
			},
		}
		err := inspectLegacyAPIKeyAbsenceWithAPI(profileEnvironment{Values: []string{"USER=fixture-account"}}, api)
		if !errors.Is(err, ErrUnsupportedProfile) || released != 1 {
			t.Fatalf("status %d was not refused/released: %v %d", status, err, released)
		}
		if strings.Contains(err.Error(), "fixture-account") || strings.Contains(err.Error(), "25293") {
			t.Fatalf("native/account detail escaped fixed error: %v", err)
		}
	}
}

func TestInspectLegacyAPIKeyAbsenceMasksNativeErrorsAndReleasesResult(t *testing.T) {
	released := 0
	nativeErr := errors.New("native-account-secret")
	err := inspectLegacyAPIKeyAbsenceWithAPI(profileEnvironment{Values: []string{"USER=fixture-account"}}, legacyKeychainAPI{
		copyMatching: func(legacyKeychainQuery) (legacyKeychainResult, error) {
			return legacyKeychainResult{status: -1, object: 7, release: func() { released++ }}, nativeErr
		},
	})
	if !errors.Is(err, ErrUnsupportedProfile) || released != 1 {
		t.Fatalf("native error was not refused/released: %v %d", err, released)
	}
	if strings.Contains(err.Error(), nativeErr.Error()) {
		t.Fatalf("native diagnostic escaped fixed error: %v", err)
	}
}

func TestLegacyAPIKeyAccountUsesSanitizedFallback(t *testing.T) {
	account, err := legacyAPIKeyAccount(profileEnvironment{Values: []string{"USER=bad account"}})
	if err != nil || account != legacyKeychainFallbackAccount {
		t.Fatalf("invalid USER was not sanitized: %q %v", account, err)
	}
	account, err = legacyAPIKeyAccount(profileEnvironment{Values: []string{"USER=valid.user-1"}})
	if err != nil || account != "valid.user-1" {
		t.Fatalf("valid USER was changed: %q %v", account, err)
	}
}

func TestCopyLegacyKeychainMatchingBuildsAndReleasesAttributesQuery(t *testing.T) {
	var created []string
	var query []legacyKeychainPair
	var released []uintptr
	native := legacyNativeAPI{
		createString: func(value string) uintptr {
			created = append(created, value)
			return uintptr(10 + len(created))
		},
		createDictionary: func(pairs []legacyKeychainPair) uintptr {
			query = slices.Clone(pairs)
			return 20
		},
		copyMatching: func(dictionary uintptr, result *uintptr) int32 {
			if dictionary != 20 {
				t.Fatalf("wrong query object: %d", dictionary)
			}
			*result = 30
			return errSecItemNotFound
		},
		release: func(object uintptr) { released = append(released, object) },
		constants: legacyKeychainConstants{
			classKey: 1, classGenericPassword: 2, attributeServiceKey: 3, returnAttributesKey: 4,
			matchLimitKey: 5, matchLimitOne: 6, authenticationUIKey: 7, authenticationUIFail: 8,
			attributeAccountKey: 9, booleanTrue: 10, keyCallbacks: 11, valueCallbacks: 12,
		},
	}
	result, err := copyLegacyKeychainMatching(native, legacyKeychainQuery{
		Class: "generic-password", Service: "Claude Code", Account: "fixture-account",
		ReturnAttributes: true, MatchLimit: 1, AuthenticationUI: "fail",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(created, []string{"fixture-account", "Claude Code"}) {
		t.Fatalf("wrong native string values: %v", created)
	}
	want := []legacyKeychainPair{
		{Key: "kSecClass", Value: "kSecClassGenericPassword", KeyRef: 1, ValueRef: 2},
		{Key: "kSecAttrService", Value: "Claude Code", KeyRef: 3, ValueRef: 12},
		{Key: "kSecReturnAttributes", Value: "kCFBooleanTrue", KeyRef: 4, ValueRef: 10},
		{Key: "kSecMatchLimit", Value: "kSecMatchLimitOne", KeyRef: 5, ValueRef: 6},
		{Key: "kSecUseAuthenticationUI", Value: "kSecUseAuthenticationUIFail", KeyRef: 7, ValueRef: 8},
		{Key: "kSecAttrAccount", Value: "fixture-account", KeyRef: 9, ValueRef: 11},
	}
	if !slices.Equal(query, want) {
		t.Fatalf("wrong attributes-only selectors: got=%+v want=%+v", query, want)
	}
	for _, pair := range query {
		if strings.Contains(pair.Key+pair.Value, "ReturnData") {
			t.Fatal("attributes query requested keychain data")
		}
	}
	if result.status != errSecItemNotFound || result.object != 30 {
		t.Fatalf("native result changed: %+v", result)
	}
	result.release()
	if !slices.Equal(released, []uintptr{20, 12, 11, 30}) {
		t.Fatalf("native object ownership was not released in order: %v", released)
	}
}

func TestBindLegacyNativeFunctionsUsesRawStringConstructorTarget(t *testing.T) {
	const (
		securityHandle       = uintptr(101)
		coreFoundationHandle = uintptr(202)
	)
	var names []string
	lookup := func(handle uintptr, name string) (uintptr, error) {
		names = append(names, name)
		if name == "SecItemCopyMatching" && handle != securityHandle {
			t.Fatalf("Security symbol %q used wrong handle %d", name, handle)
		}
		if name != "SecItemCopyMatching" && handle != coreFoundationHandle {
			t.Fatalf("CoreFoundation symbol %q used wrong handle %d", name, handle)
		}
		return uintptr(len(names)), nil
	}
	register := func(target any, symbol uintptr) {
		switch symbol {
		case 1:
			assignLegacyTestFunction(t, target, func(uintptr, *uintptr) int32 { return errSecItemNotFound })
		case 2:
			assignLegacyTestFunction(t, target, func(uintptr, *byte, uint32) uintptr { return 30 })
		case 3:
			assignLegacyTestFunction(t, target, func(uintptr, *uintptr, *uintptr, int64, uintptr, uintptr) uintptr { return 40 })
		case 4:
			assignLegacyTestFunction(t, target, func(uintptr) {})
		default:
			t.Fatalf("unexpected symbol %d for target %T", symbol, target)
		}
	}
	api, err := bindLegacyNativeFunctions(coreFoundationHandle, securityHandle, lookup, register)
	if err != nil {
		t.Fatal(err)
	}
	wantNames := []string{"SecItemCopyMatching", "CFStringCreateWithCString", "CFDictionaryCreate", "CFRelease"}
	if !slices.Equal(names, wantNames) {
		t.Fatalf("wrong symbol binding order: %v", names)
	}
	if api.createStringNative == nil || api.createString != nil {
		t.Fatalf("raw CFString binding did not land on the native field: native=%v wrapper=%v", api.createStringNative != nil, api.createString != nil)
	}
	if got := api.createStringNative(0, nil, legacyCFUTF8Encoding); got != 30 {
		t.Fatalf("raw CFString callback was not retained: %d", got)
	}
}

func assignLegacyTestFunction[T any](t *testing.T, target any, function T) {
	t.Helper()
	bound, ok := target.(*T)
	if !ok {
		t.Fatalf("native target has type %T", target)
	}
	*bound = function
}
