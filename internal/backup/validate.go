package backup

import "errors"

// ErrPassphraseRequired is returned by ValidateRemoteDestination when a
// remote destination is requested but no backup passphrase is configured
// yet (Q80). Every remote archive is age-encrypted to the onboarding
// recipient alone (age's own format refuses to combine a scrypt recipient
// with any other recipient in one file); the matching private identity
// travels alongside it wrapped under the backup passphrase instead
// (encryptArchiveForDestination's sidecar) — without a passphrase, that
// sidecar could never be produced, so a remote destination could never be
// written to safely. #60's POST /backup/destinations calls this before
// persisting a new destination and maps this error to a 400, rather than
// accepting the destination and failing silently at the next backup run.
var ErrPassphraseRequired = errors.New("backup: a backup passphrase must be set before adding a remote destination")

// ValidateRemoteDestination refuses a remote destination request when
// hasPassphrase is false. Local destinations are unaffected — encryption
// is optional for them (doc 10 §1, Q80).
func ValidateRemoteDestination(hasPassphrase bool) error {
	if !hasPassphrase {
		return ErrPassphraseRequired
	}
	return nil
}
