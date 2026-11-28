package proxy

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
)

// Appender is what the writer needs of the ledger: append entries in one
// durable write, and the time. *ledger.Store is one.
type Appender interface {
	Append(entries ...ledger.Entry) error
	Now() time.Time
}

// Writer is the single goroutine that appends proxy entries to the ledger.
// It never blocks callers: if the queue is full or the disk fails, entries are
// dropped and a `gap` entry is written once writing succeeds again (SPEC §6.4).
// The exception is an intent (write-ahead mode): its caller waits for the
// write and is told if it failed, and it is never counted as dropped.
type Writer struct {
	store Appender
	ch    chan item
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
	// AckTimeout is how long Intent waits for its write.
	AckTimeout time.Duration
	log        *slog.Logger
}

// item is a queued entry. ack is set for an intent, whose caller waits for it.
type item struct {
	e   ledger.Entry
	ack chan<- ackResult
}

type ackResult struct {
	seq int64
	err error
}

// Why an intent could not be written.
var (
	ErrQueueFull  = errors.New("write queue full")
	ErrAckTimeout = errors.New("ledger write did not finish in time")
)

const (
	// maxBatch is the most entries appended to the ledger in one write.
	maxBatch = 256
	// defaultAckTimeout is the limit on the wait for an intent (SPEC §6.4).
	defaultAckTimeout = 5 * time.Second
)

// NewWriter returns a writer with a queue of the given size. Start it with Run.
func NewWriter(s Appender, queue int, log *slog.Logger) *Writer {
	return &Writer{store: s, ch: make(chan item, queue), done: make(chan struct{}),
		FlushEvery: 200 * time.Millisecond, AckTimeout: defaultAckTimeout, log: log}
}

// Enqueue submits an entry without blocking. It reports whether the entry was
// accepted; a dropped entry is counted and later recorded in a gap entry.
//
// After Close (a call still in flight when the proxy shut down), the entry is
// appended synchronously instead, so it is not lost.
func (w *Writer) Enqueue(e ledger.Entry) bool {
	err := w.submit(item{e: e})
	if errors.Is(err, ErrQueueFull) {
		w.drop(1, "write queue full")
	}
	return err == nil
}

