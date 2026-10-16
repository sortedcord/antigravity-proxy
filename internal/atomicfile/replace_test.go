package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicfileReplaceAndSync(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "from.txt")
	to := filepath.Join(dir, "to.txt")
	if err := os.WriteFile(from, []byte("atomic replacement payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Replace(from, to); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(to)
	if err != nil || string(data) != "atomic replacement payload" {
		t.Fatalf("unexpected content: %s, err: %v", data, err)
	}
	if err := SyncDir(dir); err != nil {
		t.Fatal(err)
	}
}
