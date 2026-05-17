package compose

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultComposePath_priority(t *testing.T) {
	dir := t.TempDir()
	// Lower-priority file present first — should still pick nyx-compose.yaml when added? 
	// Create docker-compose first then nyx — order lists nyx first.
	docker := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(docker, []byte("version: '3'\nservices:\n  a:\n    image: alpine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := DefaultComposePath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != docker {
		t.Fatalf("expected %q got %q", docker, got)
	}
	nyx := filepath.Join(dir, "nyx-compose.yaml")
	if err := os.WriteFile(nyx, []byte("version: '3'\nservices:\n  b:\n    image: alpine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got2, err := DefaultComposePath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got2 != nyx {
		t.Fatalf("nyx-compose should win: want %q got %q", nyx, got2)
	}
}
