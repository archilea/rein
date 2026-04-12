package meter

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestPricerHolder_LoadReturnsInitial(t *testing.T) {
	p := &Pricer{models: map[string]map[string]ModelPrice{
		"openai": {"gpt-4o": {InputPerMToken: 1, OutputPerMToken: 2}},
	}}
	h := NewPricerHolder(p)
	if got := h.Load(); got != p {
		t.Errorf("Load(): got %p want %p", got, p)
	}
}

func TestPricerHolder_SwapReturnsPrevious(t *testing.T) {
	p1 := &Pricer{models: map[string]map[string]ModelPrice{"openai": {"a": {}}}}
	p2 := &Pricer{models: map[string]map[string]ModelPrice{"openai": {"b": {}}}}
	h := NewPricerHolder(p1)
	if prev := h.Swap(p2); prev != p1 {
		t.Errorf("Swap returned: got %p want %p", prev, p1)
	}
	if got := h.Load(); got != p2 {
		t.Errorf("Load after Swap: got %p want %p", got, p2)
	}
}

func TestPricerHolder_ConcurrentLoadAndSwap(t *testing.T) {
	// Regression guard: under concurrent Load/Swap the holder never
	// returns a partially-constructed pointer. This is a contract
	// sanity test — atomic.Pointer's guarantees already cover this,
	// but the test pins the expectation in case a future refactor
	// accidentally replaces the atomic with a mutex-and-pointer shape
	// that isn't publication-safe.
	p1 := &Pricer{models: map[string]map[string]ModelPrice{"openai": {"one": {}}}}
	p2 := &Pricer{models: map[string]map[string]ModelPrice{"openai": {"two": {}}}}
	h := NewPricerHolder(p1)

	const loads = 5000
	const swaps = 500
	var wg sync.WaitGroup
	var nilObserved atomic.Bool

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < swaps; i++ {
			if i%2 == 0 {
				h.Swap(p2)
			} else {
				h.Swap(p1)
			}
		}
	}()

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < loads; j++ {
				got := h.Load()
				if got == nil {
					nilObserved.Store(true)
					return
				}
				// Also touch Len() to force the load to materialize.
				_ = got.Len()
			}
		}()
	}
	wg.Wait()

	if nilObserved.Load() {
		t.Errorf("observed nil Pricer during concurrent Load/Swap; atomic publication is broken")
	}
}

// BenchmarkPricerHolder_LoadCost quantifies the indirection overhead of
// fetching the pricer via atomic.Pointer.Load() before calling Cost(),
// compared to calling Cost() on a direct *Pricer. This is the
// per-response cost of the #25 hot-reload design. Expected to be
// single-digit nanoseconds — the load is an aligned 8-byte atomic read.
// If this regresses by more than a few ns, the holder abstraction has
// been accidentally replaced by something heavier (a mutex, a
// sync.Map, etc.) and the full-path bench in internal/proxy will flag
// it too.
func BenchmarkPricerHolder_LoadCost(b *testing.B) {
	p, err := LoadPricer()
	if err != nil {
		b.Fatal(err)
	}
	h := NewPricerHolder(p)
	b.ResetTimer()
	b.ReportAllocs()
	var total float64
	for i := 0; i < b.N; i++ {
		cost, _ := h.Load().Cost("openai", "gpt-4o", 1000, 500)
		total += cost
	}
	_ = total
}

// BenchmarkPricer_DirectCost is the baseline for BenchmarkPricerHolder_LoadCost.
// The delta between the two is the per-call cost of the atomic load.
func BenchmarkPricer_DirectCost(b *testing.B) {
	p, err := LoadPricer()
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	var total float64
	for i := 0; i < b.N; i++ {
		cost, _ := p.Cost("openai", "gpt-4o", 1000, 500)
		total += cost
	}
	_ = total
}

func TestPricerHolder_SwapWithNilIsAllowed(t *testing.T) {
	// A nil Swap is weird but not forbidden. The contract is that Load
	// returns whatever Swap last stored. Downstream callers (adapter
	// recordSpend) guard against a nil return. Documenting the fact
	// here so a defensive NewPricerHolder-disallows-nil refactor does
	// not silently break this.
	h := NewPricerHolder(&Pricer{models: map[string]map[string]ModelPrice{}})
	h.Swap(nil)
	if got := h.Load(); got != nil {
		t.Errorf("Load after Swap(nil): got %v want nil", got)
	}
}
