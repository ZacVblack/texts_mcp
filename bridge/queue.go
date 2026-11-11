package main

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

const (
	maxBatchMessages = 200
	maxPendingTexts  = 20000
)

// uploadQueue collects texts and sends them to the Worker in small batches.
// Records are keyed by ID, so a text that is queued twice is sent once.
type uploadQueue struct {
	wc  *workerClient
	log zerolog.Logger

	mu       sync.Mutex
	messages map[string]TextRecord
	order    []string
	convs    map[string]ConvRecord
	deleted  map[string]struct{}
	wake     chan struct{}
	flushMu  sync.Mutex
}

func newUploadQueue(wc *workerClient, log zerolog.Logger) *uploadQueue {
	return &uploadQueue{
		wc:       wc,
		log:      log,
		messages: map[string]TextRecord{},
		convs:    map[string]ConvRecord{},
		deleted:  map[string]struct{}{},
		wake:     make(chan struct{}, 1),
	}
}

func (q *uploadQueue) poke() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *uploadQueue) AddText(r TextRecord) {
	q.mu.Lock()
	if _, exists := q.messages[r.ID]; !exists {
		if len(q.order) >= maxPendingTexts {
			// The Worker has been unreachable for a long time. Drop the oldest; the
			// next sync after it comes back fetches recent texts again anyway.
			oldest := q.order[0]
			q.order = q.order[1:]
			delete(q.messages, oldest)
		}
		q.order = append(q.order, r.ID)
	}
	q.messages[r.ID] = r
	delete(q.deleted, r.ID)
	q.mu.Unlock()
	q.poke()
}

func (q *uploadQueue) AddConversation(c ConvRecord) {
	q.mu.Lock()
	q.convs[c.ID] = c
	q.mu.Unlock()
	q.poke()
}

func (q *uploadQueue) AddDeleted(id string) {
	q.mu.Lock()
	if _, ok := q.messages[id]; ok {
		delete(q.messages, id)
		for i, v := range q.order {
			if v == id {
				q.order = append(q.order[:i], q.order[i+1:]...)
				break
			}
		}
	}
	q.deleted[id] = struct{}{}
	q.mu.Unlock()
	q.poke()
}

func (q *uploadQueue) Pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.messages) + len(q.convs) + len(q.deleted)
}

// take removes up to one batch from the queue.
func (q *uploadQueue) take() *IngestBatch {
	q.mu.Lock()
	defer q.mu.Unlock()
	b := &IngestBatch{}
	for len(b.Messages) < maxBatchMessages && len(q.order) > 0 {
		id := q.order[0]
		q.order = q.order[1:]
		if r, ok := q.messages[id]; ok {
			b.Messages = append(b.Messages, r)
			delete(q.messages, id)
		}
	}
	for id, c := range q.convs {
		b.Conversations = append(b.Conversations, c)
		delete(q.convs, id)
		if len(b.Conversations) >= 200 {
			break
		}
	}
	for id := range q.deleted {
		b.DeletedIDs = append(b.DeletedIDs, id)
		delete(q.deleted, id)
		if len(b.DeletedIDs) >= 500 {
			break
		}
	}
	return b
}

// putBack returns a batch that failed to upload, keeping anything newer.
func (q *uploadQueue) putBack(b *IngestBatch) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var front []string
	for _, r := range b.Messages {
		if _, newer := q.messages[r.ID]; newer {
			continue
		}
		if _, gone := q.deleted[r.ID]; gone {
			continue
		}
		q.messages[r.ID] = r
		front = append(front, r.ID)
	}
	q.order = append(front, q.order...)
	for _, c := range b.Conversations {
		if _, newer := q.convs[c.ID]; !newer {
			q.convs[c.ID] = c
		}
	}
	for _, id := range b.DeletedIDs {
		if _, readded := q.messages[id]; !readded {
			q.deleted[id] = struct{}{}
		}
	}
}

// Flush uploads everything queued right now. It returns the first error.
func (q *uploadQueue) Flush(ctx context.Context) error {
	q.flushMu.Lock()
	defer q.flushMu.Unlock()
	for {
		b := q.take()
		if b.empty() {
			return nil
		}
		cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		err := q.wc.Ingest(cctx, b)
		cancel()
		if err != nil {
			q.putBack(b)
			return err
		}
		q.log.Debug().Int("texts", len(b.Messages)).Int("conversations", len(b.Conversations)).
			Int("deleted", len(b.DeletedIDs)).Msg("Uploaded")
	}
}

// Run flushes shortly after new records arrive, retrying with backoff on errors.
func (q *uploadQueue) Run(ctx context.Context) {
	backoff := 2 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		case <-time.After(30 * time.Second):
		}
		// Let a burst of texts gather into one batch.
		select {
		case <-ctx.Done():
			return
		case <-time.After(1500 * time.Millisecond):
		}
		for q.Pending() > 0 {
			if err := q.Flush(ctx); err != nil {
				q.log.Warn().Err(err).Dur("retry_in", backoff).Msg("Upload to the Worker failed")
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				backoff = min(backoff*2, maxRetryWait)
				continue
			}
			backoff = 2 * time.Second
		}
	}
}
