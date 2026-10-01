package template

import "context"

// Detail is one catalog template as its page shows it: the metadata, the
// compose.yaml text as the catalog holds it, and the privilege summary.
type Detail struct {
	Source     string
	ID         string
	Revision   int
	Title      string
	Categories []string
	Docs       string
	Compose    string
	Privileges []Privilege
}

// Show reads a template from the catalog and computes its privilege summary
// from its Compose content with each input's default substituted, a secret,
// which has none, with a value of the shape install generates. It fails with
// ErrTemplateNotFound for an unknown id and ErrInvalidTemplate for an entry
// that does not pass the template rules, as installing does.
func Show(ctx context.Context, c Catalog, id string) (*Detail, error) {
	entry, t, err := loadTemplate(ctx, c, id)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string, len(t.Block.Inputs))
	for n, spec := range t.Block.Inputs {
		values[n] = defaultString(spec.Default)
		if spec.Kind == KindSecret {
			values[n] = secretPlaceholder
		}
	}
	return &Detail{
		Source:     entry.Source,
		ID:         t.Block.ID,
		Revision:   t.Block.Revision,
		Title:      t.Block.Title,
		Categories: t.Block.Categories,
		Docs:       t.Block.Docs,
		Compose:    string(entry.Data),
		Privileges: t.Privileges(values),
	}, nil
}
