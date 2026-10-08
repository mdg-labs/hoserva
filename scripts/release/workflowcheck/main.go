// Command workflowcheck reads .github/workflows/release.yml and reports
// every way its job layout breaks the rule that the release-signing key is
// held only by a job that runs none of the build's code: the signing job
// runs no package-manager, make, Go-build or packaging step, takes its
// .deb files only from a downloaded artifact, and needs the build job,
// which holds neither the key nor a write token. No job restores a package
// manager cache: the Go and npm caches are written by workflows on the
// default branch that run third-party code, so a release build must not
// take its inputs from them.
package main

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const signingSecret = "secrets.HOSERVA_RELEASE_SIGNING_KEY"

type workflow struct {
	Permissions permissions    `yaml:"permissions"`
	Jobs        map[string]job `yaml:"jobs"`
}

type permissions struct {
	set    bool
	scopes map[string]string
}

func (p *permissions) UnmarshalYAML(n *yaml.Node) error {
	p.set = true
	if n.Kind != yaml.MappingNode {
		p.scopes = map[string]string{"*": n.Value}
		return nil
	}
	return n.Decode(&p.scopes)
}

func (p permissions) writes() []string {
	var out []string
	for scope, level := range p.scopes {
		if level == "write" || level == "write-all" {
			out = append(out, scope)
		}
	}
	return out
}

type job struct {
	Needs       needs       `yaml:"needs"`
	Environment yaml.Node   `yaml:"environment"`
	Permissions permissions `yaml:"permissions"`
	Steps       []step      `yaml:"steps"`
}

type needs []string

func (n *needs) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		*n = needs{node.Value}
		return nil
	}
	var list []string
	if err := node.Decode(&list); err != nil {
		return err
	}
	*n = list
	return nil
}

type step struct {
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]string `yaml:"with"`
}

var forbiddenInSigning = []struct {
	name string
	re   *regexp.Regexp
}{
	{"npm", regexp.MustCompile(`(^|[\s;&|(])(npm|npx)(\s|$)`)},
	{"make", regexp.MustCompile(`(^|[\s;&|(])make(\s|$)`)},
	{"go run", regexp.MustCompile(`(^|[\s;&|(])go\s+(run|build|install|test|generate)(\s|$)`)},
	{"dpkg-buildpackage", regexp.MustCompile(`dpkg-buildpackage`)},
	{"build-deb.sh", regexp.MustCompile(`build-deb\.sh`)},
}

var fetchesElsewhere = regexp.MustCompile(`(^|[\s;&|(])(curl|wget)(\s|$)|gh\s+(run\s+download|release\s+download|api)`)

