package mcpserver_test

import "strings"

// Test helpers that read the fake's recorded state. Kept separate from the
// handler so the assertions live beside the tests that use them.

// writesTo returns the retain bodies the proxy sent for one bank.
func (f *fakeHindsight) writesTo(bank string) []retainBody {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]retainBody(nil), f.writes[bank]...)
}

// authHeaders returns the Authorization header of every request the fake saw.
func (f *fakeHindsight) authHeaders() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auths...)
}

// banksCreated returns the bank ids the proxy asked to create.
func (f *fakeHindsight) banksCreated() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.created))
	for id := range f.created {
		out = append(out, id)
	}
	sortStrings(out)
	return out
}

// memoriesIn returns the memories stored in a bank.
func (f *fakeHindsight) memoriesIn(bank string) []storedMemory {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]storedMemory(nil), f.banks[bank]...)
}

// tagsIn returns every tag on every memory in one bank, in stable order.
func (f *fakeHindsight) tagsIn(bank string) []string {
	var out []string
	for _, m := range f.memoriesIn(bank) {
		out = append(out, m.Tags...)
	}
	sortStrings(out)
	return out
}

// memoryID returns the id of the first stored memory in bank whose text
// contains substr, so a test can fetch a record it did not create.
func (f *fakeHindsight) memoryID(bank, substr string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.banks[bank] {
		if strings.Contains(m.Text, substr) {
			return m.ID
		}
	}
	return ""
}
