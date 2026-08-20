package plugin

import "regexp"

// storageIDPattern accepts opaque storage IDs (UUIDs today, legacy opaque
// strings historically): ASCII letters, digits, dot, underscore, hyphen, at
// most 64 bytes. Wire plugin IDs carry no type prefix; plugin_type travels as
// its own field.
var storageIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func parseStorageID(value string) (string, error) {
	if !storageIDPattern.MatchString(value) {
		return "", ErrInvalidRequest
	}
	return value, nil
}
