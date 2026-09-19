// Package backup implements Hoserva's config backup job (#36, doc 10 §1,
// Q28, Q40, Q74, Q80): a consistent archive of the daemon's SQLite state,
// generated configs, stacks, templates and optional SnapRAID content files,
// with secret columns and stack .env files re-encrypted under the user's
// backup passphrase. metrics.db and job logs are excluded (Q74); the
// machine key is never included (Q28).
//
// Service satisfies job.ConfigBackup for the nightly maintenance chain's
// last step (Q30). The same Run method is what callers invoke before a
// self-update or an array-topology change (doc 10 §1).
package backup
