package migrate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/mdg-labs/hoserva/internal/template"
)

// PreviewWarning is one thing the reviewer of a conversion has to know: the
// converter's warning as it was produced.
type PreviewWarning struct {
	Class   string
	Message string
	Detail  string
	Command string
}

// PreviewPrivilege is one thing the generated Compose content asks for beyond
// an ordinary container.
type PreviewPrivilege struct {
	Kind        string
	Service     string
	Detail      string
	Description string
}

// Failure codes: why a template or a project could not be read. Only the code
// is kept in the session, because the converter's and the YAML reader's error
// text can quote the file.
const (
	FailureTooLarge          = "too_large"
	FailureInvalid           = "invalid"
	FailureInvalidYAML       = "invalid_yaml"
	FailureMultipleDocuments = "multiple_documents"
)

// Preview is what a template's conversion, or a Compose Manager project's
// compose.yaml, shows the review: the source as the flash holds it, the Compose
// file, every warning and the privileges its content asks for. Nothing in it
// has been applied anywhere. It holds what the template holds, containers'
// environment included, so it is built on request from the Flash Backup zip and
// never kept: the session keeps only its Outcome.
type Preview struct {
	Source string
	// Compose is the generated Compose file of a converted template, and the
	// project's own compose.yaml for a Compose Manager project. It is empty
	// when the converter failed.
	Compose    string
	Warnings   []PreviewWarning
	Privileges []PreviewPrivilege
	// Error says why the converter, or the YAML reader of a project, could not
	// produce a preview, and Failure is its code.
	Error   string
	Failure string
}

// PreviewStatus is how a template's conversion reads.
type PreviewStatus string

const (
	// PreviewClean is a conversion with no warning that needs manual action
	// (Q36).
	PreviewClean PreviewStatus = "clean"
	// PreviewWarnings is a conversion with at least one.
	PreviewWarnings PreviewStatus = "warnings"
	// PreviewFailed is a template or project that could not be read.
	PreviewFailed PreviewStatus = "failed"
	// PreviewOnly is a Compose Manager project's compose.yaml, which is read and
	// never converted.
	PreviewOnly PreviewStatus = "previewed"
)

// Outcome is what the session keeps of a preview: how it read, the codes of its
// warnings and, for a failure, why. It holds no template content, no setting's
// value and no warning text, because the converter's messages quote settings.
type Outcome struct {
	Status PreviewStatus `json:"status"`
	// Warnings are the classes of every warning, in the converter's order.
	Warnings []string `json:"warnings,omitempty"`
	Failure  string   `json:"failure,omitempty"`
}

func actionClass(class string) bool {
	return class != template.WarnWritableLayer && class != template.WarnNote
}

// ActionWarnings is the number of warnings that make a conversion not clean
// (Q36): the writable-layer warning every conversion carries and the
// informational notes do not count.
func (o *Outcome) ActionWarnings() int {
	n := 0
	for _, c := range o.Warnings {
		if actionClass(c) {
			n++
		}
	}
	return n
}

// FailureText says why the file could not be read, in words.
func (o *Outcome) FailureText() string {
	switch o.Failure {
	case FailureTooLarge:
		return fmt.Sprintf("it is larger than %d bytes", template.MaxUnraidTemplateBytes)
	case FailureInvalid:
		return "it is not a template the converter can read"
	case FailureInvalidYAML:
		return "compose.yaml is not valid YAML"
	case FailureMultipleDocuments:
		return "compose.yaml holds more than one YAML document"
	}
	return ""
}

// Outcome is the part of the preview the session keeps.
func (p *Preview) Outcome(project bool) *Outcome {
	o := &Outcome{Failure: p.Failure}
	for _, w := range p.Warnings {
		o.Warnings = append(o.Warnings, w.Class)
	}
	switch {
	case p.Failure != "":
		o.Status = PreviewFailed
	case project:
		o.Status = PreviewOnly
	case o.ActionWarnings() > 0:
		o.Status = PreviewWarnings
	default:
		o.Status = PreviewClean
	}
	return o
}

// TemplateCounts are the conversion numbers of the report. Clean, WithWarnings
// and Failed cover the templates that had a container on the source server
// (autostart, running and stopped); a template with no container is converted
// for its preview and counted in TemplateOnly. Without the capture's container
// list nothing says which are installed, and the three counts cover every
// template (AllTemplates).
type TemplateCounts struct {
	Clean        int  `json:"clean"`
	WithWarnings int  `json:"withWarnings"`
	Failed       int  `json:"failed"`
	TemplateOnly int  `json:"templateOnly"`
	AllTemplates bool `json:"allTemplates"`
	// ComposeProjects are the Compose Manager projects whose compose.yaml is
	// previewed. They are not converted and not in the three counts above.
	ComposeProjects int `json:"composeProjects"`
}

// Counted reports whether the template is in the report's conversion counts.
func (e TemplateEntry) Counted() bool { return e.Class != ClassTemplateOnly }

// networkDefs turns the capture's networks into the converter's network input,
// so a template on a custom network gets the command that creates it exactly.
// Docker's own three networks are not user-defined.
func networkDefs(nets []Network) []template.NetworkDef {
	var out []template.NetworkDef
	for _, n := range nets {
		if builtinNetwork(n.Name) {
			continue
		}
		def := template.NetworkDef{Name: n.Name, Driver: n.Driver, Parent: n.Options["parent"], Options: n.Options}
		if len(n.Subnets) > 0 {
			def.Subnet, def.Gateway, def.IPRange = n.Subnets[0].Subnet, n.Subnets[0].Gateway, n.Subnets[0].IPRange
		}
		out = append(out, def)
	}
	return out
}

