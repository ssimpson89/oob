package detect

import (
	"sync"
	"testing"

	"github.com/ssimpson89/oob/internal/app"
)

// swap sets *p to v for the rest of the test and restores the old value when
// the test ends.
func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// notices collects the notes the package queues through app.AddNotice.
var notices struct {
	sync.Mutex
	msgs []string
}

// clearNotices starts collecting queued notes for the test and empties the
// collection when the test ends.
func clearNotices(t *testing.T) {
	t.Helper()
	notices.Lock()
	notices.msgs = nil
	notices.Unlock()
	swap(t, &app.AddNotice, func(m string) {
		notices.Lock()
		notices.msgs = append(notices.msgs, m)
		notices.Unlock()
	})
	t.Cleanup(func() {
		notices.Lock()
		notices.msgs = nil
		notices.Unlock()
	})
}
