package store

import "strings"

// This file is a small, general SQL tokenizer, deliberately not a set of
// clause-by-clause text extractors (findParenAfter("CHECK"),
// findCollateAfter, ...): that style of scan can only ever catch the
// clauses it was written to look for, and SQLite's grammar has too many
// of them — ON CONFLICT, DEFERRABLE, AUTOINCREMENT, a partial or
// expression index, a second CHECK on the same column, a CHECK inside a
// comment, and more — to enumerate one at a time and stay complete.
// Tokenizing turns a whole CREATE TABLE/INDEX/TRIGGER/VIEW statement into
// a canonical stream where comments and whitespace are gone, every
// keyword and unquoted or
// backtick-/bracket-quoted identifier is ASCII-case-folded the same way,
// and every string literal (including the two double-quoting/DEFAULT-
// bareword cases tokenize's own doc comment explains, where SQLite itself
// can't be told apart from an identifier without a schema) keeps its exact
// content. Comparing that whole stream, per stored object, catches every
// clause expressed in the statement's own text — it is not a claim that
// nothing can slip past a tokenizer this small; unusual identifier
// characters and unrecognized statement shapes are their own, separately
// tracked gaps (Q60).

// tokenKind classifies one sqlToken for the two things splitStatements and
// leadingWords need to tell apart from a plain identifier/keyword: a string
// literal (opaque data, never a keyword) and punctuation (never part of a
// leading keyword sequence).
type tokenKind int

const (
	tokWord tokenKind = iota
	tokString
	tokNumber
	tokPunct
)

// sqlToken is one lexical element of a canonicalized SQL statement. quoted
// marks a tokWord that came from any quoted source token (double-,
// backtick- or bracket-quoted): precededByDefault must never treat one as
// the bare DEFAULT keyword even when its folded content happens to read
// "default".
type sqlToken struct {
	kind   tokenKind
	text   string
	quoted bool
}

// isIdentChar and isIdentStart follow SQLite's own lexer
// (src/tokenize.c's IdChar macro): besides ASCII letters, digits and
// underscore, '$' and every byte >= 0x80 (a UTF-8 continuation or lead
// byte) are identifier characters too, at every position — SQLite draws
// no distinction between a start and a continuation position here. A
// table or column name like t$1 or tä would otherwise split into an
// identifier plus leftover characters read as something else entirely,
// and an identifier like écase — a bare word that merely ends in the
// ASCII letters "case" — would be scanned byte-by-byte outside any
// identifier, exposing "case" as if it were the CASE keyword.
func isIdentChar(b byte) bool {
	return b == '_' || b == '$' || b >= 0x80 ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func isIdentStart(b byte) bool {
	return b == '_' || b == '$' || b >= 0x80 || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// asciiLower folds only ASCII letters. SQLite itself
// only ever folds ASCII case when resolving an unquoted keyword or
// identifier — strings.ToLower is Unicode-aware and would fold, for
// example, "İ" or fold "Ä" onto "ä" differently than SQLite does, which
// would hide a real difference as often as it would correctly ignore a
// formatting one.
func asciiLower(s string) string {
	b := []byte(s)
	changed := false
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
			changed = true
		}
	}
	if !changed {
		return s
	}
	return string(b)
}

func isSpace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	default:
		return false
	}
}

