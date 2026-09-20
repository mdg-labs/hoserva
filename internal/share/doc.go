// Package share is the share model (doc 03 §4, D4): persist a share in
// SQLite, create its branch directories and per-share mergerfs mount
// (doc 02 §1, Q12), generate Samba and NFS from the same rows (Q73, doc
// 03 §4.2), and expose browse plus two distinct deletes — definition vs
// data.
package share
