package watcher_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func recipeFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"justfile", "scripts/check-coverage.sh"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		writeRecipeFile(t, dir, name, string(data))
	}
	return dir
}

func writeRecipeFile(t *testing.T, dir, name, data string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestCleanPreservesOtherSessionsScratch(t *testing.T) {
	dir := recipeFixture(t)
	outputs := []string{"bin/app", "dist/package", "coverage.out"}
	for _, name := range append(outputs, ".tmp/other-session/keep") {
		writeRecipeFile(t, dir, name, "sentinel")
	}
	cmd := exec.Command("just", "clean")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clean: %v\n%s", err, out)
	}
	for _, name := range outputs {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("clean left %s: %v", name, err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, ".tmp/other-session/keep")); err != nil || string(data) != "sentinel" {
		t.Fatalf("clean damaged another session's scratch: %q, %v", data, err)
	}
}

func TestTestRecipeDoesNotLeaveCoverageProfile(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		body := "if value() != 1 { t.Fatal(\"wrong value\") }"
		if fail {
			name = "failure"
			body += "; t.Fatal(\"intentional failure\")"
		}
		t.Run(name, func(t *testing.T) {
			dir := recipeFixture(t)
			writeRecipeFile(t, dir, "go.mod", "module recipefixture\n\ngo 1.26.2\n")
			writeRecipeFile(t, dir, "value.go", "package fixture\nfunc value() int { return 1 }\n")
			writeRecipeFile(t, dir, "value_test.go", "package fixture\nimport \"testing\"\nfunc TestValue(t *testing.T) { "+body+" }\n")
			cmd := exec.Command("just", "test")
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if (err != nil) != fail {
				t.Fatalf("test error = %v, want failure %t\n%s", err, fail, out)
			}
			if _, err := os.Stat(filepath.Join(dir, "coverage.out")); !os.IsNotExist(err) {
				t.Fatalf("test left coverage.out: %v\n%s", err, out)
			}
		})
	}
}
