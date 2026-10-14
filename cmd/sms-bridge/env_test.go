package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	body := "# comment\n\nBRIDGE_TEST_A=one\nexport BRIDGE_TEST_B=\"two words\"\nBRIDGE_TEST_C='three'\nBRIDGE_TEST_D=already\nBRIDGE_TEST_E=\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIDGE_TEST_D", "kept")
	for _, k := range []string{"BRIDGE_TEST_A", "BRIDGE_TEST_B", "BRIDGE_TEST_C", "BRIDGE_TEST_E"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	if err := loadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"BRIDGE_TEST_A": "one", "BRIDGE_TEST_B": "two words", "BRIDGE_TEST_C": "three", "BRIDGE_TEST_D": "kept", "BRIDGE_TEST_E": ""}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if err := loadDotEnv(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Errorf("a missing .env is fine, got %v", err)
	}
}

func TestLoadDotEnvBadLineHidesContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("OK=1\nzzSecretWithoutEquals\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OK", "")
	err := loadDotEnv(path)
	if err == nil || !strings.Contains(err.Error(), "line 2") || strings.Contains(err.Error(), "zzSecret") {
		t.Errorf("got %v", err)
	}
}
