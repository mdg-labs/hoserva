package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
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
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		out = appendEscapedRune(out, r, size, s[i], keepBreaks)
		i += size
	}
	return string(out)
}

// appendEscapedRune is the one per-character rule of safeText and safeBlock:
// r is what utf8 decoded at a position, size its width and first the byte
// there.
func appendEscapedRune(dst []byte, r rune, size int, first byte, keepBreaks bool) []byte {
	switch {
	case r == utf8.RuneError && size == 1:
		return fmt.Appendf(dst, "\\x%02x", first)
	case keepBreaks && (r == '\n' || r == '\t'), r == ' ', unicode.IsPrint(r):
		return utf8.AppendRune(dst, r)
	default:
		q := strconv.QuoteRune(r)
		return append(dst, q[1:len(q)-1]...)
	}
}

// copyBlock writes what r holds to w the way safeBlock writes a string, one
// chunk at a time, so a large body is never held whole. A character split
// across two reads is decoded whole; bytes that end the input or the failed
// read mid-character are escaped as invalid bytes. A read error is returned
// after everything read before it has been written.
func copyBlock(w io.Writer, r io.Reader) error {
	buf := make([]byte, 32<<10)
	out := make([]byte, 0, 2*len(buf))
	held := 0
	for {
		n, rerr := r.Read(buf[held:])
		held += n
		end := rerr != nil
		out = out[:0]
		i := 0
		for i < held && (end || utf8.FullRune(buf[i:held])) {
			c, size := utf8.DecodeRune(buf[i:held])
			out = appendEscapedRune(out, c, size, buf[i], true)
			i += size
		}
		held = copy(buf, buf[i:held])
		if len(out) > 0 {
			if _, werr := w.Write(out); werr != nil {
				return werr
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// privilegeLine is one line of a privilege summary.
func privilegeLine(p apiv1.TemplatePrivilege) string {
	detail := ""
	if d := p.Detail.Or(""); d != "" {
		detail = " " + safeText(d)
	}
	return fmt.Sprintf("  - %s%s (service %s): %s\n", safeText(string(p.Kind)), detail, safeText(p.Service), safeText(p.Description))
}
