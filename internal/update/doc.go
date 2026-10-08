// Package update implements Hoserva's self-update, rollback and Debian
// update reporting (Q67, Q68, Q49, D16):
//
//   - The update check reads only the release index on the project site
//     (https://hoserva.dev/releases/index.json) — never the GitHub API
//     and never a system-wide apt update. The index is an unsigned
//     pointer; the signed SHA256SUMS is the trust root.
//   - A downloaded .deb is verified against the signed SHA256SUMS before
//     it is installed, and the version its signed file name carries must
//     be the version being installed: strictly newer than the running
//     one (and not a beta on the stable channel), or for a rollback the
//     recorded previous version. A failed check installs nothing and
//     notifies.
//   - Install runs in a transient systemd unit after a config backup,
//     and is refused while a Parity, Array-write or Topology job runs.
//   - Rollback is the previous package plus the live-database snapshot
//     taken when that version was upgraded away — there are no down
//     migrations (D16).
//   - Hoserva never reboots on its own. A user-started reboot waits for
//     those same job classes and then runs the Q70 shutdown sequence.
//
// Every system-touching step sits behind an interface with a scriptable
// fake, so unit tests never talk to real systemd, apt, or GitHub.
package update
