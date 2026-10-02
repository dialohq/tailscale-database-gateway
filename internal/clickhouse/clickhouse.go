package clickhouse

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

const (
	clientHelloPacket     = uint64(0)
	serverHelloPacket     = uint64(0)
	serverExceptionPacket = uint64(2)
	maxHandshakeString    = 1 << 20
)

type clientHello struct {
	clientName string
	major      uint64
	minor      uint64
	revision   uint64
	database   string
	username   string
}

func readClientHello(r io.Reader) (clientHello, error) {
	packet, err := readUvarint(r)
	if err != nil {
		return clientHello{}, fmt.Errorf("read packet type: %w", err)
	}
	if packet != clientHelloPacket {
		return clientHello{}, fmt.Errorf("unexpected first client packet %d", packet)
	}

	clientName, err := readString(r)
	if err != nil {
		return clientHello{}, fmt.Errorf("read client name: %w", err)
	}
	major, err := readUvarint(r)
	if err != nil {
		return clientHello{}, fmt.Errorf("read client major version: %w", err)
	}
	minor, err := readUvarint(r)
	if err != nil {
		return clientHello{}, fmt.Errorf("read client minor version: %w", err)
	}
	revision, err := readUvarint(r)
	if err != nil {
		return clientHello{}, fmt.Errorf("read client revision: %w", err)
	}
	database, err := readString(r)
	if err != nil {
		return clientHello{}, fmt.Errorf("read database: %w", err)
	}
	username, err := readString(r)
	if err != nil {
		return clientHello{}, fmt.Errorf("read username: %w", err)
	}
	if _, err := readString(r); err != nil {
		return clientHello{}, fmt.Errorf("read password: %w", err)
	}

	return clientHello{
		clientName: clientName,
		major:      major,
		minor:      minor,
		revision:   revision,
		database:   database,
		username:   username,
	}, nil
}

func (h clientHello) encode(username, password string) ([]byte, error) {
	if len(username) > maxHandshakeString {
		return nil, fmt.Errorf("upstream username exceeds %d bytes", maxHandshakeString)
	}
	if len(password) > maxHandshakeString {
		return nil, fmt.Errorf("upstream password exceeds %d bytes", maxHandshakeString)
	}

	var b bytes.Buffer
	writeUvarint(&b, clientHelloPacket)
	writeString(&b, h.clientName)
	writeUvarint(&b, h.major)
	writeUvarint(&b, h.minor)
	writeUvarint(&b, h.revision)
	writeString(&b, h.database)
	writeString(&b, username)
	writeString(&b, password)
	return b.Bytes(), nil
}

func readUvarint(r io.Reader) (uint64, error) {
	return binary.ReadUvarint(byteReader{Reader: r})
}

type byteReader struct {
	io.Reader
}

func (r byteReader) ReadByte() (byte, error) {
	var raw [1]byte
	_, err := io.ReadFull(r.Reader, raw[:])
	return raw[0], err
}

func writeUvarint(w io.Writer, value uint64) error {
	var raw [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(raw[:], value)
	_, err := w.Write(raw[:n])
	return err
}

func readString(r io.Reader) (string, error) {
	length, err := readUvarint(r)
	if err != nil {
		return "", err
	}
	if length > maxHandshakeString {
		return "", fmt.Errorf("string length %d exceeds %d bytes", length, maxHandshakeString)
	}
	value := make([]byte, int(length))
	if _, err := io.ReadFull(r, value); err != nil {
		return "", err
	}
	return string(value), nil
}

func writeString(w io.Writer, value string) {
	writeUvarint(w, uint64(len(value)))
	_, _ = io.WriteString(w, value)
}
