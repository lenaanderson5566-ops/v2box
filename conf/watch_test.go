package conf

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestFailedWatchDoesNotLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		if stop, err := New().Watch(filepath.Join(t.TempDir(), "missing", "config.json"), func() {}); err == nil {
			stop()
			t.Fatal("accepted missing config")
		}
	}
	time.Sleep(100 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("failed watchers leaked goroutines: before=%d after=%d", before, after)
	}
}

func TestWatchReplacementAndStop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("initial"), 0600); err != nil {
		t.Fatal(err)
	}
	notified := make(chan struct{}, 10)
	stop, err := watchConfig(path, func() { notified <- struct{}{} }, 30*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	await := func() {
		t.Helper()
		select {
		case <-notified:
		case <-time.After(3 * time.Second):
			t.Fatal("no reload notification")
		}
	}
	if err := os.WriteFile(path, []byte("updated"), 0600); err != nil {
		t.Fatal(err)
	}
	await()
	replacement := filepath.Join(dir, "replacement.json")
	if err := os.WriteFile(replacement, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	await()
	if err := os.WriteFile(path, []byte("after-replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	await()
	stop()
	stop()
	if err := os.WriteFile(path, []byte("stopped"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-notified:
		t.Fatal("callback after Stop returned")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWatchLifecycleDoesNotLeak(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("initial"), 0600); err != nil {
		t.Fatal(err)
	}
	before := runtime.NumGoroutine()
	for i := 0; i < 30; i++ {
		stop, err := New().Watch(path, func() { t.Error("unexpected callback") })
		if err != nil {
			t.Fatal(err)
		}
		stop()
	}
	time.Sleep(100 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("watchers leaked: %d -> %d", before, after)
	}
}
