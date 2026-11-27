package proxy

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
)

// Writer is the single goroutine that appends proxy entries to the ledger.
// It never blocks callers: if the queue is full or the disk fails, entries are
// dropped and a `gap` entry is written once writing succeeds again (SPEC §6.4).
type Writer struct {
	store *ledger.Store
	ch    chan ledger.Entry
	done  chan struct{}

	// cmu guards closed and the send on ch: Close takes it exclusively, so an
	// Enqueue never sends on a closed channel.
	cmu    sync.RWMutex
	closed bool

	mu        sync.Mutex
	dropped   int64
	dropSince time.Time
	cause     string

	FlushEvery time.Duration
	log        *slog.Logger
}

// maxBatch is the most entries appended to the ledger in one write.
const maxBatch = 256

// NewWriter returns a writer with a queue of the given size. Start it with Run.
func NewWriter(s *ledger.Store, queue int, log *slog.Logger) *Writer {
	return &Writer{store: s, ch: make(chan ledger.Entry, queue), done: make(chan struct{}),
		FlushEvery: 200 * time.Millisecond, log: log}
}

// Enqueue submits an entry without blocking. It reports whether the entry was
// accepted; a dropped entry is counted and later recorded in a gap entry.
//
// After Close (a call still in flight when the proxy shut down), the entry is
// appended synchronously instead, so it is not lost.
func (w *Writer) Enqueue(e ledger.Entry) bool {
	w.cmu.RLock()
	if w.closed {
		w.cmu.RUnlock()
		if err := w.store.Append(e); err != nil {
			w.log.Error("late ledger write failed, entry lost", "error", err)
			return false
		}
		return true
	}
	defer w.cmu.RUnlock()
	select {
	case w.ch <- e:
		return true
	default:
		w.drop(1, "write queue full")
		return false
	}
}

func (w *Writer) drop(n int64, cause string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dropped == 0 {
		w.dropSince = w.store.Now()
	}
	w.dropped += n
	w.cause = cause
}

// Run processes the queue until Close is called or ctx is cancelled. On
// cancellation it stops accepting new entries (later ones are appended
// synchronously by Enqueue), writes what is queued, and returns ctx.Err().
func (w *Writer) Run(ctx context.Context) error {
	defer close(w.done)
	t := time.NewTicker(w.FlushEvery)
	defer t.Stop()
	var batch []ledger.Entry
	stop := ctx.Done()
	for {
		select {
		case e, ok := <-w.ch:
			if !ok {
				w.flush(batch)
				return ctx.Err()
			}
			batch = append(batch, e)
			if len(batch) >= maxBatch {
				batch = w.flush(batch)
			}
		case <-t.C:
			batch = w.flush(batch)
		case <-stop:
			stop = nil
			w.closeQueue()
		}
	}
}

// causeEncode is the gap cause for an entry the canonical writer refused. It
// says nothing about the entry.
const causeEncode = "entry could not be encoded"

// flush appends the batch, preceded by a gap entry if calls were dropped, and
// returns the emptied batch for reuse.
func (w *Writer) flush(batch []ledger.Entry) []ledger.Entry {
	if len(batch) == 0 && w.pendingDrops() == 0 {
		return batch
	}
	out, gapCount := w.withGap(batch)
	err := w.store.Append(out...)
	switch {
	case err == nil:
		w.settleGap(gapCount)
	case errors.Is(err, ledger.ErrEncode):
		w.flushEach(out, gapCount)
	default:
		w.failBatch(batch, err)
	}
	return batch[:0]
}

// failBatch drops entries after a write error. The disk is at fault, so they
// are not retried.
func (w *Writer) failBatch(entries []ledger.Entry, err error) {
	w.log.Error("ledger write failed, entries dropped", "entries", len(entries), "error", err)
	w.drop(int64(len(entries)), "ledger write error: "+err.Error())
}

// settleGap takes the gap entry just written off the dropped count.
func (w *Writer) settleGap(gapCount int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dropped -= gapCount
	if w.dropped > 0 {
		w.dropSince = w.store.Now()
	}
}

// flushEach appends out one entry at a time, after Append refused it as a
// whole because one entry cannot be encoded. That write left the ledger
// alone, so only the entries that cannot be encoded are dropped, and counted
// in a gap entry. If the disk fails during this, the rest of the batch is
// dropped as in flush. The first of out is the pending gap when gapCount > 0.
func (w *Writer) flushEach(out []ledger.Entry, gapCount int64) {
	var bad int64
	for i, e := range out {
		err := w.store.Append(e)
		if err == nil && gapCount > 0 && i == 0 {
			w.settleGap(gapCount)
		}
		if errors.Is(err, ledger.ErrEncode) {
			w.log.Error("entry could not be encoded, dropped", "type", e.Type(), "error", err)
			bad++
			continue
		}
		if err != nil {
			w.dropEncoded(bad)
			w.failBatch(out[i:], err)
			return
		}
	}
	if bad > 0 {
		w.dropEncoded(bad)
		w.flush(nil) // write its gap now: nothing may come after a shutdown flush
	}
}

func (w *Writer) dropEncoded(n int64) {
	if n > 0 {
		w.drop(n, causeEncode)
	}
}

// withGap prepends a gap entry for the calls dropped so far, if any.
func (w *Writer) withGap(batch []ledger.Entry) ([]ledger.Entry, int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dropped == 0 {
		return batch, 0
	}
	gap := ledger.Entry{"type": ledger.TypeGap, "dropped": w.dropped,
		"since": ledger.FormatTime(w.dropSince), "until": ledger.FormatTime(w.store.Now()), "cause": w.cause}
	return append([]ledger.Entry{gap}, batch...), w.dropped
}

func (w *Writer) pendingDrops() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped
}

// Close drains the queue and waits for the final write. It is safe to call
// more than once and concurrently with Enqueue.
func (w *Writer) Close() {
	w.closeQueue()
	<-w.done
}

// closeQueue stops the queue: later entries are appended synchronously.
func (w *Writer) closeQueue() {
	w.cmu.Lock()
	defer w.cmu.Unlock()
	if !w.closed {
		w.closed = true
		close(w.ch)
	}
}
