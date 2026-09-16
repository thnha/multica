package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestProbeMuseConfiguredExecutable(t *testing.T) {
	dir := t.TempDir()
	name := "muse"
	if runtime.GOOS == "windows" {
		name = "muse.exe"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("fake executable, never executed"), 0755); err != nil {
		t.Fatal(err)
	}
	orig := resolveAgentsViaLoginShell
	t.Cleanup(func() { resolveAgentsViaLoginShell = orig })
	resolveAgentsViaLoginShell = func([]string) map[string]string { return map[string]string{} }
	resetShellResolveCacheForTest(t)
	t.Setenv("PATH", dir)
	t.Setenv("MULTICA_MUSE_PATH", path)
	t.Setenv("MULTICA_MUSE_MODEL", "test-model")
	entry, ok := probeAgentCLIs()["muse"]
	if !ok || entry.Path != path || entry.Command != path || entry.Model != "test-model" {
		t.Fatalf("Muse runtime = %+v, found %v", entry, ok)
	}
	if providerDisplayName("muse") != "Muse Code" {
		t.Fatal("Muse display name missing")
	}
}
