package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

const (
	exitOK     = 0
	exitError  = 1
	exitDoctor = 4
)

var jsonOutput bool

func emit(v any) {
	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(v); err != nil {
			fmt.Fprintf(os.Stderr, "hoserva: encoding output: %v\n", err)
			os.Exit(exitError)
		}
		return
	}
	printHuman(v)
}

func printHuman(v any) {
	switch x := v.(type) {
	case string:
		fmt.Println(x)
	default:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(v)
	}
}

// safeText returns s for a terminal's human output: every control character
// (C0, DEL, C1, line breaks and tabs included) and every other character a
// terminal could act on or hide is written as a visible escape the way %q
// writes it, and each byte that is not valid UTF-8 as \xNN. Backslashes stay
// as they are, so the escapes are for reading, not for parsing back. Use it
// for every field a template, a catalog entry or an imported file supplies.
func safeText(s string) string { return escapeText(s, false) }

// safeBlock is safeText for a field that legitimately spans lines, such as a
// Compose file: line feeds and tabs are kept, every other control character
// (a carriage return included) is escaped.
func safeBlock(s string) string { return escapeText(s, true) }

func escapeText(s string, keepBreaks bool) string {
	var sb strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&sb, "\\x%02x", s[i])
		case keepBreaks && (r == '\n' || r == '\t'), r == ' ', unicode.IsPrint(r):
			sb.WriteString(s[i : i+size])
		default:
			q := strconv.QuoteRune(r)
			sb.WriteString(q[1 : len(q)-1])
		}
		i += size
	}
	return sb.String()
}

// privilegeLine is one line of a privilege summary.
func privilegeLine(p apiv1.TemplatePrivilege) string {
	detail := ""
	if d := p.Detail.Or(""); d != "" {
		detail = " " + safeText(d)
	}
	return fmt.Sprintf("  - %s%s (service %s): %s\n", safeText(string(p.Kind)), detail, safeText(p.Service), safeText(p.Description))
}
