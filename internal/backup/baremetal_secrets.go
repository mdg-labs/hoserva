package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// NotRestoredRecipient is the Kind of a NotRestored that is the archive's
// backup recipient, which a bare-metal restore adopts only when identity.age
// opens: without it this installation keeps its own (Q80).
const NotRestoredRecipient = "backup_recipient"

// secretKey names one sealed value: a column of a table in the row with this
// id, as secrets.age names it.
type secretKey struct{ table, column, rowID string }

// restorableSecrets are the columns secrets.age carries a database entry for,
// each keyed by its table's row key (the id column, or `registry` for registry_credentials). A bare-metal
// restore writes them under this installation's machine key, and refuses an
// entry for any other column, so nothing in the file ever names a statement.
// A stack's .env is not among them: secrets.age carries it as a stack entry,
// and the restore seals it into stacks.env.
var restorableSecrets = map[string]bool{
	"acme_config.dns_secret":          true,
	"acme_config.account_key":         true,
	"ups_config.monitor_password":     true,
	"ups_config.network_password":     true,
	"backup_destinations.secrets":     true,
	"notify_channels.secret":          true,
	"registry_credentials.credential": true,
}

// sealedRestore is what a bare-metal restore writes from the opened archive,
// already sealed under this installation's machine key, so that a failure to
// seal any of it is found before the first write.
type sealedRestore struct {
	values     map[secretKey][]byte
	passphrase []byte
	recipient  *sealedRecipient
}

type sealedRecipient struct {
	public         string
	wrapped, check []byte
}

func sealRestore(o SecretsOutcome, cipher RecipientCipher) (sealedRestore, error) {
	out := sealedRestore{values: map[secretKey][]byte{}}
	passphrase, ok := o.OpenedPassphrase()
	if !ok {
		return out, nil
	}
	if cipher == nil {
		return sealedRestore{}, errors.New("no machine-key cipher to seal the archive's secrets under")
	}
	seal := func(what string, plain []byte) ([]byte, error) {
		sealed, err := cipher.Encrypt(plain)
		if err != nil {
			return nil, fmt.Errorf("sealing %s under this installation's machine key: %w", what, err)
		}
		return sealed, nil
	}

	var err error
	if out.passphrase, err = seal("the backup passphrase", []byte(passphrase)); err != nil {
		return sealedRestore{}, err
	}
	if r := o.Recipient(); r != nil {
		wrapped, err := seal("the backup recipient's identity", []byte(r.Identity))
		if err != nil {
			return sealedRestore{}, err
		}
		out.recipient = &sealedRecipient{public: r.Public, wrapped: wrapped, check: computeRecipientCheckValue(r.Identity, r.Public)}
	}
	if o.secrets == nil {
		return out, nil
	}
	for _, e := range o.secrets.payload.Database {
		if !restorableSecrets[e.Table+"."+e.Column] {
			return sealedRestore{}, fmt.Errorf("secrets.age holds a value for %s.%s, which this Hoserva does not restore", e.Table, e.Column)
		}
		if len(e.Value) == 0 {
			continue
		}
		sealed, err := seal(e.Table+"."+e.Column, e.Value)
		if err != nil {
			return sealedRestore{}, err
		}
		out.values[secretKey{e.Table, e.Column, e.RowID}] = sealed
	}
	// A stack's .env is the plaintext its stacks.env column is sealed from
	// (the same value the restore writes to its .env file), for the stacks
	// the archive has files of.
	for _, e := range o.secrets.StackEnvs() {
		if !slices.Contains(o.archiveStacks, e.Stack) {
			continue
		}
		sealed, err := seal("the .env of stack "+e.Stack, e.Body)
		if err != nil {
			return sealedRestore{}, err
		}
		out.values[secretKey{"stacks", "env", e.Stack}] = sealed
	}
	return out, nil
}

// rowKey is the column that identifies a row of a table with sealed columns:
// id, unless sealedRowKeys names another.
func rowKey(table string) string {
	if k, ok := sealedRowKeys[table]; ok {
		return k
	}
	return "id"
}

// holds reports whether the restore writes k's column of its row, so that
// clearing it and reporting it as cleared would be wrong.
func (r sealedRestore) holds(k secretKey) bool {
	if k.table == "schema_info" && k.column == "backup_passphrase" {
		return r.passphrase != nil
	}
	_, ok := r.values[k]
	return ok
}

// restore writes each sealed value into its own table, column and row of the
// staged database, then the backup passphrase. A value whose row the staged
// database does not have is skipped: the row was removed between the moment
// the archive's database was copied and the moment its secrets were read.
func (r sealedRestore) restore(ctx context.Context, tx *sql.Tx) error {
	keys := make([]secretKey, 0, len(r.values))
	for k := range r.values {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b secretKey) int {
		return strings.Compare(a.table+"."+a.column+"."+a.rowID, b.table+"."+b.column+"."+b.rowID)
	})
	for _, k := range keys {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET %s = ? WHERE CAST(%s AS TEXT) = ?`, k.table, k.column, rowKey(k.table)), r.values[k], k.rowID); err != nil {
			return fmt.Errorf("writing %s.%s (row %s): %w", k.table, k.column, k.rowID, err)
		}
	}
	if r.passphrase != nil {
		res, err := tx.ExecContext(ctx, `UPDATE schema_info SET backup_passphrase = ? WHERE id = 1`, r.passphrase)
		if err != nil {
			return fmt.Errorf("writing the backup passphrase: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("writing the backup passphrase: %w", err)
		}
		if n == 0 {
			// An archive whose installation never touched its settings has no
			// row yet; this is the one the settings service would create.
			if _, err := tx.ExecContext(ctx, `INSERT INTO schema_info (id, installation_id, created_at, backup_passphrase) VALUES (1, ?, ?, ?)`,
				uuid.NewString(), time.Now().UTC().Format(time.RFC3339), r.passphrase); err != nil {
				return fmt.Errorf("writing the backup passphrase: %w", err)
			}
		}
	}
	return nil
}

// recipientNotRestored is the report entry for an archive whose backup
// recipient could not be adopted: this installation keeps its own.
func recipientNotRestored(o SecretsOutcome) NotRestored {
	reason, why := NotRestoredNoSecrets, "the archive has no identity.age"
	switch o.Identity {
	case SecretsNoPassphrase:
		reason, why = NotRestoredNoPassphrase, "no backup passphrase is available to open the archive's identity.age"
	case SecretsPassphraseIncorrect:
		reason, why = NotRestoredPassphraseIncorrect, "the backup passphrase does not open the archive's identity.age"
	}
	return NotRestored{Kind: NotRestoredRecipient, Name: "backup_recipient", Reason: reason,
		Message: fmt.Sprintf("this installation keeps its own backup recipient: %s; archives it writes from now on are encrypted to it, not to the archive's", why)}
}
