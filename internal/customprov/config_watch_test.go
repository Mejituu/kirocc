package customprov

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const watchBaseConfig = `{
  "providers": {
    "p": {
      "base_url": "http://old.example",
      "auth_token": "old-token",
      "models": [{"id": "glm-5.3"}]
    }
  }
}`

const watchTextOnlyConfig = `{
  "providers": {
    "p": {
      "base_url": "http://old.example",
      "auth_token": "old-token",
      "models": [{"id": "glm-5.3", "text_only": true}]
    }
  }
}`

const watchCredentialsConfig = `{
  "providers": {
    "p": {
      "base_url": "https://new.example/v1",
      "auth_token": "new-token",
      "models": [{"id": "glm-5.3"}]
    }
  }
}`

// watchSameSizeConfig changes only the token, and the replacement has the same
// length, so the file's size does not change.
const watchSameSizeConfig = `{
  "providers": {
    "p": {
      "base_url": "http://old.example",
      "auth_token": "new-token",
      "models": [{"id": "glm-5.3"}]
    }
  }
}`

const watchTwoModelsConfig = `{
  "providers": {
    "p": {
      "base_url": "http://old.example",
      "auth_token": "old-token",
      "models": [{"id": "glm-5.3"}, {"id": "kimi-k3"}]
    }
  }
}`

const watchMappingBaseConfig = `{
  "providers": {
    "p": {
      "base_url": "http://p",
      "auth_token": "t",
      "models": [{"id": "m1"}, {"id": "search-model", "web_search": true}]
    }
  }
}`

const watchMappingUpdatedConfig = `{
  "kiro": {"web_search_fallback": {"provider": "p", "model": "search-model"}},
  "providers": {
    "p": {
      "base_url": "http://p",
      "auth_token": "t",
      "models": [{"id": "m1"}, {"id": "search-model", "web_search": true}],
      "mappings": [{"model": "claude-sonnet-5", "to": {"model": "m1"}}],
      "web_search_fallback": {"model": "search-model"}
    }
  }
}`

