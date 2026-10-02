package credentials

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRoleFileCredentialsSelectsRequestedConfiguredRole(t *testing.T) {
	directory := t.TempDir()
	adminPath := filepath.Join(directory, "admin.json")
	readonlyPath := filepath.Join(directory, "readonly.json")
	if err := os.WriteFile(adminPath, []byte(`{"username":"admin-user","password":"admin-password"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(readonlyPath, []byte(`{"username":"readonly-user","password":"readonly-password"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	roles := NewFileRoles(map[string]string{
		"admin":    adminPath,
		"readonly": readonlyPath,
	})
	credentials, err := roles["readonly"](context.Background(), Identity{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Username != "readonly-user" {
		t.Fatalf("credentials = %#v, want readonly role", credentials)
	}
	if roles["support"] != nil {
		t.Fatal("unexpected support role")
	}
}

func TestRoleFileCredentialsRejectsEmptyPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte(`{"username":"vault-user","password":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := NewFileRoles(map[string]string{"readonly": path})["readonly"]
	if _, err := provider(context.Background(), Identity{}, false); err == nil {
		t.Fatal("expected an empty password to be rejected")
	}
}