// Intent queues an intent entry and waits until it is on disk, fsynced, then
// returns its seq. It does not wait for more entries to batch with it: the
// writer flushes at once, with whatever is queued. It fails when the queue is
// full, when the write fails, after AckTimeout, or when ctx ends. Nothing is
// counted as dropped; the caller decides what a failure means. An intent that
// failed after it was queued may still be written later.
func (w *Writer) Intent(ctx context.Context, e ledger.Entry) (int64, error) {
	ack := make(chan ackResult, 1)
	if err := w.submit(item{e: e, ack: ack}); err != nil {
		return 0, err
	}
	t := time.NewTimer(w.AckTimeout)
	defer t.Stop()
	select {
	case r := <-ack:
		return r.seq, r.err
	case <-t.C:
		return 0, ErrAckTimeout
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// submit queues it, or appends it at once when the writer is closed. It
// returns ErrQueueFull when the queue has no room. An intent that is appended
// at once has its answer on its ack channel, not in the return value.
func (w *Writer) submit(it item) error {
	w.cmu.RLock()
	if w.closed {
		w.cmu.RUnlock()
		return w.appendLate(it)
	}
	defer w.cmu.RUnlock()
	select {
	case w.ch <- it:
		return nil
	default:
		return ErrQueueFull
	}
}

func (w *Writer) appendLate(it item) error {
	err := w.store.Append(it.e)
	if it.ack != nil {
		it.ack <- ackResult{seq: it.e.Seq(), err: err}
		return nil
	}
	if err != nil {
		w.log.Error("late ledger write failed, entry lost", "error", err)
	}
	return err
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
	var batch []item
	stop := ctx.Done()
	for {
		select {
		case it, ok := <-w.ch:
			if !ok {
				w.flush(batch)
				return ctx.Err()
			}
			var closed bool
			if batch, closed = w.take(batch, it); closed {
				return ctx.Err()
			}
		case <-t.C:
			batch = w.flush(batch)
		case <-stop:
			stop = nil
			w.closeQueue()
		}
	}
}

// take adds it to the batch and flushes when the batch is full or holds an
// intent. An intent flushes at once, with every entry already queued behind
// it, so concurrent intents share one fsync. closed is true when the queue
// was found closed and empty, after the flush.
func (w *Writer) take(batch []item, it item) (rest []item, closed bool) {
	batch = append(batch, it)
	if it.ack == nil {
		if len(batch) < maxBatch {
			return batch, false
		}
		return w.flush(batch), false
	}
	batch, closed = w.queued(batch)
	return w.flush(batch), closed
}

// queued adds to the batch what is already in the queue, up to maxBatch,
// without waiting. closed is true when it reached the end of a closed queue.
func (w *Writer) queued(batch []item) (all []item, closed bool) {
	for len(batch) < maxBatch {
		select {
		case it, ok := <-w.ch:
			if !ok {
				return batch, true
			}
			batch = append(batch, it)
		default:
			return batch, false
		}
	}
	return batch, false
}

// causeEncode is the gap cause for an entry the canonical writer refused. It
// says nothing about the entry.
const causeEncode = "entry could not be encoded"

// flush appends the batch, preceded by a gap entry if calls were dropped, and
// returns the emptied batch for reuse. Every intent in it is answered.
func (w *Writer) flush(batch []item) []item {
	if len(batch) == 0 && w.pendingDrops() == 0 {
		return batch
	}
	out, gapCount := w.withGap(batch)
	err := w.store.Append(entriesOf(out)...)
	switch {
	case err == nil:
		w.settleGap(gapCount)
		answer(out, nil)
	case errors.Is(err, ledger.ErrEncode):
		w.flushEach(out, gapCount)
	default:
		w.failBatch(batch, err)
	}
	return batch[:0]
}

func entriesOf(items []item) []ledger.Entry {
	es := make([]ledger.Entry, len(items))
	for i, it := range items {
		es[i] = it.e
	}
	return es
}

// answer tells the callers waiting on an intent in items how its write went.
func answer(items []item, err error) {
	for _, it := range items {
		if it.ack != nil {
			it.ack <- ackResult{seq: it.e.Seq(), err: err}
		}
	}
}

// failBatch drops entries after a write error. The disk is at fault, so they
// are not retried. An intent is not dropped: its caller is told and refuses
// the call.
func (w *Writer) failBatch(items []item, err error) {
	answer(items, err)
	var lost int64
	for _, it := range items {
		if it.ack == nil {
			lost++
		}
	}
	if lost == 0 {
		return
	}
	w.log.Error("ledger write failed, entries dropped", "entries", lost, "error", err)
	w.drop(lost, "ledger write error: "+err.Error())
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
func (w *Writer) flushEach(out []item, gapCount int64) {
	var bad int64
	for i, it := range out {
		err := w.store.Append(it.e)
		if err == nil && gapCount > 0 && i == 0 {
			w.settleGap(gapCount)
		}
		if err == nil || errors.Is(err, ledger.ErrEncode) {
			bad += w.settleOne(it, err)
			continue
		}
		w.dropEncoded(bad)
		w.failBatch(out[i:], err)
		return
	}
	if bad > 0 {
		w.dropEncoded(bad)
		w.flush(nil) // write its gap now: nothing may come after a shutdown flush
	}
}

// settleOne answers it after its own append, which succeeded or was refused
// as not encodable. It returns 1 for a refused entry that counts as dropped.
func (w *Writer) settleOne(it item, err error) int64 {
	answer([]item{it}, err)
	if err == nil || it.ack != nil {
		return 0
	}
	w.log.Error("entry could not be encoded, dropped", "type", it.e.Type(), "error", err)
	return 1
}

func (w *Writer) dropEncoded(n int64) {
	if n > 0 {
		w.drop(n, causeEncode)
	}
}

// withGap prepends a gap entry for the calls dropped so far, if any.
func (w *Writer) withGap(batch []item) ([]item, int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dropped == 0 {
		return batch, 0
	}
	gap := ledger.Entry{"type": ledger.TypeGap, "dropped": w.dropped,
		"since": ledger.FormatTime(w.dropSince), "until": ledger.FormatTime(w.store.Now()), "cause": w.cause}
	return append([]item{{e: gap}}, batch...), w.dropped
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