// loadWatchRegistry loads a config from a path that the test keeps stable for
// later rewrites.
func loadWatchRegistry(t *testing.T, path string) *Registry {
	t.Helper()
	t.Setenv("KIROCC_MODEL_MAPPINGS", "")
	reg, err := loadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func overwriteConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newWatchFixture loads initial into a temp file and uses the load-time file
// state as the reload baseline. The returned check can be called directly
// with no real polling delay.
func newWatchFixture(t *testing.T, initial string, debounce time.Duration) (string, *Proxy, *configReloader) {
	t.Helper()
	path := writeConfig(t, initial)
	reg := loadWatchRegistry(t, path)
	p := NewProxy(reg, time.Second)
	return path, p, newConfigReloader(p, path, debounce, reg.loaded)
}

func TestConfigReload_FileAppearsAfterWatcherStart(t *testing.T) {
	captureSlog(t)
	path := writeConfig(t, watchBaseConfig)
	reg := loadWatchRegistry(t, path)
	p := NewProxy(reg, time.Second)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	r := newConfigReloader(p, path, 0, nil)
	if r.last != nil {
		t.Fatal("last = non-nil, want the missing file to leave no baseline")
	}

	overwriteConfig(t, path, watchTextOnlyConfig)
	r.check()

	hit, ok := p.Lookup("claude-glm-5.3")
	if !ok || !hit.TextOnly {
		t.Fatalf("Lookup after the file appeared = %+v, %v; want text_only=true", hit, ok)
	}
}

func TestConfigReload_BaselineFromLoad(t *testing.T) {
	captureSlog(t)
	path := writeConfig(t, watchBaseConfig)
	reg := loadWatchRegistry(t, path)
	p := NewProxy(reg, time.Second)

	// Simulate a change between the startup Load and the watcher's first poll.
	overwriteConfig(t, path, watchTextOnlyConfig)
	r := newConfigReloader(p, path, 0, reg.loaded)
	r.check()

	hit, ok := p.Lookup("claude-glm-5.3")
	if !ok || !hit.TextOnly {
		t.Fatalf("Lookup after a load-to-watch change = %+v, %v; want text_only=true", hit, ok)
	}
}

func TestSameFileState_WindowsIdentityFallback(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	if err := os.WriteFile(a, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-time.Minute)
	for _, path := range []string{a, b} {
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	ai, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	bi, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(ai, bi) {
		t.Fatal("test files unexpectedly have the same identity")
	}
	if sameFileStateFor("windows", nil, bi) || sameFileStateFor("windows", ai, nil) {
		t.Error("nil FileInfo must not compare equal")
	}
	if !sameFileStateFor("windows", ai, bi) {
		t.Error("Windows should fall back to equal metadata when identity is unavailable")
	}
	if sameFileStateFor("linux", ai, bi) {
		t.Error("Unix must treat a different identity as a change")
	}
	if err := os.WriteFile(b, []byte("longer"), 0o600); err != nil {
		t.Fatal(err)
	}
	bi, err = os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	if sameFileStateFor("windows", ai, bi) {
		t.Error("different metadata must be a change on Windows")
	}
}

func TestConfigReload_UpdatesTextOnly(t *testing.T) {
	path, p, r := newWatchFixture(t, watchBaseConfig, 0)
	p.media.learn("p", "glm-5.3")

	if hit, ok := p.Lookup("claude-glm-5.3"); !ok || hit.TextOnly {
		t.Fatalf("initial Lookup = %+v, %v; want text_only=false", hit, ok)
	}

	overwriteConfig(t, path, watchTextOnlyConfig)
	r.check()

	hit, ok := p.Lookup("claude-glm-5.3")
	if !ok {
		t.Fatal("Lookup missed after reload")
	}
	if !hit.TextOnly {
		t.Error("TextOnly = false after reload, want true")
	}
	if !p.media.has("p", "glm-5.3") {
		t.Error("media memory was lost on reload")
	}

	// An unchanged file must not be reloaded again: the successful load
	// advanced the baseline.
	before := p.registry.Load()
	r.check()
	if p.registry.Load() != before {
		t.Error("unchanged config was reloaded")
	}
}

func TestConfigReload_UpdatesProviderCredentials(t *testing.T) {
	path, p, r := newWatchFixture(t, watchBaseConfig, 0)

	overwriteConfig(t, path, watchCredentialsConfig)
	r.check()

	hit, ok := p.Lookup("claude-glm-5.3")
	if !ok {
		t.Fatal("Lookup missed after reload")
	}
	if hit.Provider.BaseURL != "https://new.example/v1" {
		t.Errorf("BaseURL = %q, want https://new.example/v1", hit.Provider.BaseURL)
	}
	if hit.Provider.authToken != "new-token" {
		t.Error("authToken was not replaced by the reload")
	}
}

func TestConfigReload_UpdatesMappingsAndFallbacks(t *testing.T) {
	path, p, r := newWatchFixture(t, watchMappingBaseConfig, 0)
	p.RecordSession("s", "p")

	if _, _, ok := p.MatchMapping("s", RequestInfo{Model: "claude-sonnet-5"}); ok {
		t.Fatal("mapping matched before the reload")
	}
	if _, ok := p.WebSearchFallbackHit("p"); ok {
		t.Fatal("provider fallback hit before the reload")
	}
	if _, ok := p.KiroWebSearchFallbackHit(); ok {
		t.Fatal("kiro fallback hit before the reload")
	}

	overwriteConfig(t, path, watchMappingUpdatedConfig)
	r.check()

	_, m, ok := p.MatchMapping("s", RequestInfo{Model: "claude-sonnet-5"})
	if !ok || m.Provider != "p" || m.ModelTo != "m1" {
		t.Fatalf("mapping = %+v, ok = %v; want p/m1", m, ok)
	}
	hit, ok := p.WebSearchFallbackHit("p")
	if !ok || hit.BaseModel != "search-model" || !hit.WebSearch {
		t.Errorf("provider fallback = %+v, %v; want search-model with web_search", hit, ok)
	}
	hit, ok = p.KiroWebSearchFallbackHit()
	if !ok || hit.BaseModel != "search-model" || !hit.WebSearch {
		t.Errorf("kiro fallback = %+v, %v; want search-model with web_search", hit, ok)
	}
}

func TestConfigReload_RemovesTextOnly(t *testing.T) {
	path, p, r := newWatchFixture(t, watchTextOnlyConfig, 0)
	if hit, ok := p.Lookup("claude-glm-5.3"); !ok || !hit.TextOnly {
		t.Fatalf("initial Lookup = %+v, %v; want text_only=true", hit, ok)
	}

	overwriteConfig(t, path, watchBaseConfig)
	r.check()

	hit, ok := p.Lookup("claude-glm-5.3")
	if !ok {
		t.Fatal("Lookup missed after removing text_only")
	}
	if hit.TextOnly {
		t.Error("TextOnly = true after removal, want false")
	}
}

func TestConfigReload_RemovesModel(t *testing.T) {
	path, p, r := newWatchFixture(t, watchTwoModelsConfig, 0)
	if _, ok := p.Lookup("claude-kimi-k3"); !ok {
		t.Fatal("kimi-k3 missing before removal")
	}

	overwriteConfig(t, path, watchBaseConfig)
	r.check()

	if _, ok := p.Lookup("claude-kimi-k3"); ok {
		t.Error("kimi-k3 survived its removal")
	}
	if _, ok := p.Lookup("claude-glm-5.3"); !ok {
		t.Error("retained model glm-5.3 was lost")
	}
}

func TestConfigReload_RemovesMappingsAndFallbacks(t *testing.T) {
	path, p, r := newWatchFixture(t, watchMappingUpdatedConfig, 0)
	p.RecordSession("s", "p")

	if _, _, ok := p.MatchMapping("s", RequestInfo{Model: "claude-sonnet-5"}); !ok {
		t.Fatal("mapping missing before removal")
	}
	if _, ok := p.WebSearchFallbackHit("p"); !ok {
		t.Fatal("provider fallback missing before removal")
	}
	if _, ok := p.KiroWebSearchFallbackHit(); !ok {
		t.Fatal("kiro fallback missing before removal")
	}

	overwriteConfig(t, path, watchMappingBaseConfig)
	r.check()

	if _, _, ok := p.MatchMapping("s", RequestInfo{Model: "claude-sonnet-5"}); ok {
		t.Error("mapping survived its removal")
	}
	if _, ok := p.WebSearchFallbackHit("p"); ok {
		t.Error("provider fallback survived its removal")
	}
	if _, ok := p.KiroWebSearchFallbackHit(); ok {
		t.Error("kiro fallback survived its removal")
	}
}

func TestConfigReload_BadJSON5KeepsOldAndRecovers(t *testing.T) {
	buf := captureSlog(t)
	path, p, r := newWatchFixture(t, watchBaseConfig, 0)
	baseline := r.last

	overwriteConfig(t, path, "{not json")
	r.check()

	if hit, ok := p.Lookup("claude-glm-5.3"); !ok || hit.TextOnly {
		t.Fatalf("Lookup after a bad reload = %+v, %v; want the old config", hit, ok)
	}
	r.check()
	if n := strings.Count(buf.String(), "custom provider config reload failed"); n != 1 {
		t.Fatalf("warning count = %d, want 1 (the same error must not repeat)\nlogs:\n%s", n, buf.String())
	}
	warn := findLogRecord(t, buf, "custom provider config reload failed")
	warnAttrs, _ := warn["attributes"].(map[string]any)
	if warnAttrs["path"] != path {
		t.Errorf("failure log path = %v, want %q", warnAttrs["path"], path)
	}
	if warnAttrs["err"] == nil || warnAttrs["err"] == "" {
		t.Errorf("failure log err = %v, want the load error", warnAttrs["err"])
	}
	if r.last != baseline {
		t.Error("a failed load advanced the file baseline")
	}

	overwriteConfig(t, path, watchTextOnlyConfig)
	r.check()

	hit, ok := p.Lookup("claude-glm-5.3")
	if !ok || !hit.TextOnly {
		t.Fatalf("Lookup after recovery = %+v, %v; want text_only=true", hit, ok)
	}
	rec := findLogRecord(t, buf, "custom provider config reloaded")
	attrs, _ := rec["attributes"].(map[string]any)
	if attrs["path"] != path {
		t.Errorf("reload log path = %v, want %q", attrs["path"], path)
	}
	if attrs["providers"] != float64(1) || attrs["models"] != float64(1) {
		t.Errorf("reload log counts = providers %v, models %v; want 1 and 1", attrs["providers"], attrs["models"])
	}
	if strings.Contains(buf.String(), "old-token") {
		t.Errorf("auth_token leaked into logs:\n%s", buf.String())
	}

	// A successful load clears the suppression, so the same error later is a
	// new episode and must be logged again.
	overwriteConfig(t, path, "{not json")
	r.check()
	if n := strings.Count(buf.String(), "custom provider config reload failed"); n != 2 {
		t.Errorf("warning count after a new failure = %d, want 2", n)
	}
}

func TestConfigReload_DeletedFileKeepsOldAndRecovers(t *testing.T) {
	buf := captureSlog(t)
	path, p, r := newWatchFixture(t, watchBaseConfig, 0)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	r.check()
	if hit, ok := p.Lookup("claude-glm-5.3"); !ok || hit.TextOnly {
		t.Fatalf("Lookup after deletion = %+v, %v; want the old config", hit, ok)
	}
	r.check()
	if n := strings.Count(buf.String(), "custom provider config reload failed"); n != 1 {
		t.Fatalf("warning count = %d, want 1 (deletion must warn once)\nlogs:\n%s", n, buf.String())
	}

	overwriteConfig(t, path, watchTextOnlyConfig)
	r.check()
	hit, ok := p.Lookup("claude-glm-5.3")
	if !ok || !hit.TextOnly {
		t.Fatalf("Lookup after recreation = %+v, %v; want text_only=true", hit, ok)
	}
}

func TestConfigReload_DebounceWaitsForStableFile(t *testing.T) {
	path, p, r := newWatchFixture(t, watchBaseConfig, 20*time.Millisecond)

	overwriteConfig(t, path, watchTextOnlyConfig)
	r.check()

	hit, ok := p.Lookup("claude-glm-5.3")
	if !ok || !hit.TextOnly {
		t.Fatalf("Lookup after debounced reload = %+v, %v; want text_only=true", hit, ok)
	}
}

func TestConfigReload_DebounceSkipsStillChangingFile(t *testing.T) {
	path, p, r := newWatchFixture(t, watchBaseConfig, 100*time.Millisecond)
	overwriteConfig(t, path, watchTextOnlyConfig)
	r.sleep = func(time.Duration) {
		overwriteConfig(t, path, watchCredentialsConfig)
	}
	r.check()

	// The file changed again during the quiet period, so neither version may
	// be installed.
	hit, ok := p.Lookup("claude-glm-5.3")
	if !ok || hit.TextOnly || hit.Provider.BaseURL != "http://old.example" {
		t.Fatalf("unstable file was reloaded: %+v, %v", hit, ok)
	}

	// The next check sees the credentials config at rest.
	r.debounce = 0
	r.check()
	hit, ok = p.Lookup("claude-glm-5.3")
	if !ok || hit.TextOnly || hit.Provider.BaseURL != "https://new.example/v1" {
		t.Fatalf("stable file was not reloaded: %+v, %v", hit, ok)
	}
}

func TestConfigReload_DeletedDuringDebounce(t *testing.T) {
	buf := captureSlog(t)
	path, p, r := newWatchFixture(t, watchBaseConfig, 100*time.Millisecond)
	overwriteConfig(t, path, watchTextOnlyConfig)
	r.sleep = func(time.Duration) {
		if err := os.Remove(path); err != nil {
			t.Error(err)
		}
	}
	r.check()

	if hit, ok := p.Lookup("claude-glm-5.3"); !ok || hit.TextOnly {
		t.Fatalf("Lookup after deletion during debounce = %+v, %v; want the old config", hit, ok)
	}
	if n := strings.Count(buf.String(), "custom provider config reload failed"); n != 1 {
		t.Errorf("warning count = %d, want 1", n)
	}
}

func TestConfigReload_SameSizeContentChange(t *testing.T) {
	path, p, r := newWatchFixture(t, watchBaseConfig, 0)
	overwriteConfig(t, path, watchSameSizeConfig)
	// Only ModTime signals this change; set it explicitly so the test does not
	// depend on filesystem timestamp granularity.
	stamp := time.Now().Add(-time.Minute)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	r.check()

	hit, ok := p.Lookup("claude-glm-5.3")
	if !ok || hit.Provider.authToken != "new-token" {
		t.Fatalf("same-size change was missed: %+v, %v", hit, ok)
	}
}

func TestNewConfigReloader_StatsWithoutBaseline(t *testing.T) {
	path := writeConfig(t, watchBaseConfig)
	r := newConfigReloader(NewProxy(nil, time.Second), path, 0, nil)
	if r.last == nil {
		t.Error("last = nil, want the current file state when no baseline is supplied")
	}
}

func TestWatchConfig_DisabledWithoutPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		NewProxy(nil, time.Second).WatchConfig(ctx)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("WatchConfig did not return when custom providers are disabled")
	}
}

