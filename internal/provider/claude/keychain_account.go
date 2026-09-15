package claude

import "os/user"

const legacyKeychainFallbackAccount = "claude-code-user"

func legacyAPIKeyAccount(environment profileEnvironment) (string, error) {
	entries, err := parseEnvironment(environment.Values)
	if err != nil {
		return "", err
	}
	account := entries["USER"]
	if account == "" {
		current, currentErr := user.Current()
		if currentErr == nil {
			account = current.Username
		}
	}
	if !validLegacyAPIKeyAccount(account) {
		account = legacyKeychainFallbackAccount
	}
	return account, nil
}

func validLegacyAPIKeyAccount(account string) bool {
	if account == "" {
		return false
	}
	for _, value := range []byte(account) {
		if (value < 'A' || value > 'Z') && (value < 'a' || value > 'z') && (value < '0' || value > '9') && value != '.' && value != '_' && value != '-' {
			return false
		}
	}
	return true
}
