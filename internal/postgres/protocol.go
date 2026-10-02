package postgres

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"

	"github.com/jackc/pgx/v5/pgproto3"
)

type initialPacket struct {
	Startup *pgproto3.StartupMessage
	Cancel  *pgproto3.CancelRequest
}

func readInitialPacket(conn io.ReadWriter) (initialPacket, error) {
	backend := pgproto3.NewBackend(conn, conn)
	negotiationRequests := 0
	for {
		message, err := backend.ReceiveStartupMessage()
		if err != nil {
			return initialPacket{}, fmt.Errorf("read startup message: %w", err)
		}
		switch message := message.(type) {
		case *pgproto3.SSLRequest, *pgproto3.GSSEncRequest:
			if negotiationRequests >= 2 {
				return initialPacket{}, fmt.Errorf("invalid encryption negotiation request")
			}
			if _, err := conn.Write([]byte{'N'}); err != nil {
				return initialPacket{}, fmt.Errorf("reject encryption negotiation: %w", err)
			}
			negotiationRequests++
			continue
		case *pgproto3.CancelRequest:
			return initialPacket{Cancel: message}, nil
		case *pgproto3.StartupMessage:
			if message.Parameters["user"] == "" || message.Parameters["database"] == "" {
				return initialPacket{}, fmt.Errorf("startup message must contain user and database")
			}
			return initialPacket{Startup: message}, nil
		default:
			return initialPacket{}, fmt.Errorf("unsupported startup message %T", message)
		}
	}
}

func sanitizeRuntimeParameters(parameters map[string]string, applicationName string) (map[string]string, []string) {
	runtimeParameters := make(map[string]string)
	var unrecognized []string
	for name, value := range parameters {
		switch name {
		case "user", "database":
			continue
		}
		switch name {
		case "application_name", "client_encoding", "DateStyle", "TimeZone", "extra_float_digits":
			runtimeParameters[name] = value
		default:
			unrecognized = append(unrecognized, name)
		}
	}
	runtimeParameters["application_name"] = applicationName
	sort.Strings(unrecognized)
	return runtimeParameters, unrecognized
}

func writeBackendMessages(writer io.Writer, messages ...pgproto3.BackendMessage) error {
	backend := pgproto3.NewBackend(nil, writer)
	for _, message := range messages {
		backend.Send(message)
	}
	return backend.Flush()
}

func writeStartupSuccess(writer io.Writer, upstream *Upstream, unrecognizedOptions []string) error {
	messages := make([]pgproto3.BackendMessage, 0, len(upstream.ParameterStatuses)+4)
	if len(unrecognizedOptions) > 0 {
		messages = append(messages, &pgproto3.NegotiateProtocolVersion{
			NewestMinorProtocol: 0,
			UnrecognizedOptions: unrecognizedOptions,
		})
	}
	messages = append(messages, &pgproto3.AuthenticationOk{})
	for _, name := range slices.Sorted(maps.Keys(upstream.ParameterStatuses)) {
		messages = append(messages, &pgproto3.ParameterStatus{Name: name, Value: upstream.ParameterStatuses[name]})
	}
	messages = append(messages,
		&pgproto3.BackendKeyData{ProcessID: upstream.PID, SecretKey: upstream.SecretKey},
		&pgproto3.ReadyForQuery{TxStatus: upstream.TxStatus},
	)
	return writeBackendMessages(writer, messages...)
}

func writeFatal(writer io.Writer, code, message string) {
	_ = writeBackendMessages(writer, &pgproto3.ErrorResponse{
		Severity:            "FATAL",
		SeverityUnlocalized: "FATAL",
		Code:                code,
		Message:             message,
	})
}