func code(script string) string {
	var b strings.Builder
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func stripComments(n *yaml.Node) {
	n.HeadComment, n.LineComment, n.FootComment = "", "", ""
	for _, c := range n.Content {
		stripComments(c)
	}
}

func nodeText(n *yaml.Node) string {
	stripComments(n)
	out, err := yaml.Marshal(n)
	if err != nil {
		return ""
	}
	return string(out)
}

func check(data []byte) []string {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return []string{"release.yml is not valid YAML: " + err.Error()}
	}
	var wf workflow
	if err := root.Decode(&wf); err != nil {
		return []string{"release.yml does not have the expected shape: " + err.Error()}
	}

	jobText := map[string]string{}
	if len(root.Content) > 0 {
		top := root.Content[0]
		for i := 0; i+1 < len(top.Content); i += 2 {
			if top.Content[i].Value != "jobs" {
				continue
			}
			jobs := top.Content[i+1]
			for j := 0; j+1 < len(jobs.Content); j += 2 {
				jobText[jobs.Content[j].Value] = nodeText(jobs.Content[j+1])
			}
		}
	}

	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if w := wf.Permissions.writes(); len(w) > 0 {
		add("workflow-level permissions grant write (%s); every job would inherit it", strings.Join(w, ", "))
	}

	var signing []string
	for name := range wf.Jobs {
		if strings.Contains(jobText[name], signingSecret) {
			signing = append(signing, name)
		}
	}
	if len(signing) == 0 {
		add("no job references %s", signingSecret)
	}
	if len(signing) > 1 {
		add("more than one job references %s: %s", signingSecret, strings.Join(signing, ", "))
	}

	names := make([]string, 0, len(wf.Jobs))
	for name := range wf.Jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for i, s := range wf.Jobs[name].Steps {
			switch {
			case strings.HasPrefix(s.Uses, "actions/setup-go@"):
				if s.With["cache"] != "false" {
					add("job %s step %d uses actions/setup-go without cache: false", name, i+1)
				}
			case strings.HasPrefix(s.Uses, "actions/setup-node@"):
				if s.With["package-manager-cache"] != "false" {
					add("job %s step %d uses actions/setup-node without package-manager-cache: false", name, i+1)
				}
				if v, ok := s.With["cache"]; ok {
					add("job %s step %d uses actions/setup-node and sets cache: %s", name, i+1, v)
				}
			}
		}
	}

	signingSet := map[string]bool{}
	for _, name := range signing {
		signingSet[name] = true
		j := wf.Jobs[name]

		for i, s := range j.Steps {
			for _, bad := range forbiddenInSigning {
				if bad.re.MatchString(code(s.Run)) {
					add("job %s step %d holds the signing key and runs %s", name, i+1, bad.name)
				}
			}
			if strings.HasPrefix(s.Uses, "actions/setup-node@") || strings.HasPrefix(s.Uses, "actions/setup-go@") {
				add("job %s step %d holds the signing key and uses %s", name, i+1, strings.SplitN(s.Uses, "@", 2)[0])
			}
			if fetchesElsewhere.MatchString(code(s.Run)) {
				add("job %s step %d fetches files with a tool other than the downloaded artifact", name, i+1)
			}
		}

		var downloadPath string
		downloads := 0
		for _, s := range j.Steps {
			if strings.HasPrefix(s.Uses, "actions/download-artifact@") {
				downloads++
				downloadPath = s.With["path"]
			}
		}
		if downloads != 1 {
			add("job %s must download exactly one artifact with actions/download-artifact, found %d", name, downloads)
		}
		staged := false
		for _, s := range j.Steps {
			if strings.Contains(s.Run, "scripts/release/stage-release-artifacts.sh") && downloadPath != "" && strings.Contains(s.Run, downloadPath) {
				staged = true
			}
		}
		if !staged {
			add("job %s does not take its .deb files from the downloaded artifact through scripts/release/stage-release-artifacts.sh", name)
		}
		if !strings.Contains(nodeText(&j.Environment), "release") {
			add("job %s does not run in the release environment", name)
		}
		if !j.Permissions.set {
			add("job %s has no permissions of its own", name)
		}
		if len(j.Needs) == 0 {
			add("job %s needs no other job", name)
		}
		checkout := false
		for _, s := range j.Steps {
			if strings.HasPrefix(s.Uses, "actions/checkout@") {
				checkout = true
				if s.With["persist-credentials"] != "false" {
					add("job %s checks out without persist-credentials: false", name)
				}
			}
		}
		if !checkout {
			add("job %s has no checkout of its own", name)
		}

		var builders []string
		for _, dep := range j.Needs {
			dj, ok := wf.Jobs[dep]
			if !ok {
				add("job %s needs unknown job %s", name, dep)
				continue
			}
			for _, s := range dj.Steps {
				if strings.Contains(s.Run, "build-deb.sh") {
					builders = append(builders, dep)
					break
				}
			}
		}
		if len(builders) == 0 {
			add("job %s does not need the job that builds the .deb files", name)
		}
	}

	builds := 0
	for name, j := range wf.Jobs {
		if signingSet[name] {
			continue
		}
		runsBuild := false
		for _, s := range j.Steps {
			if strings.Contains(s.Run, "build-deb.sh") {
				runsBuild = true
			}
		}
		if !runsBuild {
			continue
		}
		builds++
		text := jobText[name]
		if strings.Contains(text, "secrets.") {
			add("build job %s references a secret", name)
		}
		if strings.Contains(text, "github.token") {
			add("build job %s is given the workflow token", name)
		}
		if !j.Permissions.set {
			add("build job %s has no permissions of its own", name)
		}
		if w := j.Permissions.writes(); len(w) > 0 {
			add("build job %s has write permission (%s)", name, strings.Join(w, ", "))
		}
		if j.Permissions.set && j.Permissions.scopes["contents"] != "read" {
			add("build job %s does not set contents: read", name)
		}
		if j.Environment.Kind != 0 {
			add("build job %s runs in an environment", name)
		}
		uploads := false
		for _, s := range j.Steps {
			if strings.HasPrefix(s.Uses, "actions/upload-artifact@") {
				uploads = true
			}
		}
		if !uploads {
			add("build job %s does not upload the .deb files with actions/upload-artifact", name)
		}
	}
	if builds != 1 {
		add("expected exactly one job running build-deb.sh outside the signing job, found %d", builds)
	}

	return problems
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: workflowcheck <release.yml>")
		os.Exit(2)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "workflowcheck:", err)
		os.Exit(2)
	}
	problems := check(data)
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, "workflowcheck:", p)
	}
	if len(problems) > 0 {
		os.Exit(1)
	}
}
