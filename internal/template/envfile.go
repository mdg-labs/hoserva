package template

import "strings"

// dotenvValue writes v as the value of a .env line so that Compose's dotenv
// parser (compose-go v2, the one `docker compose` runs) reads it back as v.
// A value of characters that need no quoting is written as it is. Any other
// is double-quoted, the one form in which a trailing backslash, a single
// quote and a line break are all representable: `\` and `"` are escaped, `$`
// is written `\$` so it is never interpolated, and a line break is written
// `\n` or `\r`, which keeps the value on one line. v must not hold a NUL
// byte, which the format has no escape for.
func dotenvValue(v string) string {
	if plainEnvValue.MatchString(v) {
		return v
	}
	var sb strings.Builder
	sb.WriteByte('"')
	for i := 0; i < len(v); i++ {
		switch c := v[i]; c {
		case '\\', '"', '$':
			sb.WriteByte('\\')
			sb.WriteByte(c)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		default:
			sb.WriteByte(c)
		}
	}
	sb.WriteByte('"')
	return sb.String()
}
