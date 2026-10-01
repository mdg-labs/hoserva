// Command catalog-verify checks a catalog archive and its detached signature
// against the catalog public key compiled into hoservad
// (internal/template.CatalogPublicKey), and that the archive's own serial is
// the one named. scripts/devenv/catalog-snapshot.sh runs it, so the build
// checks the snapshot with exactly the code the daemon checks it with at
// start. Usage: catalog-verify <archive> <signature> <serial>.
package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/mdg-labs/hoserva/internal/template"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "catalog-verify:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: catalog-verify <archive> <signature> <serial>")
	}
	want, err := strconv.ParseInt(args[2], 10, 64)
	if err != nil {
		return fmt.Errorf("serial %q: %w", args[2], err)
	}
	archive, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(args[1])
	if err != nil {
		return err
	}
	serial, err := template.VerifyArchive(template.CatalogPublicKey, archive, sig)
	if err != nil {
		return err
	}
	if serial != want {
		return fmt.Errorf("the archive's serial is %d, the pin names %d", serial, want)
	}
	return nil
}
