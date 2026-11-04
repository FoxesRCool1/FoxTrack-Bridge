//go:build headless

package main

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Every feature file runs in its own try, so even with no DOM at all (node)
// the bundle as a whole must run to the end without an uncaught error.
func TestFeaturesJSIsolatesEachFile(t *testing.T) {
	rec := httptest.NewRecorder()
	handleFeaturesJS(rec, httptest.NewRequest("GET", "/features.js", nil))
	body := rec.Body.String()
	if n := strings.Count(body, "(0, eval)("); n < 5 {
		t.Fatalf("expected one eval per feature file, got %d", n)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	f := filepath.Join(t.TempDir(), "features.js")
	if err := os.WriteFile(f, []byte(body+"\nconsole.log('END');\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, f).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "END") {
		t.Fatalf("bundle did not run to the end: %v\n%s", err, out)
	}
}
