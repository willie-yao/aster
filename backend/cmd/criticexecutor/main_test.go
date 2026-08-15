package main

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/causalcritic"
)

func TestReadRequestRejectsTrailingData(t *testing.T) {
	t.Setenv(causalcritic.RequestEnv, base64.StdEncoding.EncodeToString([]byte(`{} {}`)))
	if _, err := readRequest(); err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("err=%v", err)
	}
}