// previewTemplate converts one template in memory. It creates nothing and runs
// nothing.
func previewTemplate(data []byte, networks []template.NetworkDef) *Preview {
	conv, err := template.ConvertUnraid(data, template.ConvertOptions{Networks: networks})
	if err != nil {
		failure := FailureInvalid
		if len(data) > template.MaxUnraidTemplateBytes {
			failure = FailureTooLarge
		}
		return &Preview{Source: string(data), Error: err.Error(), Failure: failure}
	}
	p := &Preview{Source: conv.Source, Compose: conv.Compose}
	for _, w := range conv.Warnings {
		p.Warnings = append(p.Warnings, PreviewWarning{Class: w.Class, Message: w.Message, Detail: w.Detail, Command: w.Command})
	}
	for _, pr := range conv.Privileges {
		p.Privileges = append(p.Privileges, PreviewPrivilege{Kind: pr.Kind, Service: pr.Service, Detail: pr.Detail, Description: pr.Description})
	}
	return p
}

// previewCompose reads a Compose Manager project's compose.yaml for the same
// review as a converted stack: its privileges are computed from the file's own
// content. The file is not converted, and a second YAML document is refused
// because Compose merges every document, so one could add what the summary
// never sees.
func previewCompose(data []byte) *Preview {
	p := &Preview{Source: string(data), Compose: string(data)}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var compose map[string]any
	if err := dec.Decode(&compose); err != nil && !errors.Is(err, io.EOF) {
		p.Compose, p.Failure, p.Error = "", FailureInvalidYAML, fmt.Sprintf("compose.yaml is not valid YAML: %v", err)
		return p
	}
	var next any
	if err := dec.Decode(&next); !errors.Is(err, io.EOF) {
		p.Compose, p.Failure, p.Error = "", FailureMultipleDocuments, "compose.yaml holds more than one YAML document"
		return p
	}
	for _, pr := range (&template.Template{Compose: compose}).Privileges(nil) {
		p.Privileges = append(p.Privileges, PreviewPrivilege{Kind: pr.Kind, Service: pr.Service, Detail: pr.Detail, Description: pr.Description})
	}
	return p
}

// countPreviews fills in the report's conversion counts from the outcomes.
func countPreviews(imp *Import, haveContainers bool) {
	c := &TemplateCounts{AllTemplates: !haveContainers}
	for _, e := range imp.Templates {
		switch {
		case !e.Counted():
			c.TemplateOnly++
		case e.Outcome.Status == PreviewClean:
			c.Clean++
		case e.Outcome.Status == PreviewWarnings:
			c.WithWarnings++
		default:
			c.Failed++
		}
	}
	for _, p := range imp.ComposeProjects {
		if p.Outcome != nil && p.Outcome.Status == PreviewOnly {
			c.ComposeProjects++
		}
	}
	imp.TemplateCounts = c
}

// reportPreviews adds the conversion rows to the report: the counts, each
// counted template that has warnings or could not convert, and each Compose
// Manager project whose compose.yaml could not be read. A row names and counts;
// it never quotes the template.
func reportPreviews(r *Report, imp *Import) {
	c := imp.TemplateCounts
	if len(imp.Templates) > 0 {
		counted := c.Clean + c.WithWarnings + c.Failed
		scope := fmt.Sprintf("Of the %d installed %s", counted, plural(counted, "template", "templates"))
		if c.AllTemplates {
			scope = fmt.Sprintf("Of all %d %s", counted, plural(counted, "template", "templates"))
		}
		msg := fmt.Sprintf("%s, %d convert cleanly and %d with warnings (Q36)", scope, c.Clean, c.WithWarnings)
		if c.Failed > 0 {
			msg += fmt.Sprintf(", and %d could not be converted", c.Failed)
		}
		msg += "."
		if c.AllTemplates {
			msg += " Without the container list nothing says which templates are installed, so these counts cover every template."
		}
		if c.TemplateOnly > 0 {
			msg += fmt.Sprintf(" %d template-only %s converted for the preview and %s not counted.", c.TemplateOnly, plural(c.TemplateOnly, "template is", "templates are"), plural(c.TemplateOnly, "is", "are"))
		}
		r.add(CheckTemplates, StatusInfo, "", "%s", msg)
	}
	for _, e := range imp.Templates {
		if !e.Counted() {
			continue
		}
		o := e.Outcome
		switch o.Status {
		case PreviewWarnings:
			r.add(CheckTemplates, StatusInfo, e.Name, "Converts with %d %s to review (%s). Open its preview before recreating the container.",
				o.ActionWarnings(), plural(o.ActionWarnings(), "warning", "warnings"), joinNames(actionClasses(o)))
		case PreviewFailed:
			r.add(CheckTemplates, StatusWarn, e.Name, "The converter could not read the template (%s). It cannot be converted.", o.FailureText())
		}
	}
	if c.ComposeProjects > 0 {
		r.add(CheckContainers, StatusInfo, "", "%d Compose Manager %s previewed with %s own compose.yaml and not converted.", c.ComposeProjects, plural(c.ComposeProjects, "project", "projects"), plural(c.ComposeProjects, "its", "their"))
	}
	for _, pr := range imp.ComposeProjects {
		if pr.Outcome != nil && pr.Outcome.Status == PreviewFailed {
			r.add(CheckContainers, StatusWarn, pr.Name, "The project's %s.", pr.Outcome.FailureText())
		}
	}
}

func actionClasses(o *Outcome) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range o.Warnings {
		if actionClass(c) && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// templateKey is the name a template is looked up by: the file's name, which
// is unique where the container's <Name> may not be.
func templateKey(e TemplateEntry) string {
	return path.Base(e.File)
}
