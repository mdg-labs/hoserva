package main

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// commentMarker is how the comment workflow finds its own comment again.
	commentMarker = "<!-- hoserva-test-report -->"
	// GitHub rejects a comment over 65,536 characters; this leaves room for
	// the header and the truncation notice.
	maxCommentRunes = 60000
	// The input is the run summary an untrusted job wrote. Anything past this
	// is not read.
	maxSummaryBytes = 2 << 20
)

var (
	runURLPattern = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9._-]+/[A-Za-z0-9._-]+/actions/runs/[0-9]+$`)
	shaPattern    = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	mentionStart  = regexp.MustCompile(`@([A-Za-z0-9_-])`)
	fenceLine     = regexp.MustCompile("^ {0,3}(`{3,})[^`]*$")
)

// neutralizeMentions puts a zero-width joiner after every @ that would start a
// user or team mention, so the comment never notifies anyone: its text comes
// from the pull request's own code.
func neutralizeMentions(s string) string {
	return mentionStart.ReplaceAllString(s, "@\u200d$1")
}

// safePrefix returns the longest prefix of body that is at most limit runes
// and ends at a blank line outside every code fence and <details> block, so a
// cut never leaves one open.
func safePrefix(body string, limit int) (string, bool) {
	if utf8.RuneCountInString(body) <= limit {
		return body, false
	}
	var fence string
	depth, runes, best := 0, 0, 0
	pos := 0
	for pos < len(body) {
		end := strings.IndexByte(body[pos:], '\n')
		var line string
		next := len(body)
		if end >= 0 {
			line = body[pos : pos+end]
			next = pos + end + 1
		} else {
			line = body[pos:]
		}
		runes += utf8.RuneCountInString(body[pos:next])
		if runes > limit {
			break
		}
		switch m := fenceLine.FindStringSubmatch(line); {
		case fence == "" && m != nil:
			fence = m[1]
		case fence != "" && m != nil && len(m[1]) >= len(fence) && strings.TrimSpace(line) == m[1]:
			fence = ""
		case fence == "":
			depth += strings.Count(line, "<details")
			depth -= strings.Count(line, "</details>")
		}
		if fence == "" && depth <= 0 && strings.TrimSpace(line) == "" {
			best = next
		}
		pos = next
	}
	return body[:best], true
}

func renderComment(summary, runURL, sha string, when time.Time) string {
	summary = strings.ToValidUTF8(summary, "�")
	summary = strings.ReplaceAll(summary, "<!--", "&lt;!--")
	summary = neutralizeMentions(summary)

	var head strings.Builder
	head.WriteString(commentMarker + "\n")
	head.WriteString("### Hoserva test summary\n\n")
	fmt.Fprintf(&head, "Latest CI run for commit `%s`: [workflow run](%s), %s\n\n", sha[:min(len(sha), 7)], runURL, when.UTC().Format("2006-01-02 15:04 UTC"))

	notice := fmt.Sprintf("\n**This summary was cut to fit GitHub's comment size limit.** The [full run summary](%s) lists everything.\n", runURL)
	budget := maxCommentRunes - utf8.RuneCountInString(head.String()) - utf8.RuneCountInString(notice)
	body, cut := safePrefix(summary, budget)
	if cut {
		return head.String() + body + notice
	}
	return head.String() + body
}

func runComment(path, runURL, sha, when string, stdout io.Writer) error {
	switch {
	case !runURLPattern.MatchString(runURL):
		return fmt.Errorf("-run-url %q is not a GitHub Actions run URL", runURL)
	case !shaPattern.MatchString(sha):
		return fmt.Errorf("-sha %q is not a lowercase hex commit id", sha)
	}
	t, err := time.Parse(time.RFC3339, when)
	if err != nil {
		return fmt.Errorf("-time: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxSummaryBytes))
	if err != nil {
		return err
	}
	_, err = io.WriteString(stdout, renderComment(string(data), runURL, sha, t))
	return err
}
