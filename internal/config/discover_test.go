package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func touch(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("tasks: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDiscoverPrecedence(t *testing.T) {
	dir := t.TempDir()
	local := touch(t, dir, ".coucou.yaml")
	flagFile := touch(t, dir, "from-flag.yaml")
	envFile := touch(t, dir, "from-env.yaml")

	if got, err := Discover(dir, flagFile, envFile); err != nil || got != flagFile {
		t.Errorf("flag should win: got %q, err %v", got, err)
	}
	if got, err := Discover(dir, "", envFile); err != nil || got != envFile {
		t.Errorf("env should win over local: got %q, err %v", got, err)
	}
	if got, err := Discover(dir, "", ""); err != nil || got != local {
		t.Errorf("local should be used: got %q, err %v", got, err)
	}
}

func TestDiscoverYmlAlias(t *testing.T) {
	dir := t.TempDir()
	alias := touch(t, dir, ".coucou.yml")
	got, err := Discover(dir, "", "")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got != alias {
		t.Errorf("got %q, want %q", got, alias)
	}
}

func TestDiscoverPrefersYamlOverYml(t *testing.T) {
	dir := t.TempDir()
	primary := touch(t, dir, ".coucou.yaml")
	touch(t, dir, ".coucou.yml")
	got, err := Discover(dir, "", "")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got != primary {
		t.Errorf("got %q, want %q", got, primary)
	}
}

// The design explicitly rejects ancestor search: a config in the parent
// directory must NOT be found.
func TestDiscoverDoesNotSearchAncestors(t *testing.T) {
	parent := t.TempDir()
	touch(t, parent, ".coucou.yaml")
	child := filepath.Join(parent, "frontend")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := Discover(child, "", "")
	if err == nil {
		t.Fatal("expected an error; ancestor configs must not be discovered")
	}
	if !strings.Contains(err.Error(), child) {
		t.Errorf("error %q should name the directory searched (%s)", err, child)
	}
}

func TestDiscoverMissingFlagFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	if _, err := Discover(dir, filepath.Join(dir, "nope.yaml"), ""); err == nil {
		t.Fatal("expected an error for a --config path that does not exist")
	}
}

func TestDiscoverDirectoryInCwdWithYmlFallback(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".coucou.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	yml := touch(t, dir, ".coucou.yml")
	got, err := Discover(dir, "", "")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got != yml {
		t.Errorf("got %q, want %q (should skip directory and use .yml)", got, yml)
	}
}

func TestDiscoverDirectoryInCwdWithNoFallback(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".coucou.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Discover(dir, "", "")
	if err == nil {
		t.Fatal("expected an error when only a directory named .coucou.yaml exists")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error %q should name the directory searched (%s)", err, dir)
	}
}

func TestDiscoverFlagPointingAtDirectory(t *testing.T) {
	dir := t.TempDir()
	dirPath := filepath.Join(dir, "somedir")
	if err := os.Mkdir(dirPath, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Discover(dir, dirPath, "")
	if err == nil {
		t.Fatal("expected an error when --config points at a directory")
	}
	if !strings.Contains(err.Error(), "--config") {
		t.Errorf("error %q should mention --config", err)
	}
}

func TestDiscoverMissingEnvPathDoesNotFallback(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, ".coucou.yaml")
	_, err := Discover(dir, "", filepath.Join(dir, "nope.yaml"))
	if err == nil {
		t.Fatal("expected an error when $COUCOU_CONFIG points to a non-existent file")
	}
	if !strings.Contains(err.Error(), "$COUCOU_CONFIG") {
		t.Errorf("error %q should mention $COUCOU_CONFIG", err)
	}
}
