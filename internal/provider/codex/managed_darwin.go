//go:build darwin

package codex

import (
	"errors"
	"fmt"

	"github.com/ebitengine/purego"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type preferenceAPI struct {
	create    func(uintptr, string, uint32) uintptr
	copyValue func(uintptr, uintptr) uintptr
	release   func(uintptr)
}

const coreFoundation = "/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation"

// managedSources uses the same authoritative preference API as the pinned
// native loader. Filesystem plist absence cannot establish MDM absence.
// The library and every returned CF object belong to this finite call.
func managedSources() (sources []task.PolicySourceDigest, resultErr error) {
	handle, err := purego.Dlopen(coreFoundation, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, fmt.Errorf("%w: load managed preference API: %w", ErrUnsupportedProfile, err)
	}
	defer func() { resultErr = errors.Join(resultErr, purego.Dlclose(handle)) }()
	var api preferenceAPI
	for _, binding := range []struct {
		name   string
		target any
	}{{"CFStringCreateWithCString", &api.create}, {"CFPreferencesCopyAppValue", &api.copyValue}, {"CFRelease", &api.release}} {
		symbol, lookupErr := purego.Dlsym(handle, binding.name)
		if lookupErr != nil {
			return nil, fmt.Errorf("%w: managed preference API unavailable: %w", ErrUnsupportedProfile, lookupErr)
		}
		purego.RegisterFunc(binding.target, symbol)
	}
	return readManagedPreferences(api)
}

func readManagedPreferences(api preferenceAPI) ([]task.PolicySourceDigest, error) {
	var sources []task.PolicySourceDigest
	domain := api.create(0, "com.openai.codex", 0x08000100)
	if domain == 0 {
		return nil, fmt.Errorf("%w: cannot allocate preference domain", ErrUnsupportedProfile)
	}
	defer api.release(domain)
	for _, key := range []string{"config_toml_base64", "requirements_toml_base64"} {
		name := api.create(0, key, 0x08000100)
		if name == 0 {
			return nil, fmt.Errorf("%w: cannot allocate preference key", ErrUnsupportedProfile)
		}
		value := api.copyValue(name, domain)
		api.release(name)
		if value != 0 {
			api.release(value)
			return nil, fmt.Errorf("%w: managed Codex preferences are unsupported", ErrUnsupportedProfile)
		}
		sources = append(sources, task.PolicySourceDigest{Path: coreFoundation, Kind: "CFPreferences:com.openai.codex:" + key})
	}
	return sources, nil
}
