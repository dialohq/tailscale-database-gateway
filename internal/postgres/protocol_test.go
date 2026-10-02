package postgres

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

type readWriter struct {
	io.Reader
	io.Writer
}

func TestReadInitialPacketRejectsTLSAndParsesStartup(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	result := make(chan initialPacket, 1)
	errors := make(chan error, 1)
	go func() {
		packet, err := readInitialPacket(server)
		result <- packet
		errors <- err
	}()

	sslRequest, err := (&pgproto3.SSLRequest{}).Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(sslRequest); err != nil {
		t.Fatal(err)
	}
	var response [1]byte
	if _, err := client.Read(response[:]); err != nil {
		t.Fatal(err)
	}
	if response[0] != 'N' {
		t.Fatalf("SSL response = %q, want N", response[0])
	}

	startup, err := (&pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersion30,
		Parameters: map[string]string{
			"user":     "untrusted",
			"database": "caddie",
		},
	}).Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(startup); err != nil {
		t.Fatal(err)
	}
	if err := <-errors; err != nil {
		t.Fatal(err)
	}
	packet := <-result
	if packet.Startup == nil || packet.Startup.Parameters["database"] != "caddie" {
		t.Fatalf("startup packet = %#v", packet)
	}
}

func TestReadInitialPacketParsesLongCancelKey(t *testing.T) {
	request := &pgproto3.CancelRequest{ProcessID: 42, SecretKey: bytes.Repeat([]byte{0xa5}, 32)}
	encoded, err := request.Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	packet, err := readInitialPacket(readWriter{Reader: bytes.NewReader(encoded), Writer: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if packet.Cancel == nil || packet.Cancel.ProcessID != 42 || !bytes.Equal(packet.Cancel.SecretKey, request.SecretKey) {
		t.Fatalf("cancel packet = %#v", packet.Cancel)
	}
}

func TestReadInitialPacketRequiresUserAndDatabase(t *testing.T) {
	for name, parameters := range map[string]map[string]string{
		"user":     {"user": "readonly"},
		"database": {"database": "app"},
	} {
		t.Run(name, func(t *testing.T) {
			encoded, err := (&pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersion30, Parameters: parameters}).Encode(nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := readInitialPacket(readWriter{Reader: bytes.NewReader(encoded), Writer: io.Discard}); err == nil {
				t.Fatal("incomplete startup packet was accepted")
			}
		})
	}
}

func TestReadStartupFrameRejectsOversizedPacketBeforeAllocation(t *testing.T) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], 1<<20)
	if _, err := readInitialPacket(readWriter{Reader: bytes.NewReader(header[:]), Writer: io.Discard}); err == nil {
		t.Fatal("expected oversized packet rejection")
	}
}

func TestSanitizeRuntimeParameters(t *testing.T) {
	parameters, unrecognized := sanitizeRuntimeParameters(map[string]string{
		"user":             "attacker",
		"database":         "other",
		"application_name": "spoofed",
		"client_encoding":  "UTF8",
		"options":          "-c role=postgres",
	}, "tailscale:alice")

	if parameters["application_name"] != "tailscale:alice" || parameters["client_encoding"] != "UTF8" {
		t.Fatalf("runtime parameters = %#v", parameters)
	}
	if _, ok := parameters["options"]; ok {
		t.Fatalf("unsafe options survived: %#v", parameters)
	}
	if len(unrecognized) != 1 || unrecognized[0] != "options" {
		t.Fatalf("unrecognized options = %#v", unrecognized)
	}
}