func TestWatchConfig_ReturnsOnCancel(t *testing.T) {
	path, p, _ := newWatchFixture(t, watchBaseConfig, 0)
	if p.configPath != path {
		t.Fatalf("configPath = %q, want %q", p.configPath, path)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.WatchConfig(ctx)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("WatchConfig did not return after ctx cancellation")
	}
}

func TestWatchConfig_PollsChangedFile(t *testing.T) {
	captureSlog(t)
	path, p, _ := newWatchFixture(t, watchBaseConfig, 0)
	if p.configPath != path {
		t.Fatalf("configPath = %q, want %q", p.configPath, path)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.watchConfig(ctx, 5*time.Millisecond, 0)
	}()
	defer func() {
		cancel()
		<-done
	}()

	// Let the watcher record its baseline before the file changes.
	time.Sleep(50 * time.Millisecond)
	overwriteConfig(t, path, watchTextOnlyConfig)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hit, ok := p.Lookup("claude-glm-5.3"); ok && hit.TextOnly {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("watchConfig did not reload the changed file")
}

func TestConfigReload_ConcurrentLookups(t *testing.T) {
	captureSlog(t)
	path, p, r := newWatchFixture(t, watchBaseConfig, 0)

	stop := make(chan struct{})
	errCh := make(chan string, 8)
	var wg sync.WaitGroup
	var lookups atomic.Int64
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				hit, ok := p.Lookup("claude-glm-5.3")
				if !ok || hit.BaseModel != "glm-5.3" {
					errCh <- "Lookup returned an unexpected result during a reload"
					return
				}
				_ = p.ListModels()
				lookups.Add(1)
				runtime.Gosched()
			}
		}()
	}

	for i := range 100 {
		if i%2 == 0 {
			overwriteConfig(t, path, watchTextOnlyConfig)
		} else {
			overwriteConfig(t, path, watchBaseConfig)
		}
		r.check()
		time.Sleep(time.Millisecond)
	}
	close(stop)
	wg.Wait()
	close(errCh)
	for msg := range errCh {
		t.Error(msg)
	}
	if lookups.Load() == 0 {
		t.Error("no concurrent lookups ran")
	}
}