// scanQuoted reads a quoted region starting at s[i] (s[i] is the opening
// quote character itself), honoring SQL's doubled-quote escape (”/""/“)
// inside '...'/"..."/`...`, and returns its unescaped content plus the
// index just past the closing quote.
func scanQuoted(s string, i int, quote byte) (content string, next int) {
	var b strings.Builder
	i++
	for i < len(s) {
		if s[i] == quote {
			if i+1 < len(s) && s[i+1] == quote {
				b.WriteByte(quote)
				i += 2
				continue
			}
			return b.String(), i + 1
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String(), i
}

// scanBracket reads a bracket-quoted identifier starting at s[i] == '['.
// SQLite/T-SQL-style bracket quoting has no internal escape.
func scanBracket(s string, i int) (content string, next int) {
	start := i + 1
	j := start
	for j < len(s) && s[j] != ']' {
		j++
	}
	content = s[start:j]
	if j < len(s) {
		j++
	}
	return content, j
}

var multiCharOps = []string{"<=", ">=", "<>", "!=", "||", "<<", ">>"}

func scanOperator(s string, i int) (string, int) {
	for _, op := range multiCharOps {
		if strings.HasPrefix(s[i:], op) {
			return op, i + len(op)
		}
	}
	return s[i : i+1], i + 1
}

// scanNumber reads a numeric literal (decimal, with an optional fractional
// part and exponent, or a 0x hex literal) starting at s[i].
func scanNumber(s string, i int) (text string, next int) {
	start := i
	if s[i] == '0' && i+1 < len(s) && (s[i+1] == 'x' || s[i+1] == 'X') {
		i += 2
		for i < len(s) && (isDigit(s[i]) || (s[i] >= 'a' && s[i] <= 'f') || (s[i] >= 'A' && s[i] <= 'F')) {
			i++
		}
		return s[start:i], i
	}
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && isDigit(s[i]) {
			i++
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if j < len(s) && isDigit(s[j]) {
			i = j
			for i < len(s) && isDigit(s[i]) {
				i++
			}
		}
	}
	return s[start:i], i
}

// quoteLiteral re-quotes a string literal's exact content in one canonical
// form, regardless of how the source text happened to escape it.
func quoteLiteral(content string) string {
	return "'" + strings.ReplaceAll(content, "'", "''") + "'"
}

// tokenize turns sql into its canonical token stream: comments (`--` to end
// of line, `/* ... */`, not nested) and whitespace are dropped entirely;
// every keyword and backtick- or bracket-quoted identifier is ASCII-case-
// folded, since SQLite resolves those exactly like a bare identifier and
// quoting style carries no meaning of its own; every single-quoted string
// literal keeps its exact, case-sensitive content but is re-quoted in one
// canonical form. Two exceptions are deliberate, not oversights: a
// double-quoted token's content is kept exactly as written,
// never folded, because SQLite treats "..." as an identifier only if one by
// that name exists and falls back to a string literal otherwise — this
// tokenizer has no schema to resolve that against, and folding a token that
// might be string data would hide a real difference; and the single bare
// word immediately following a DEFAULT keyword is kept exactly as written
// too, because SQLite accepts an unquoted word there and treats it as a
// string literal, not an identifier. For this one narrow ambiguity —
// SQLite itself giving the same bare text two different possible
// meanings — both exceptions are biased toward a false positive (two
// schemas that really are identical reported as drift) rather than a
// false negative. That bias is unrelated to, and no substitute for,
// canonicalize's own separate handling of whether a quoted word ever
// compares equal to an unquoted one (see its own doc comment below).
func tokenize(sql string) []sqlToken {
	var out []sqlToken
	i, n := 0, len(sql)
	for i < n {
		c := sql[i]
		switch {
		case isSpace(c):
			i++
		case c == '-' && i+1 < n && sql[i+1] == '-':
			i += 2
			for i < n && sql[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && sql[i+1] == '*':
			if end := strings.Index(sql[i+2:], "*/"); end >= 0 {
				i += 2 + end + 2
			} else {
				i = n
			}
		case c == '\'':
			content, next := scanQuoted(sql, i, '\'')
			out = append(out, sqlToken{kind: tokString, text: quoteLiteral(content)})
			i = next
		case c == '"':
			content, next := scanQuoted(sql, i, '"')
			out = append(out, sqlToken{kind: tokWord, text: content, quoted: true})
			i = next
		case c == '`':
			content, next := scanQuoted(sql, i, '`')
			out = append(out, sqlToken{kind: tokWord, text: asciiLower(content), quoted: true})
			i = next
		case c == '[':
			content, next := scanBracket(sql, i)
			out = append(out, sqlToken{kind: tokWord, text: asciiLower(content), quoted: true})
			i = next
		case isDigit(c) || (c == '.' && i+1 < n && isDigit(sql[i+1])):
			text, next := scanNumber(sql, i)
			out = append(out, sqlToken{kind: tokNumber, text: asciiLower(text)})
			i = next
		case isIdentStart(c):
			j := i
			for j < n && isIdentChar(sql[j]) {
				j++
			}
			raw := sql[i:j]
			text := asciiLower(raw)
			if precededByDefault(out) && !isDefaultLiteralKeyword(text) {
				text = raw
			}
			out = append(out, sqlToken{kind: tokWord, text: text})
			i = j
		default:
			text, next := scanOperator(sql, i)
			out = append(out, sqlToken{kind: tokPunct, text: text})
			i = next
		}
	}
	return out
}

// precededByDefault reports whether the token stream built so far ends
// right on a bare, unquoted DEFAULT keyword — the one position where
// SQLite accepts a bare, unquoted word and treats it as a string literal
// rather than an identifier. The exception applies only after DEFAULT was
// itself written unquoted: a double-quoted "default" is quoted data or a
// quoted identifier, never the keyword, so it must not turn the word that
// happens to follow it into a bareword literal too.
func precededByDefault(out []sqlToken) bool {
	if len(out) == 0 {
		return false
	}
	last := out[len(out)-1]
	return last.kind == tokWord && !last.quoted && last.text == "default"
}

// isDefaultLiteralKeyword is SQLite's whole literal-value keyword set
// (NULL, TRUE, FALSE, CURRENT_TIME, CURRENT_DATE, CURRENT_TIMESTAMP) — the
// only bare words the grammar itself resolves as a keyword/literal in an
// expression position rather than as a column-name identifier. Two
// separate callers need exactly this set: tokenize's own DEFAULT-bareword
// exception (below) uses it to decide whether the word right after a bare
// DEFAULT still folds normally, as itself a keyword, rather than being
// kept case-exact as an implicit string; and canonicalize uses it to know
// which bare words a quoted token of the same folded text can never mean
// the same thing as — SQLite gives `DEFAULT NULL`'s bare NULL and
// `DEFAULT "null"`'s quoted string/identifier fallback two genuinely
// different values (the same is true of TRUE/FALSE/CURRENT_*, wherever
// they appear bare, not only after DEFAULT — a partial index's own
// `WHERE ... IS NULL`, for example).
func isDefaultLiteralKeyword(word string) bool {
	switch word {
	case "null", "true", "false", "current_time", "current_date", "current_timestamp":
		return true
	default:
		return false
	}
}

// canonicalize renders sql's whole token stream as one comparable string —
// every token's canonical text, joined by a single space, so that two
// statements with the same token sequence render to the same string.
//
// Every tokWord, quoted or not, is re-quoted with its own quote marks
// here, never joined bare: a double-quoted token's content can itself
// contain spaces or words that read like other tokens (`"id integer"` is
// one column named `id integer`), and joining it bare, the same separator
// used between every other pair of tokens, would make it textually
// indistinguishable from that many separate bare-word tokens —
// `CREATE TABLE t ("id integer" TEXT)` and `CREATE TABLE t (id integer TEXT)`
// would canonicalize to the same string despite naming a different
// column. Quoting every word this same way, not only the ones that came
// from a double-quoted source token, is what then makes a schema.sql
// column declared `"key" TEXT` compare equal to the exact same column as
// sqldef's generated ADD COLUMN writes it, backtick-quoted and folded to
// key text: both become the token word "key", and both are now quoted
// alike in the canonical form, so the two only ever differ when
// their folded content actually differs. A double-quoted token's own
// content is still never folded (see tokenize's own doc comment on why),
// so a real case difference in one keeps comparing unequal.
//
// One deliberate exception: a bare, unquoted word whose folded text is
// one of isDefaultLiteralKeyword's set is rendered with a leading NUL
// byte instead of quote marks — a byte that can never open any other
// token's own rendering (a quoted word always starts with `"`, a string
// literal always starts with `'`, punctuation and numbers render as
// their own literal text) — so it never compares equal to a quoted token
// with the same folded text. This is the one case where "quoted or not"
// would otherwise be comparing two genuinely different SQL values as if
// they were the same spelling of the same one. Every other bare word (an
// ordinary identifier, a type name, a structural keyword like CREATE or
// PRIMARY, or the DEFAULT-bareword exception's own case-preserved
// implicit string) keeps comparing equal to its quoted form, which is
// exactly the equivalence the previous paragraph depends on.
func canonicalize(sql string) string {
	return canonicalizeTokens(tokenize(sql), true)
}

// canonicalizeBody renders sql the same way canonicalize does, except a
// double-quoted word never compares equal to a bare word with the same
// folded text. SQLite does not resolve names inside a trigger's or a
// view's own body at creation time the way it resolves a column
// definition: a bare identifier that matches no column fails outright when
// the trigger fires or the view is read, while the exact same word
// double-quoted instead falls back to a string literal — two genuinely
// different things, never interchangeable spellings of the same one the
// way a column definition's own quoting style is (canonicalize's own doc
// comment). Column definitions never appear inside either body — there is
// no ALTER TRIGGER/VIEW ADD COLUMN — so this never touches the equivalence
// canonicalize itself depends on for ADD COLUMN's re-splicing.
func canonicalizeBody(sql string) string {
	return canonicalizeTokens(tokenize(sql), false)
}

// canonicalizeTokens renders tokens as one comparable string, quoting every
// tokWord alike (so a quoted word's content, which can itself contain
// spaces or other-token-shaped text, never merges bare with its
// neighbors — see canonicalize's own doc comment) when unifyQuoting is
// true, and leaving a bare word bare, distinct from any quoted word with
// the same text, when it is false. The literal-keyword marker
// (isDefaultLiteralKeyword) exists only to keep a bare NULL/TRUE/FALSE/
// CURRENT_* apart from its quoted spelling under unifyQuoting: when
// unifyQuoting is false, a bare word already renders differently from a
// quoted one with no marker needed.
func canonicalizeTokens(tokens []sqlToken, unifyQuoting bool) string {
	parts := make([]string, len(tokens))
	for i, t := range tokens {
		switch {
		case t.kind != tokWord:
			parts[i] = t.text
		case !t.quoted && unifyQuoting && isDefaultLiteralKeyword(t.text):
			parts[i] = "\x00" + t.text
		case t.quoted || unifyQuoting:
			parts[i] = `"` + strings.ReplaceAll(t.text, `"`, `""`) + `"`
		default:
			parts[i] = t.text
		}
	}
	return strings.Join(parts, " ")
}

// splitStatements splits sql into its top-level statements — text between
// semicolons that sit outside any quoted region, comment, or a CREATE
// TRIGGER body's own BEGIN...END block (SQLite allows several statements
// separated by ';' inside one, which are no more "separate top-level
// statements" than a CHECK expression's own parentheses are). It does not
// track parenthesis depth at all: a raw ';' character can only appear as
// a statement terminator, inside a quoted region, or inside a comment —
// SQL expression syntax (a column list, CHECK, DEFAULT, an index's
// column or WHERE expression) never contains one — and every quoted
// region and every comment is already consumed whole, parentheses and
// all, by the cases above before this switch ever reaches a ';' or a
// paren character on its own. Empty statements (a trailing or doubled
// ';') are dropped.
//
// BEGIN and END are both ordinary, unquoted identifiers in SQLite — a
// table can have a column literally named begin or end (confirmed
// directly: `CREATE TABLE a (begin INTEGER)` and `CREATE TABLE a (end
// INTEGER)` both parse). Counting every bare "begin" and "end" anywhere
// would be wrong in both directions: an identifier named begin outside a
// trigger would inflate beginDepth and could swallow a later COMMIT into
// the same "statement"; a CASE...END inside a trigger body would close the
// block early and leave the trigger's own END mistaken for a new,
// unbalanced one. Neither is hypothetical — both are exactly what a
// generated migration or a perfectly ordinary schema can contain.
//
// This tracks BEGIN...END only for a statement that has already been
// identified, by its own leading words, as a CREATE [TEMP|TEMPORARY]
// TRIGGER — nothing else ever opens a block — and once inside one, pairs
// CASE with END using the same depth counter, since a trigger body's own
// CASE...END is the only other construct that needs a bare END not to
// close the surrounding block. A bare begin/end/case anywhere else (a
// column reference, a CREATE VIEW's own CASE expression, ordinary DML) is
// never inspected for this at all.
func splitStatements(sql string) []string {
	var out []string
	start := 0
	beginDepth := 0
	i, n := 0, len(sql)
	for i < n {
		c := sql[i]
		switch {
		case isSpace(c):
			i++
		case c == '-' && i+1 < n && sql[i+1] == '-':
			i += 2
			for i < n && sql[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && sql[i+1] == '*':
			if end := strings.Index(sql[i+2:], "*/"); end >= 0 {
				i += 2 + end + 2
			} else {
				i = n
			}
		case c == '\'' || c == '"' || c == '`':
			_, next := scanQuoted(sql, i, c)
			i = next
		case c == '[':
			_, next := scanBracket(sql, i)
			i = next
		case isIdentStart(c):
			j := i
			for j < n && isIdentChar(sql[j]) {
				j++
			}
			switch strings.ToLower(sql[i:j]) {
			case "begin":
				if beginDepth == 0 && isCreateTriggerLeading(sql[start:i]) {
					beginDepth = 1
				}
			case "case":
				if beginDepth > 0 {
					beginDepth++
				}
			case "end":
				if beginDepth > 0 {
					beginDepth--
				}
			}
			i = j
		case c == ';' && beginDepth == 0:
			out = append(out, sql[start:i])
			i++
			start = i
		default:
			i++
		}
	}
	if start < n {
		out = append(out, sql[start:])
	}
	trimmed := make([]string, 0, len(out))
	for _, s := range out {
		if strings.TrimSpace(s) != "" {
			trimmed = append(trimmed, s)
		}
	}
	return trimmed
}

// isCreateTriggerLeading reports whether stmtPrefix — everything scanned of
// the current statement so far — opens with CREATE TRIGGER or
// CREATE TEMP/TEMPORARY TRIGGER. Every other leading word before the
// column, table name or clause words that inevitably follow is itself a
// plain identifier-shaped token, so leadingWords's own "stop at the first
// non-word token" rule can't tell them apart on its own; a small limit is
// enough here because only the first two or three words are ever checked.
func isCreateTriggerLeading(stmtPrefix string) bool {
	words := leadingWords(stmtPrefix, 3)
	if len(words) < 2 || words[0] != "create" {
		return false
	}
	if words[1] == "trigger" {
		return true
	}
	return len(words) >= 3 && (words[1] == "temp" || words[1] == "temporary") && words[2] == "trigger"
}

// isWord reports whether tokens[i] is a tokWord with exactly this text —
// exact, not case-insensitive, comparison is correct here because every
// caller only ever asks about a keyword position, and a keyword can never
// be meaningfully quoted and still function as that keyword.
func isWord(tokens []sqlToken, i int, text string) bool {
	return i < len(tokens) && tokens[i].kind == tokWord && tokens[i].text == text
}

// qualifiedNameEnd returns the index just past the (possibly
// schema-qualified) name starting at tokens[i] — a single word token, or a
// word, a ".", and another word.
func qualifiedNameEnd(tokens []sqlToken, i int) int {
	if i >= len(tokens) || tokens[i].kind != tokWord {
		return i
	}
	if i+2 < len(tokens) && tokens[i+1].kind == tokPunct && tokens[i+1].text == "." && tokens[i+2].kind == tokWord {
		return i + 3
	}
	return i + 1
}

// leadingWords returns up to limit leading tokWord tokens of stmt, lower-cased
// — the run of bare keywords/identifiers a statement starts with, stopping
// at the first token that isn't one (a string literal, a number, or
// punctuation). "COMMIT;" and "PRAGMA foreign_keys = OFF" both start with
// one; a string literal or a quoted table name used as a statement (never
// valid SQL on its own) does not, which is exactly why this can never
// mistake data for a keyword.
func leadingWords(stmt string, limit int) []string {
	var words []string
	for _, t := range tokenize(stmt) {
		if t.kind != tokWord {
			break
		}
		words = append(words, t.text)
		if len(words) >= limit {
			break
		}
	}
	return words
}
