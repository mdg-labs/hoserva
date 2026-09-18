// Package config is the one path from SQLite state to generated files
// (D4, doc 01 §2): render state to text, write it under a doc 01 §2
// header that names who wrote it and at what config revision, and record
// its hash so a later Check can tell a hand edit from a file Hoserva
// itself last wrote. Every write is atomic (temp file plus rename) and
// goes under a caller-supplied root, so tests and dev runs never touch
// /etc.
package config
