//go:build linux

package auth

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestPeerCredentialOfReportsOwnProcess(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	acceptedCh := make(chan *net.UnixConn, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		acceptedCh <- conn.(*net.UnixConn)
	}()

	client, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close() }()

	var server *net.UnixConn
	select {
	case server = <-acceptedCh:
	case err := <-errCh:
		t.Fatalf("accept: %v", err)
	}
	defer func() { _ = server.Close() }()

	cred, err := PeerCredentialOf(server)
	if err != nil {
		t.Fatalf("PeerCredentialOf: %v", err)
	}
	if int(cred.UID) != os.Getuid() {
		t.Errorf("UID = %d, want %d (this process's own uid — client and server are the same process in this test)", cred.UID, os.Getuid())
	}
}
