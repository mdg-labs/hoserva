package template

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mdg-labs/hoserva/internal/store"
)

// UpdateStatus is what comparing an installed stack with its template's
// source found.
type UpdateStatus string

const (
	// UpdateAvailable: the source lists a higher revision than the stack
	// was installed from.
	UpdateAvailable UpdateStatus = "update_available"
	// UpdateUpToDate: the source lists the installed revision or an older
	// one.
	UpdateUpToDate UpdateStatus = "up_to_date"
	// UpdateNoTemplate: the stack records no template, or no usable
	// revision number, so there is nothing to compare.
	UpdateNoTemplate UpdateStatus = "not_from_template"
	// UpdateSourceRemoved: the source the stack was installed from is no
	// longer one of the catalog's sources.
	UpdateSourceRemoved UpdateStatus = "source_removed"
	// UpdateTemplateRemoved: the source no longer lists the template.
	UpdateTemplateRemoved UpdateStatus = "template_removed"
)

// InstalledStack is what an update check reads of a stack: the template it
// records and the docker-compose.yml it holds. Nothing here is ever written.
type InstalledStack struct {
	Name           string
	Source         string
	TemplateID     string
	Revision       string
	Compose        string
	ManuallyEdited bool
}

// SourceCatalogs resolves a source by the id stacks record it under;
// *Sources is the production implementation.
type SourceCatalogs interface {
	From(ctx context.Context, source string) (Catalog, error)
}

// Update is the result of an update check. Diff is set only for
// UpdateAvailable. The source's badge says how far its newer revision is
// trusted, and ManuallyEdited that the diff is against a Compose file the
// user changed by hand.
type Update struct {
	Status            UpdateStatus
	Source            string
	TemplateID        string
	InstalledRevision int
	AvailableRevision int
	Kind              store.CatalogSourceKind
	Signed            bool
	ManuallyEdited    bool
	Diff              string
}

// CheckUpdate compares the stack with the template in the source it was
// installed from. It only reads: the stack, its files and its row are never
// changed, so a newer revision is only ever offered. A template from a source
// that cannot be reached for reading fails the check with the catalog's
// error, never "up to date".
func CheckUpdate(ctx context.Context, cats SourceCatalogs, st InstalledStack) (Update, error) {
	out := Update{Source: st.Source, TemplateID: st.TemplateID, ManuallyEdited: st.ManuallyEdited}
	installed, err := strconv.Atoi(strings.TrimSpace(st.Revision))
	if st.TemplateID == "" || st.Source == "" || err != nil || installed < 1 {
		out.Status = UpdateNoTemplate
		return out, nil
	}
	out.InstalledRevision = installed

	cat, err := cats.From(ctx, st.Source)
	if errors.Is(err, ErrSourceNotFound) {
		out.Status = UpdateSourceRemoved
		return out, nil
	}
	if err != nil {
		return Update{}, err
	}
	entry, t, err := loadTemplate(ctx, cat, st.TemplateID)
	if errors.Is(err, ErrTemplateNotFound) {
		// A catalog that cannot be listed says nothing about the template.
		if _, ierr := cat.Index(ctx); ierr != nil {
			return Update{}, ierr
		}
		out.Status = UpdateTemplateRemoved
		return out, nil
	}
	if err != nil {
		return Update{}, err
	}
	out.Kind, out.Signed = entry.Kind, entry.Signed
	out.AvailableRevision = t.Block.Revision
	if t.Block.Revision <= installed {
		out.Status = UpdateUpToDate
		return out, nil
	}
	out.Status = UpdateAvailable
	out.Diff = UnifiedDiff(
		fmt.Sprintf("docker-compose.yml (installed, revision %d)", installed),
		fmt.Sprintf("%s (revision %d)", ComposeFile, t.Block.Revision),
		st.Compose, string(entry.Data))
	return out, nil
}
