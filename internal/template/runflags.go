package template

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// shellWords splits a command-line fragment into words the way a POSIX shell
// would, for the part of the syntax an ExtraParams string uses: whitespace,
// single quotes, double quotes and backslash escapes. Unraid runs the string
// through a shell, so anything the shell would act on beyond that (command
// separators, redirections, expansions, globs) has no structured meaning
// here: the whole string is refused instead of read some other way.
func shellWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	flush := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case ' ', '\t', '\n', '\r':
			flush()
		case '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, fmt.Errorf("an unterminated single quote")
			}
			cur.WriteString(s[i+1 : i+1+end])
			i += end + 1
			inWord = true
		case '"':
			inWord = true
			i++
		quoted:
			for {
				if i >= len(s) {
					return nil, fmt.Errorf("an unterminated double quote")
				}
				switch ch := s[i]; ch {
				case '"':
					break quoted
				case '$', '`':
					return nil, fmt.Errorf("a shell expansion (%c) inside double quotes", ch)
				case '\\':
					if i+1 < len(s) && strings.IndexByte("$`\"\\\n", s[i+1]) >= 0 {
						if s[i+1] != '\n' {
							cur.WriteByte(s[i+1])
						}
						i += 2
						continue
					}
					cur.WriteByte(ch)
					i++
				default:
					cur.WriteByte(ch)
					i++
				}
			}
		case '\\':
			if i+1 >= len(s) {
				return nil, fmt.Errorf("a trailing backslash")
			}
			if s[i+1] != '\n' {
				cur.WriteByte(s[i+1])
				inWord = true
			}
			i++
		case ';', '&', '|', '<', '>', '(', ')', '`', '$', '*', '?', '[', '{':
			return nil, fmt.Errorf("the shell syntax %q", string(c))
		case '#', '~':
			if !inWord {
				return nil, fmt.Errorf("the shell syntax %q", string(c))
			}
			cur.WriteByte(c)
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	flush()
	return words, nil
}

var plainWord = regexp.MustCompile(`^[A-Za-z0-9_./:=@%+,-]+$`)

// showWords renders words on one line for a comment or a message. A word
// that is not plain is written quoted with its control characters escaped,
// so no text from a template can end the line it is shown on.
func showWords(words ...string) string {
	out := make([]string, len(words))
	for i, w := range words {
		if plainWord.MatchString(w) {
			out[i] = w
		} else {
			out[i] = strconv.Quote(w)
		}
	}
	return strings.Join(out, " ")
}

// runFlag is one flag of a `docker run` command line after parsing, resolved
// to its long form where it has one.
type runFlag struct {
	// Name is the long form for a flag the table knows, and the flag as
	// written for one it does not.
	Name     string
	Value    string
	HasValue bool
	// Raw is the words the flag was read from, shown when it cannot be
	// translated.
	Raw string
	// Missing is set on a flag that takes a value and was given none.
	Missing bool
	// Known is set on a flag that appears in the translate table.
	Known bool
}

// shortRunFlags resolves the short forms of doc 04 §5 to their long form.
var shortRunFlags = map[byte]string{
	'v': "--volume",
	'p': "--publish",
	'e': "--env",
	'u': "--user",
	'h': "--hostname",
	'm': "--memory",
	'w': "--workdir",
	'i': "--interactive",
	't': "--tty",
}

// parseRunFlags reads the words of a docker run argument list into flags. A
// flag that takes a value takes the next word as it, as docker does, and
// `--flag=value` is the same flag. A short flag cluster such as -it splits
// into its flags, and -m512m, -m=512m and -m 512m are one flag. A word that
// is not a flag, and a flag the table does not know, is returned as an
// unknown flag: the caller never drops either.
func parseRunFlags(words []string, table map[string]runFlagSpec) []runFlag {
	var out []runFlag
	for i := 0; i < len(words); i++ {
		w := words[i]
		switch {
		case w == "--":
			out = append(out, runFlag{Name: "--", Raw: showWords(words[i:]...)})
			i = len(words)
		case strings.HasPrefix(w, "--"):
			name, value, hasValue := strings.Cut(w, "=")
			spec, known := table[name]
			f := runFlag{Name: name, Value: value, HasValue: hasValue, Raw: showWords(w), Known: known}
			switch {
			case known && spec.takesValue && !hasValue:
				if i+1 < len(words) {
					f.Value, f.HasValue = words[i+1], true
					f.Raw = showWords(w, words[i+1])
					i++
				} else {
					f.Missing = true
				}
			case !known && !hasValue && i+1 < len(words) && !strings.HasPrefix(words[i+1], "-"):
				f.Raw = showWords(w, words[i+1])
				i++
			}
			out = append(out, f)
		case strings.HasPrefix(w, "-") && len(w) > 1:
			body := w[1:]
			for j := 0; j < len(body); j++ {
				long, ok := shortRunFlags[body[j]]
				if !ok {
					f := runFlag{Name: "-" + body[j:], Raw: showWords("-" + body[j:])}
					if j == 0 && i+1 < len(words) && !strings.Contains(w, "=") && !strings.HasPrefix(words[i+1], "-") {
						f.Raw = showWords(w, words[i+1])
						i++
					}
					out = append(out, f)
					break
				}
				spec := table[long]
				if !spec.takesValue {
					out = append(out, runFlag{Name: long, Raw: showWords("-" + string(body[j])), Known: true})
					continue
				}
				f := runFlag{Name: long, Known: true}
				rest := strings.TrimPrefix(body[j+1:], "=")
				switch {
				case rest != "":
					f.Value, f.HasValue = rest, true
					f.Raw = showWords(w)
				case strings.HasPrefix(body[j+1:], "="):
					f.HasValue = true
					f.Raw = showWords(w)
				case i+1 < len(words):
					f.Value, f.HasValue = words[i+1], true
					f.Raw = showWords(w, words[i+1])
					i++
				default:
					f.Missing = true
					f.Raw = showWords(w)
				}
				out = append(out, f)
				break
			}
		default:
			out = append(out, runFlag{Name: w, Raw: showWords(w)})
		}
	}
	return out
}
