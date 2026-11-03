// Package landing serves public static content without exposing directory
// listings, hidden files, or files outside the configured root.
package landing

import (
	_ "embed"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed index.html
var DefaultHTML string

func New(directory string) (http.Handler, error) {
	if directory == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !publicRequest(w, r) {
				return
			}
			if r.URL.Path != "/" && r.URL.Path != "/index.html" {
				http.NotFound(w, r)
				return
			}
			http.ServeContent(w, r, "index.html", time.Time{}, strings.NewReader(DefaultHTML))
		}), nil
	}
	directory, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	index, err := os.OpenInRoot(directory, "index.html")
	if err != nil {
		return nil, fmt.Errorf("landing page requires a readable index.html: %w", err)
	}
	stat, err := index.Stat()
	index.Close()
	if err != nil || !stat.Mode().IsRegular() {
		return nil, fmt.Errorf("landing page index.html must be a regular file")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !publicRequest(w, r) {
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if strings.ContainsAny(name, "\\\x00:") {
			http.NotFound(w, r)
			return
		}
		for _, part := range strings.Split(name, "/") {
			if strings.HasPrefix(part, ".") {
				http.NotFound(w, r)
				return
			}
		}
		if name == "" || strings.HasSuffix(name, "/") {
			name += "index.html"
		}
		file, err := os.OpenInRoot(directory, filepath.FromSlash(name))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, info.Name(), info.ModTime(), file)
	}), nil
}

func publicRequest(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return false
	}
	return true
}
