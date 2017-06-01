package passphrase

import (
	"os"
	"strings"
)

const (
	envPrefix = "NOTARY_SIGNER_"
)

// NewDefaultPassphraseStore instantiates a default passphrase store
func NewDefaultPassphraseStore() Storage {
	return DefaultPassphraseStore{}
}

// DefaultPassphraseStore implements a basic passphrase store which just stores and
// retrieves passphrase from an environment variable.
type DefaultPassphraseStore struct {
}

// SetPassphrase stores the clear passphrase in the ENV variable as is in clear text.
func (pw DefaultPassphraseStore) SetPassphrase(alias string, newPassphrase string) error {
	envVariable := strings.ToUpper(envPrefix + alias)
	error := os.Setenv(envVariable, newPassphrase)

	return error
}

// GetPassphrase retrieves the clear passphrase from the ENV variable.
func (pw DefaultPassphraseStore) GetPassphrase(alias string) (string, error) {
	envVariable := strings.ToUpper(envPrefix + alias)
	pass := os.Getenv(envVariable)
	return pass, nil
}
