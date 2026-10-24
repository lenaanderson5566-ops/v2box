package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestManagerDelegation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux manager")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "manager.sh")
	if runManager(path, nil) == nil {
		t.Fatal("missing manager accepted")
	}
	os.WriteFile(path, []byte("not a script"), 0700)
	if runManager(path, nil) == nil {
		t.Fatal("binary accepted as manager")
	}
	os.WriteFile(path, []byte("#!/bin/bash\n[[ $1 == update && $2 == 'literal;argument' ]]\n"), 0700)
	if err := runManager(path, []string{"update", "literal;argument"}); err != nil {
		t.Fatal(err)
	}
	if runManager(path, []string{"uninstall"}) == nil {
		t.Fatal("child failure ignored")
	}
	link := filepath.Join(dir, "link")
	os.Symlink(path, link)
	if runManager(link, nil) == nil {
		t.Fatal("symlink accepted")
	}
}
