package mcpserver_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// markerFor builds a marker that cannot be a prefix of another caller's, so a
// substring check between markers is unambiguous.
func markerFor(i int) string {
	return fmt.Sprintf("zz-marker-%d-end", i)
}

// The proxy resolves its routing table per request from a hot-reloadable
// snapshot, so a burst of concurrent callers is exactly the condition under
// which a routing bug would surface as one agent reading another's bank. This
// drives many callers across distinct banks at once and checks every one of them
// stayed in its own lane.
func TestConcurrentCallersStayInTheirOwnBanks(t *testing.T) {
	const callers = 12

	var b strings.Builder
	b.WriteString(`{"tokens":{`)
	for i := range callers {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"conc-token-%d":{"agent":"Conc%d","bank":"conc-bank-%d","tools":"*"}`, i, i, i)
	}
	b.WriteString("}}")

	s := newTestServer(t, b.String())

	var wg sync.WaitGroup
	// Buffered generously and drained non-blockingly: if a regression makes many
	// callers fail, the test must report that, not deadlock on a full channel.
	errs := make(chan string, callers*callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token := fmt.Sprintf("conc-token-%d", i)
			marker := markerFor(i)
			if text, isErr := s.callTool(token, "retain", map[string]any{
				"items": []any{map[string]any{"content": marker}},
			}); isErr {
				errs <- fmt.Sprintf("caller %d retain failed: %s", i, text)
				return
			}
			text, isErr := s.callTool(token, "recall", map[string]any{"query": "concurrency marker"})
			if isErr {
				errs <- fmt.Sprintf("caller %d recall failed: %s", i, text)
				return
			}
			// Its own marker must be there...
			if !strings.Contains(text, marker) {
				errs <- fmt.Sprintf("caller %d cannot see its own memory", i)
			}
			// ...and no other bank's marker may appear.
			for j := range callers {
				if j == i {
					continue
				}
				if strings.Contains(text, markerFor(j)) {
					errs <- fmt.Sprintf("caller %d saw bank %d's memory", i, j)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}

	// Each marker must have landed in its own bank and nowhere else.
	for i := range callers {
		bank := fmt.Sprintf("conc-bank-%d", i)
		if got := len(s.fake.writesTo(bank)); got != 1 {
			t.Errorf("bank %s saw %d writes, want 1", bank, got)
		}
	}
}

// Reloading the routing table while requests are in flight must not drop a
// caller or let one slip into the wrong bank.
func TestConcurrentCallsDuringReload(t *testing.T) {
	s := newTestServer(t, routingTable)

	stop := make(chan struct{})
	var reloads sync.WaitGroup
	reloads.Add(1)
	go func() {
		defer reloads.Done()
		// Alternate between two valid tables; each swap is a reload trigger.
		// The swap is atomic -- write a temp file, then rename -- because a
		// non-atomic rewrite can expose a partial file, which the watcher would
		// reject as a parse error instead of reloading. Real deployments write
		// config the same way, so this is the condition worth testing.
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := writeFileAtomically(s.cfgPath, routingTableUpdated); err != nil {
				return
			}
			if err := writeFileAtomically(s.cfgPath, routingTable); err != nil {
				return
			}
		}
	}()

	var wg sync.WaitGroup
	errs := make(chan string, 256)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 5 {
				text, isErr := s.callTool(alphaToken, "retain", map[string]any{
					"items": []any{map[string]any{"content": "reload-race marker"}},
				})
				if isErr {
					errs <- "retain failed during reload: " + text
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	reloads.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
	// alpha moves between "ops" and "moved"; both must only ever receive its
	// writes, and the shared bank must never see one.
	if got := len(s.fake.writesTo("shared")); got != 0 {
		t.Errorf("alpha's writes leaked into the default bank during reload: %d", got)
	}
	total := len(s.fake.writesTo("ops")) + len(s.fake.writesTo("moved"))
	if total != 40 {
		t.Errorf("expected 40 writes across the two banks, got %d", total)
	}
}
