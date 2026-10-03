package migrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
)

var (
	// ErrNoPreview is returned when the session's report was made before scans
	// converted templates, so it holds no preview.
	ErrNoPreview = errors.New("this report holds no template conversion preview; scan again")
	// ErrTemplateNotFound is returned for a name that is no template file and
	// no Compose Manager project of the session's report.
	ErrTemplateNotFound = errors.New("there is no template or Compose Manager project with this name in the migration report")
	// ErrSourceUnavailable is returned when a preview cannot be built because the
	// Flash Backup zip the report was made from is not kept: it was removed, or
	// the report was made from the Unraid stick, which nothing is copied from.
	ErrSourceUnavailable = errors.New("the Flash Backup zip this report was made from is not kept on this machine, so the template preview cannot be built; scan a Flash Backup zip again")
	// ErrNoCompose is returned for a Compose Manager project whose compose.yaml
	// is not in the source, which has nothing to preview.
	ErrNoCompose = errors.New("the Compose Manager project's compose.yaml is not in the source, so it has no preview")
)

// TemplateKind says what a preview is of.
type TemplateKind string

const (
	KindTemplate       TemplateKind = "template"
	KindComposeProject TemplateKind = "compose_project"
)

// TemplateList is every template and Compose Manager project of the report,
// with the conversion counts. It holds only what the session keeps: no template
// content.
type TemplateList struct {
	Counts          TemplateCounts
	Templates       []TemplateEntry
	ComposeProjects []ComposeProject
}

// TemplateView is one preview, built from the zip when asked for, with what
// identifies it.
type TemplateView struct {
	Kind TemplateKind
	// Name is the template's file name, or the project's name.
	Name string
	// Entry is the template, for a template.
	Entry *TemplateEntry
	// Project is the Compose Manager project, for a project.
	Project *ComposeProject
	Preview *Preview
}

func (s *Service) previewImport(ctx context.Context) (*Import, error) {
	st, err := s.State(ctx)
	if err != nil {
		return nil, err
	}
	if st.Report == nil {
		return nil, ErrNoReport
	}
	if st.Report.Import.TemplateCounts == nil {
		return nil, ErrNoPreview
	}
	return &st.Report.Import, nil
}

// Templates lists the report's templates and Compose Manager projects with
// their conversion outcome and counts.
func (s *Service) Templates(ctx context.Context) (*TemplateList, error) {
	imp, err := s.previewImport(ctx)
	if err != nil {
		return nil, err
	}
	return &TemplateList{Counts: *imp.TemplateCounts, Templates: imp.Templates, ComposeProjects: imp.ComposeProjects}, nil
}

// Template builds one preview on request: a template by its file name
// (`my-notes.xml`), or a Compose Manager project by its name. The source and the
// generated Compose are never kept in the session; they are converted again from
// the Flash Backup zip the session keeps, with the networks the capture held. A
// session whose zip is gone has no preview, never an old one.
func (s *Service) Template(ctx context.Context, name string) (*TemplateView, error) {
	st, err := s.State(ctx)
	if err != nil {
		return nil, err
	}
	if st.Report == nil {
		return nil, ErrNoReport
	}
	imp := &st.Report.Import
	if imp.TemplateCounts == nil {
		return nil, ErrNoPreview
	}
	if st.Source == nil || isDeviceScan(st.Source.File) || !s.fileExists(st.Source.File) {
		return nil, ErrSourceUnavailable
	}
	var view *TemplateView
	for i := range imp.Templates {
		if e := &imp.Templates[i]; templateKey(*e) == name {
			view = &TemplateView{Kind: KindTemplate, Name: name, Entry: e}
		}
	}
	for i := range imp.ComposeProjects {
		if p := &imp.ComposeProjects[i]; view == nil && p.Name == name {
			if p.Outcome == nil {
				return nil, ErrNoCompose
			}
			view = &TemplateView{Kind: KindComposeProject, Name: name, Project: p}
		}
	}
	if view == nil {
		return nil, ErrTemplateNotFound
	}
	file := ""
	if view.Entry != nil {
		file = view.Entry.File
	} else {
		file = view.Project.File
	}
	src, f, err := OpenZipFile(s.path(st.Source.File))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrSourceUnavailable
	}
	if err != nil {
		return nil, fmt.Errorf("opening the Flash Backup zip: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := src.Read(file)
	if err != nil {
		return nil, fmt.Errorf("reading %s from the Flash Backup zip: %w", file, err)
	}
	if view.Entry != nil {
		view.Preview = previewTemplate(data, networkDefs(imp.Networks))
	} else {
		view.Preview = previewCompose(data)
	}
	return view, nil
}
