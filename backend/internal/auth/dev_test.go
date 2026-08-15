package auth

import (
	"context"
	"net/http/httptest"
	"testing"
)

func TestDevAuthenticator_AlwaysAdmin(t *testing.T) {
	d := NewDevAuthenticator("dev-admin", "tok")
	id, err := d.Authenticate(context.Background(), httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Login != "dev-admin" || id.Token != "tok" {
		t.Errorf("identity = %+v, want dev-admin/tok", id)
	}
}
