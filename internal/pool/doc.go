// Package pool builds the mergerfs mount topology doc 02 §1 and Q12
// describe: a catch-all /mnt/user pool over every data disk, one mount
// per share with branches chosen by the share's own cache mode, and the
// array-only /run/hoserva/array/<share> mount the mover writes through
// so mergerfs — never the mover — places every file (doc 09 §2). It
// renders the systemd .mount units that topology needs. Production
// brings them up with SystemdMounter (systemctl start on those units)
// so mergerfs lives outside hoserva.service's cgroup (#335); Mounter's
// direct argv is the lab path (no init system, doc 06 §3). It computes
// no placement of its own: which disk a file lands on is mergerfs's
// create policy, never this package's (doc 09 §2, "one placement
// algorithm").
package pool
