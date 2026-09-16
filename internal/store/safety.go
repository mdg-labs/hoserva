package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// destructiveKind names the fixed classes of statement Destructive
// recognizes, in the order Destructive itself reports them. This is a
// reporting layer over allowlist.go's classifier — it names which
// contract-only shape a table-affecting statement is, for the audit trail
// and for db-migration's own "never generate this automatically" gate; it
// is CheckAllowedStatements, not this file, that actually refuses an
// unregistered one from ever being applied, and it already covers every
// contract-only shape (including DROP INDEX/VIEW/TRIGGER and a contract
// step's own INSERT ... SELECT), not just the three named here.
type destructiveKind string

const (
	destructiveDropTable  destructiveKind = "drop_table"
	destructiveDropColumn destructiveKind = "drop_column"
	destructiveRebuild    destructiveKind = "rebuild"
)

var destructiveOrder = []destructiveKind{destructiveDropTable, destructiveDropColumn, destructiveRebuild}

var destructiveReason = map[destructiveKind]string{
	destructiveDropTable:  "DROP TABLE",
	destructiveDropColumn: "DROP COLUMN",
	destructiveRebuild:    "table rebuild (RENAME TO after CREATE TABLE — SQLite's documented way to change a column's type or constraints)",
}

// Destructive returns every reason sql is flagged as data-unsafe (D16,
// doc 01 §4). sqldef's own --enable-drop-table gate (Q60) only ever checks
// whether a statement contains the literal substring "DROP TABLE"
// (database.RunDDLs, sqldef v1.0.7) — it does not gate DROP COLUMN at all,
// and SQLite has no ALTER COLUMN, so a column type or constraint change is
// expressed, if at all, as a rebuild: a new table, a copy, a DROP TABLE of
// the old one, and a RENAME TO. Hoserva's own scan is what actually
// enforces D16, not sqldef's flag.
//
// A registered contract step is still reported here — the caller decides
// whether the registration excuses it — so the reasons are always
// available for the audit trail even when the check as a whole passes.
func Destructive(sql string) []string {
	found := map[destructiveKind]bool{}
	for _, stmt := range splitStatements(sql) {
		if kind, ok := classifyDestructive(stmt); ok {
			found[kind] = true
		}
	}
	var reasons []string
	for _, kind := range destructiveOrder {
		if found[kind] {
			reasons = append(reasons, destructiveReason[kind])
		}
	}
	return reasons
}

// classifyDestructive names which of the three table-affecting
// contract-only shapes stmt is, driven by the same classifyStatement and
// parseAlterTable allowlist.go uses to decide whether stmt may run at
// all — there is exactly one statement classifier in this package, this
// function only ever asks it "which kind" for a statement it already
// called classContractOnly. A statement classDisallowed here (a shape
// parseAlterTable could not place into one of SQLite's own fixed ALTER
// TABLE forms, for example) is not reported by Destructive: it is never
// permitted to run at all, registered or not, which is a stronger
// guarantee than requiring contract registration for it.
func classifyDestructive(stmt string) (destructiveKind, bool) {
	if classifyStatement(stmt) != classContractOnly {
		return "", false
	}
	tokens := tokenize(stmt)
	switch {
	case isWord(tokens, 0, "drop") && isWord(tokens, 1, "table"):
		return destructiveDropTable, true
	case isWord(tokens, 0, "drop"):
		return "", false // DROP INDEX/VIEW/TRIGGER — reported by CheckAllowedStatements, not here
	case isWord(tokens, 0, "alter"):
		nameEnd := qualifiedNameEnd(tokens, 2)
		if parseAlterTable(tokens, nameEnd) == alterDropColumn {
			return destructiveDropColumn, true
		}
		return destructiveRebuild, true // the only other contract-only ALTER TABLE shape is RENAME TO
	default:
		return "", false // a contract step's own INSERT ... SELECT copy — not a drop
	}
}

// ReadContracts parses the "contracts" file format: one migration filename
// per line, each one a deliberately reviewed contract step of an
// expand/contract change whose data already has a new home (D16). Blank
// lines and lines starting with "#" are ignored, so the file can carry a
// one-line reason next to each entry.
func ReadContracts(data []byte) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		out[fields[0]] = true
	}
	return out
}

// CheckSafety fails on the first migration that carries a destructive
// change and is not a registered contract step.
func CheckSafety(migrations []Migration, contracts map[string]bool) error {
	for _, m := range migrations {
		reasons := Destructive(m.SQL)
		if len(reasons) == 0 {
			continue
		}
		if contracts[m.Filename] {
			continue
		}
		return fmt.Errorf("migration %q contains a destructive change (%s) and is not a registered contract step — add it to internal/store/migrations/%s only after confirming the data it removes already has a new home (D16)", m.Filename, strings.Join(reasons, ", "), ContractsFile)
	}
	return nil
}

// LoadContractsFile reads the contracts file from dir. A missing file
// means no contract steps have been registered yet, which is valid, not
// an error.
func LoadContractsFile(dir string) (map[string]bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, ContractsFile))
	if os.IsNotExist(err) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading contracts file: %w", err)
	}
	return ReadContracts(data), nil
}
