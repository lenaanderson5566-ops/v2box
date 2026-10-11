package conf

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch only signals reload; the server loop owns configuration publication.
// The returned stop function waits for the watcher and its callback to exit.
func (p *Conf) Watch(filePath string, reload func()) (func(), error) {
	return watchConfig(filePath, reload, 5*time.Second)
}

func watchConfig(filePath string, reload func(), delay time.Duration) (func(), error) {
	path, err := filepath.Abs(filePath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("watch config: %w", err)
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("new watcher error: %w", err)
	}
	// A directory watch survives editors replacing the file atomically.
	if err := watcher.Add(filepath.Dir(path)); err != nil {
		watcher.Close()
		return nil, fmt.Errorf("watch config directory: %w", err)
	}
	done, exited := make(chan struct{}), make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }); <-exited }
	go func() {
		defer close(exited)
		defer watcher.Close()
		var timer *time.Timer
		var fired <-chan time.Time
		defer func() {
			if timer != nil {
				timer.Stop()
			}
		}()
		for {
			select {
			case <-done:
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if filepath.Clean(event.Name) != path || !event.Has(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) {
					continue
				}
				if timer == nil {
					timer = time.NewTimer(delay)
				} else {
					timer.Reset(delay)
				}
				fired = timer.C
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("File watcher error: %s", err)
			case <-fired:
				fired = nil
				select {
				case <-done:
					return
				default:
				}
				reload()
			}
		}
	}()
	return stop, nil
}
