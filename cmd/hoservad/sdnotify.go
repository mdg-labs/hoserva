package main

import (
	"log"
	"net"
	"os"
	"strings"
)

// notifySystemdReady sends systemd's sd_notify(3) "READY=1" datagram
// (doc 02 §1, Q69): packaging/debian/hoserva.service's Type=notify makes
// this — not the process merely having forked — what every unit ordered
// After=hoserva.service (pool.HoservadServiceUnit, referenced from
// hoserva-storage-ready.service's own generated content) actually waits
// for, so a dependent service's boot-time activation of that unit can
// never race ahead of cmd/hoservad having written and reloaded this
// boot's storage-target units. NOTIFY_SOCKET is unset outside systemd (a
// dev run, the loop-device lab, an L3 guest invoked without it), which
// sd_notify(3) itself documents as a normal no-op, not an error.
func notifySystemdReady() {
	socketPath := os.Getenv("NOTIFY_SOCKET")
	if socketPath == "" {
		return
	}
	// sd_notify(3): a leading '@' names an abstract namespace socket,
	// addressed with a leading NUL byte instead of the literal '@'.
	if strings.HasPrefix(socketPath, "@") {
		socketPath = "\x00" + socketPath[1:]
	}

	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socketPath, Net: "unixgram"})
	if err != nil {
		log.Printf("hoservad: notifying systemd ready: %v", err)
		return
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("READY=1")); err != nil {
		log.Printf("hoservad: notifying systemd ready: %v", err)
	}
}
