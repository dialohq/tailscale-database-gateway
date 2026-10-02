package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

func NewFileRoles(paths map[string]string) Roles {
	return NewRoles(paths, loadRoleFile)
}

func loadRoleFile(_ context.Context, _ Identity, role, path string, _ bool) (Credentials, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return Credentials{}, fmt.Errorf("read %q credentials: %w", role, err)
	}

	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var credentials Credentials
	if err := decoder.Decode(&credentials); err != nil {
		return Credentials{}, fmt.Errorf("decode credentials: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Credentials{}, fmt.Errorf("decode credentials: trailing JSON value")
	}
	if credentials.Username == "" {
		return Credentials{}, fmt.Errorf("credentials username is empty")
	}
	if credentials.Password == "" {
		return Credentials{}, fmt.Errorf("credentials password is empty")
	}
	return credentials, nil
}
