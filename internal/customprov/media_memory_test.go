package customprov

import (
	"fmt"
	"sync"
	"testing"
)

func TestMediaMemoryLearnAndHas(t *testing.T) {
	m := newMediaMemory()

	if m.has("provider-a", "model-a") {
		t.Fatal("has() = true before learn()")
	}

	m.learn("provider-a", "model-a")

	if !m.has("provider-a", "model-a") {
		t.Fatal("has() = false after learn()")
	}
}

func TestMediaMemoryProviderIsolation(t *testing.T) {
	m := newMediaMemory()
	m.learn("provider-a", "shared-model")

	if m.has("provider-b", "shared-model") {
		t.Fatal("same model under a different provider must not match")
	}
	if !m.has("provider-a", "shared-model") {
		t.Fatal("learned pair must still match")
	}
}

func TestMediaMemoryModelIsolation(t *testing.T) {
	m := newMediaMemory()
	m.learn("shared-provider", "model-a")

	if m.has("shared-provider", "model-b") {
		t.Fatal("same provider with a different model must not match")
	}
	if !m.has("shared-provider", "model-a") {
		t.Fatal("learned pair must still match")
	}
}

func TestMediaMemoryEmptyFields(t *testing.T) {
	m := newMediaMemory()

	// Empty values are ordinary key components, not wildcards.
	m.learn("", "")
	if !m.has("", "") {
		t.Fatal(`has("", "") = false after learn("", "")`)
	}
	if m.has("", "model-a") || m.has("provider-a", "") {
		t.Fatal("empty component matched a non-empty pair")
	}

	m.learn("", "model-a")
	if !m.has("", "model-a") {
		t.Fatal(`has("", "model-a") = false after learn`)
	}
	if !m.has("", "") {
		t.Fatal("learn(\"\", \"model-a\") disturbed the empty pair")
	}

	m.learn("provider-a", "")
	if !m.has("provider-a", "") {
		t.Fatal(`has("provider-a", "") = false after learn`)
	}
	if m.has("provider-a", "model-a") {
		t.Fatal(`has("provider-a", "model-a") = true without learn`)
	}
}

func TestMediaMemoryNilReceiver(t *testing.T) {
	var m *mediaMemory

	if m.has("provider-a", "model-a") {
		t.Fatal("nil receiver has() must return false")
	}

	m.learn("provider-a", "model-a") // must not panic
}

func TestMediaMemoryRepeatedLearn(t *testing.T) {
	m := newMediaMemory()
	m.learn("provider-a", "model-a")
	m.learn("provider-a", "model-a")
	m.learn("provider-a", "model-a")

	if !m.has("provider-a", "model-a") {
		t.Fatal("has() = false after repeated learn()")
	}
	if got := len(m.models); got != 1 {
		t.Fatalf("len(models) = %d after repeated learn, want 1", got)
	}
}

func TestMediaMemoryConcurrent(t *testing.T) {
	const (
		workers = 32
		pairs   = 100
	)

	m := newMediaMemory()
	// Seed a shared pair that must stay observable for the whole run.
	m.learn("shared-provider", "shared-model")

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < pairs; i++ {
				provider := fmt.Sprintf("provider-%d", w)
				model := fmt.Sprintf("model-%d", i)

				if m.has(provider, model) {
					t.Errorf("has(%q, %q) = true before learn", provider, model)
					return
				}
				m.learn(provider, model)
				if !m.has(provider, model) {
					t.Errorf("has(%q, %q) = false after learn", provider, model)
					return
				}
				if !m.has("shared-provider", "shared-model") {
					t.Error("shared pair stopped matching during concurrent learn")
					return
				}
			}
		}(w)
	}
	wg.Wait()

	// Every learned pair must still be present after all goroutines finish.
	for w := 0; w < workers; w++ {
		for i := 0; i < pairs; i++ {
			provider := fmt.Sprintf("provider-%d", w)
			model := fmt.Sprintf("model-%d", i)
			if !m.has(provider, model) {
				t.Fatalf("has(%q, %q) = false after concurrent learn", provider, model)
			}
		}
	}
	if got, want := len(m.models), workers*pairs+1; got != want {
		t.Fatalf("len(models) = %d, want %d", got, want)
	}
}
