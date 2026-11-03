package landing

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStaticFilesAreConfined(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"index.html": "<p>public</p>", "style.css": "body{}", ".env": "secret"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	handler, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/", 200}, {"HEAD", "/", 200}, {"GET", "/style.css", 200},
		{"GET", "/empty/", 404}, {"GET", "/.env", 404}, {"GET", "/../index.html", 404},
		{"GET", "/%2e%2e/index.html", 404}, {"GET", "/index.html:secret", 404}, {"POST", "/", 405},
	} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, nil))
		if rr.Code != tc.status {
			t.Fatalf("%s %s: %d", tc.method, tc.path, rr.Code)
		}
		if tc.method == "HEAD" && rr.Body.Len() != 0 {
			t.Fatal("HEAD has body")
		}
		if strings.Contains(rr.Body.String(), "secret") {
			t.Fatal("hidden data exposed")
		}
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(outside, []byte("outside"), 0600)
	if err := os.Symlink(outside, filepath.Join(dir, "link.txt")); err != nil {
		t.Logf("symlink check unavailable: %v", err)
		return
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("GET", "/link.txt", nil))
	if rr.Code != 404 {
		t.Fatal("outside symlink served")
	}
}

func TestDefaultAndMissingDirectory(t *testing.T) {
	h, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Welcome") {
		t.Fatal("missing built-in page")
	}
	if _, err := New(t.TempDir()); err == nil {
		t.Fatal("directory without index accepted")
	}
}
