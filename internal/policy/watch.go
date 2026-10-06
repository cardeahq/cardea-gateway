package policy

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"
)

// Watch reloads when policy or data files change, until ctx ends. It
// returns immediately, nil, when watching is off or no policy is configured.
func (e *Engine) Watch(ctx context.Context) error {
	if !e.cfg.Watch || e.cfg.Path == "" {
		return nil
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating file watcher: %w", err)
	}
	defer func() { _ = w.Close() }()

	// Parent directories, not the files: editors that save by
	// rename-and-replace would otherwise end the watch silently.
	configured := map[string]bool{filepath.Clean(e.cfg.Path): true}
	if err := w.Add(filepath.Dir(e.cfg.Path)); err != nil {
		return fmt.Errorf("watching %s: %w", e.cfg.Path, err)
	}
	for _, p := range e.cfg.Data {
		configured[filepath.Clean(p)] = true
		info, err := os.Stat(p)
		if err == nil && info.IsDir() {
			err = addTree(w, p)
		} else if err == nil {
			err = w.Add(filepath.Dir(p))
		}
		if err != nil {
			return fmt.Errorf("watching %s: %w", p, err)
		}
	}

	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			e.logger.Error("policy file watcher error", zap.Error(err))
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			if ev.Op == fsnotify.Chmod {
				continue
			}
			counted := configured[filepath.Clean(ev.Name)] || isDataExt(ev.Name) || strings.EqualFold(filepath.Ext(ev.Name), ".rego")
			if ev.Has(fsnotify.Create) {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					_ = addTree(w, ev.Name)
					counted = true
				}
			}
			if !counted {
				continue
			}
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(e.cfg.WatchSettle, func() { _ = e.Reload("watch") })
		}
	}
}

func addTree(w *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return w.Add(path)
		}
		return nil
	})
}
