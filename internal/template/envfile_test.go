package template

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/container"
)

// composeDotenv reads a .env text the way compose-go v2's dotenv parser does
// (dotenv/parser.go: parse, extractVarValue, expandEscapes). The package's own
// parseEnvLine does not follow those rules, so it cannot show what Compose
// reads. Interpolation of what the parser leaves unescaped is interpolate.
func composeDotenv(src string) (map[string]string, error) {
	out := map[string]string{}
	for {
		src = strings.TrimLeftFunc(src, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' })
		if src == "" {
			return out, nil
		}
		eq := strings.IndexAny(src, "=:\n")
		if eq < 0 || src[eq] == '\n' {
			return nil, fmt.Errorf("no value on %q", src)
		}
		key := strings.TrimRight(src[:eq], " \t")
		rest := strings.TrimLeft(src[eq+1:], " \t\r\v\f")
		if rest != "" && (rest[0] == '"' || rest[0] == '\'') {
			quote := rest[0]
			var chars []byte
			escaped, closed := false, false
			var after string
			for i := 1; i < len(rest) && !closed; i++ {
				ch := rest[i]
				switch {
				case ch != quote && !escaped && ch == '\\':
					escaped = true
				case ch != quote:
					if escaped {
						escaped = false
						chars = append(chars, '\\')
					}
					chars = append(chars, ch)
				case escaped:
					escaped = false
					chars = append(chars, ch)
				default:
					value := string(chars)
					if quote == '"' {
						value = interpolate(expandDotenvEscapes(value), out)
					}
					out[key] = value
					after, closed = rest[i+1:], true
				}
			}
			if !closed {
				return nil, fmt.Errorf("unterminated quoted value %q", rest)
			}
			src = after
			continue
		}
		line, after, _ := strings.Cut(rest, "\n")
		line, _, _ = strings.Cut(line, " #")
		out[key] = interpolate(strings.TrimRight(line, " \t\r\v\f"), out)
		src = after
	}
}

var dotenvEscapeRe = regexp.MustCompile(`(\\(?:[abcfnrtv$"\\]|0\d{0,3}))`)

func expandDotenvEscapes(s string) string {
	return dotenvEscapeRe.ReplaceAllStringFunc(s, func(m string) string {
		if m == `\$` {
			return "$$"
		}
		if strings.HasPrefix(m, `\0`) {
			m = strings.Replace(m, `\0`, `\`, 1)
		}
		v, _, _, err := strconv.UnquoteChar(m, '"')
		if err != nil {
			return m
		}
		return string(v)
	})
}

// dotenvValues are the value classes a .env value has to carry: each is one
// the old single-quote form could not write, or that quoting rules treat
// specially.
var dotenvValues = map[string]string{
	"plain":                  "abc123",
	"trailing backslash":     `c:\dir\`,
	"three backslashes":      `a\\\`,
	"two backslashes":        `a\\`,
	"only a backslash":       `\`,
	"backslash then quote":   `a\"b`,
	"backslash then dollar":  `a\$b`,
	"backslash n":            `a\nb`,
	"backslash zero":         `a\0b`,
	"backslash x":            `a\x41b`,
	"single quote":           "it's",
	"trailing single quote":  "pass'",
	"double quote":           `say "hi"`,
	"trailing double quote":  `x"`,
	"both quotes":            `it's "x"`,
	"dollar":                 "pa$word",
	"dollar brace":           "${X}$$",
	"expansion":              "$(id)`id`",
	"reference shaped":       "${DB_PASS}",
	"line break":             "a\nb",
	"carriage return":        "a\rb",
	"crlf":                   "a\r\nb",
	"trailing line break":    "a\n",
	"leading line break":     "\na",
	"line then a definition": "a\nDOCKER_HOST=tcp://evil:1",
	"hash":                   "a #b",
	"leading hash":           "#a",
	"spaces":                 "  two words  inside  ",
	"tab":                    "a\tb",
	"equals":                 "a=b=c",
	"colon":                  "a:b",
	"percent":                "100%",
	"unicode":                "pässwörd-日本",
	"all of it":              "a\\\n'\"$#\r \\",
}

func TestDotenvValue_ComposeReadsEveryClassBackExactly(t *testing.T) {
	for name, value := range dotenvValues {
		t.Run(name, func(t *testing.T) {
			line := "A=" + dotenvValue(value) + "\n"
			if strings.Count(line, "\n") != 1 {
				t.Errorf("the value spreads over several lines: %q", line)
			}
			if names := container.ReservedEnvDefined(line); len(names) > 0 {
				t.Errorf("the line defines %v", names)
			}
			got, err := composeDotenv(line + "B=after\n")
			if err != nil {
				t.Fatalf("Compose cannot read %q: %v", line, err)
			}
			if got["A"] != value {
				t.Errorf("Compose reads %q from %q, want %q", got["A"], line, value)
			}
			if got["B"] != "after" {
				t.Errorf("the line after it reads %q, want it unaffected", got["B"])
			}
		})
	}
}

// The port above is the oracle of every round-trip in this package, so it is
// held to cases checked against compose-go v2.16.1's parser.
func TestComposeDotenvPort_FollowsComposeGoOnTheRulesTheEncoderRelies(t *testing.T) {
	for _, tc := range []struct{ src, want string }{
		{`A="it's \$x"`, "it's $x"},
		{`A="a\nb"`, "a\nb"},
		{`A="c:\\dir\\"`, `c:\dir\`},
		{`A='c:\dir\'`, ``},
		{`A="q\"q"`, `q"q`},
		{`A='it\'s'`, `it's`},
		{`A="a\qb"`, `a\qb`},
	} {
		got, err := composeDotenv(tc.src + "\n")
		if tc.src == `A='c:\dir\'` {
			if err == nil {
				t.Errorf("%s: a trailing backslash before the closing single quote must be an unterminated value, got %v", tc.src, got)
			}
			continue
		}
		if err != nil || got["A"] != tc.want {
			t.Errorf("%s reads %q, %v, want %q", tc.src, got["A"], err, tc.want)
		}
	}
}
