package customprov

import (
	"context"
	"log/slog"
	"os"
	"runtime"
	"time"
)

const (
	// configPollInterval is how often the config file is checked.
	configPollInterval = 5 * time.Second
	// configReloadDebounce lets an editor finish writing before the file is
	// parsed.
	configReloadDebounce = 300 * time.Millisecond
)

// WatchConfig applies config-file changes for the lifetime of ctx. It polls
// on every platform because the file may be replaced by an editor at any
// time and no platform-specific notification is needed.
func (p *Proxy) WatchConfig(ctx context.Context) {
	p.watchConfig(ctx, configPollInterval, configReloadDebounce)
}

// watchConfig is WatchConfig with injectable timing for tests. A missing
// config path means custom providers are disabled, so there is nothing to
// watch.
func (p *Proxy) watchConfig(ctx context.Context, interval, debounce time.Duration) {
	if p == nil || p.configPath == "" {
		return
	}
	var baseline os.FileInfo
	if reg := p.registry.Load(); reg != nil {
		baseline = reg.loaded
	}
	r := newConfigReloader(p, p.configPath, debounce, baseline)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.check()
		}
	}
}

// reloadConfig loads and validates the file, then installs it atomically. The
// current registry stays in place when loading fails.
func (p *Proxy) reloadConfig() error {
	reg, err := Load(p.configPath)
	if err != nil {
		return err
	}
	p.registry.Store(reg)
	models := 0
	for _, provider := range reg.providers {
		models += len(provider.Models)
	}
	slog.Info("custom provider config reloaded",
		"path", p.configPath, "providers", len(reg.providers), "models", models)
	return nil
}

// configReloader polls the config file. Its baseline is the file state
// observed when the last successful load started; a failed load leaves both
// the installed registry and the baseline untouched, so the next tick
// retries.
type configReloader struct {
	proxy    *Proxy
	path     string
	debounce time.Duration
	// sleep is time.Sleep; tests replace it to change the file during the
	// quiet period deterministically.
	sleep   func(time.Duration)
	last    os.FileInfo
	lastErr string
}

// newConfigReloader uses baseline when the caller has the load-time state;
// otherwise it records the current state so the first change is still
// detected.
func newConfigReloader(p *Proxy, path string, debounce time.Duration, baseline os.FileInfo) *configReloader {
	r := &configReloader{proxy: p, path: path, debounce: debounce, sleep: time.Sleep, last: baseline}
	if r.last == nil {
		if info, err := os.Stat(path); err == nil {
			r.last = info
		}
	}
	return r
}

// check compares the file with the last successful load and reloads after a
// quiet period. It is a single step so tests can drive it directly.
func (r *configReloader) check() {
	info, err := os.Stat(r.path)
	if err != nil {
		// A deleted or temporarily unreadable file leaves the last usable
		// config and routing in place.
		r.warnFailure(err)
		return
	}
	if r.last != nil && sameFileState(r.last, info) {
		return
	}
	if r.debounce > 0 {
		r.sleep(r.debounce)
		stable, err := os.Stat(r.path)
		if err != nil {
			r.warnFailure(err)
			return
		}
		if !sameFileState(info, stable) {
			// Still being written; the next tick starts a fresh quiet period.
			return
		}
		info = stable
	}
	if err := r.proxy.reloadConfig(); err != nil {
		r.warnFailure(err)
		return
	}
	r.last = info
	r.lastErr = ""
}

// sameFileState reports whether two stats describe the same unchanged file.
// os.SameFile compares the file identity (inode on Unix, volume serial and
// file index on Windows), so an editor's save-as/rename replacement is
// noticed even when the name is unchanged.
func sameFileState(a, b os.FileInfo) bool {
	return sameFileStateFor(runtime.GOOS, a, b)
}

// sameFileStateFor applies the platform policy:
//   - different metadata is a change;
//   - equal metadata with the same identity is unchanged;
//   - equal metadata without a confirmed identical identity is unchanged on
//     Windows (a replacement that coincidentally preserves size and mtime is
//     missed; known blind spot) and a change on Unix.
//
// A failed Stat (for example, a missing or locked file) is handled by check's
// error branch, which keeps the previous config. goos is a parameter so the
// Windows branch is testable on other hosts.
func sameFileStateFor(goos string, a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return false
	}
	if !a.ModTime().Equal(b.ModTime()) || a.Size() != b.Size() {
		return false
	}
	if os.SameFile(a, b) {
		return true
	}
	return goos == "windows"
}

// warnFailure logs each distinct error once. A successful reload resets the
// suppression.
func (r *configReloader) warnFailure(err error) {
	msg := err.Error()
	if msg == r.lastErr {
		return
	}
	r.lastErr = msg
	slog.Warn("custom provider config reload failed", "path", r.path, "err", err)
}
