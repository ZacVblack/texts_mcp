package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// Photos in texts. The bridge downloads each photo from Google Messages, makes
// it small enough (photoimg.go) and uploads it to the Worker, so Claude can look
// at it. Only photos from the last photoMaxAgeDays are fetched.
const (
	photoMaxAgeDays    = 14
	photoMaxTries      = 5
	photoBatch         = 20
	photoCheckEvery    = time.Minute
	photoBackfillEvery = 24 * time.Hour
	// The backfill looks this far back in each recently active conversation.
	photoBackfillConversations = 60
	photoBackfillPages         = 3
	// When the Worker doesn't know about photos yet (an older Worker), wait
	// this long before asking again.
	photoUnsupportedWait = 10 * time.Minute
	// A photo Google sends inline (no download needed) bigger than this is
	// not kept in the queue.
	photoMaxInlineBytes = 3 << 20
)

// photoJob is one photo waiting to be fetched and uploaded.
type photoJob struct {
	ID             string `json:"id"`
	MessageID      string `json:"message_id"`
	ConversationID string `json:"conversation_id"`
	TsMs           int64  `json:"ts"`
	MediaID        string `json:"media_id"`
	Key            []byte `json:"key"`
	// Inline holds the photo itself when Google sent it with the message.
	Inline []byte `json:"inline,omitempty"`
	// Thumb marks a preview: Google only has the full photo once the phone
	// uploads it, which the bridge asks for after sending the preview.
	Thumb bool `json:"thumb,omitempty"`
	// PartID is the attachment's own ID, used to ask for the full photo.
	PartID string `json:"part_id,omitempty"`
	Tries  int    `json:"tries,omitempty"`
}

// photosNoMedia counts photo attachments seen with nothing to download yet
// (not even a preview), for the heartbeat.
var photosNoMedia atomic.Int64

// doneKey keeps a preview and the full photo apart, so the full photo is
// still fetched after its preview went up.
func (j photoJob) doneKey() string {
	if j.Thumb {
		return j.ID + "#preview"
	}
	return j.ID
}

func photoCutoff(now time.Time) int64 {
	return now.Add(-photoMaxAgeDays * 24 * time.Hour).UnixMilli()
}

// photoJobsFor lists the photos in a message. Each gets the ID
// "<message id>.<n>", n counting the message's attachments from 0.
func photoJobsFor(m *gmproto.Message) []photoJob {
	var jobs []photoJob
	n := 0
	for _, info := range m.GetMessageInfo() {
		media := info.GetMediaContent()
		if media == nil {
			continue
		}
		idx := n
		n++
		if mediaKind(media.GetMimeType(), media.GetFormat()) != "photo" {
			continue
		}
		j := photoJob{
			ID:             m.GetMessageID() + "." + strconv.Itoa(idx),
			MessageID:      m.GetMessageID(),
			ConversationID: m.GetConversationID(),
			TsMs:           m.GetTimestamp() / 1000,
			PartID:         info.GetActionMessageID(),
		}
		// Google often has only a preview of a photo until the phone uploads
		// the full one (many MMS photos), so take the best that is there.
		switch {
		case media.GetMediaID() != "" && len(media.GetDecryptionKey()) > 0:
			j.MediaID, j.Key = media.GetMediaID(), media.GetDecryptionKey()
		case media.GetThumbnailMediaID() != "" && len(media.GetThumbnailDecryptionKey()) > 0:
			j.MediaID, j.Key, j.Thumb = media.GetThumbnailMediaID(), media.GetThumbnailDecryptionKey(), true
		case len(media.GetMediaData()) > 0 && len(media.GetMediaData()) <= photoMaxInlineBytes:
			j.Inline, j.Thumb = media.GetMediaData(), true
		default:
			// Still downloading on the phone; a later update brings it.
			photosNoMedia.Add(1)
			continue
		}
		jobs = append(jobs, j)
	}
	return jobs
}

// photoQueue holds photos waiting to go up. It is saved on the phone, so a
// restart or a dead battery doesn't lose them.
type photoQueue struct {
	log  zerolog.Logger
	path string

	mu      sync.Mutex
	pending map[string]photoJob
	done    map[string]bool
	wake    chan struct{}
}

func photoQueuePath() string { return filepath.Join(defaultConfigDir(), "photos-pending.json") }

func newPhotoQueue(path string, log zerolog.Logger) *photoQueue {
	q := &photoQueue{log: log, path: path, pending: map[string]photoJob{}, done: map[string]bool{}, wake: make(chan struct{}, 1)}
	if data, err := os.ReadFile(path); err == nil {
		var jobs []photoJob
		if json.Unmarshal(data, &jobs) == nil {
			cutoff := photoCutoff(time.Now())
			for _, j := range jobs {
				if j.ID != "" && j.TsMs >= cutoff {
					q.pending[j.ID] = j
				}
			}
		}
	}
	return q
}

func (q *photoQueue) poke() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Add queues photos that are recent enough and not already handled.
func (q *photoQueue) Add(jobs ...photoJob) {
	if q == nil || len(jobs) == 0 {
		return
	}
	cutoff := photoCutoff(time.Now())
	added := false
	q.mu.Lock()
	for _, j := range jobs {
		if j.TsMs < cutoff || q.done[j.doneKey()] || (j.Thumb && q.done[j.ID]) {
			continue
		}
		if old, ok := q.pending[j.ID]; ok {
			if j.Thumb && !old.Thumb {
				continue // already waiting for the full photo
			}
			if j.Thumb == old.Thumb {
				// Keep the try count; take the newest media details.
				j.Tries = old.Tries
			}
		}
		q.pending[j.ID] = j
		added = true
	}
	q.mu.Unlock()
	if added {
		q.save()
		q.poke()
	}
}

// next returns up to n waiting photos, newest first: recent photos matter most.
func (q *photoQueue) next(n int) []photoJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	cutoff := photoCutoff(time.Now())
	jobs := make([]photoJob, 0, len(q.pending))
	for id, j := range q.pending {
		if j.TsMs < cutoff {
			delete(q.pending, id)
			continue
		}
		jobs = append(jobs, j)
	}
	sort.Slice(jobs, func(a, b int) bool { return jobs[a].TsMs > jobs[b].TsMs })
	if len(jobs) > n {
		jobs = jobs[:n]
	}
	return jobs
}

// finish drops a photo from the queue, uploaded or given up on. A newer
// version of it queued meanwhile (the full photo after a preview) stays.
func (q *photoQueue) finish(j photoJob) {
	q.mu.Lock()
	if cur, ok := q.pending[j.ID]; ok && cur.Thumb == j.Thumb {
		delete(q.pending, j.ID)
	}
	q.done[j.doneKey()] = true
	q.mu.Unlock()
	q.save()
}

// retry counts a failed try and gives up after photoMaxTries. It reports
// whether the photo was dropped.
func (q *photoQueue) retry(id string) bool {
	q.mu.Lock()
	j, ok := q.pending[id]
	dropped := false
	if ok {
		j.Tries++
		if j.Tries >= photoMaxTries {
			delete(q.pending, id)
			q.done[j.doneKey()] = true
			dropped = true
		} else {
			q.pending[id] = j
		}
	}
	q.mu.Unlock()
	q.save()
	return dropped
}

func (q *photoQueue) Pending() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}

func (q *photoQueue) save() {
	if q.path == "" {
		return
	}
	q.mu.Lock()
	jobs := make([]photoJob, 0, len(q.pending))
	for _, j := range q.pending {
		jobs = append(jobs, j)
	}
	q.mu.Unlock()
	data, _ := json.Marshal(jobs)
	_ = os.MkdirAll(filepath.Dir(q.path), 0o700)
	if err := writeFileAtomic(q.path, data, 0o600); err != nil {
		q.log.Debug().Err(err).Msg("Could not save the photo queue")
	}
}

// photoLoop uploads waiting photos while this session is connected.
func (s *session) photoLoop(ctx context.Context) {
	q := s.r.photos
	if q == nil {
		return
	}
	// Let the first sync go first.
	if !sleepCtx(ctx, 30*time.Second) {
		return
	}
	if time.Since(time.UnixMilli(s.r.photoBackfillAt())) >= photoBackfillEvery {
		if err := s.photoBackfill(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Warn().Err(err).Msg("Could not look for recent photos; will try again next time")
		}
	}
	ticker := time.NewTicker(photoCheckEvery)
	defer ticker.Stop()
	warnedOld := false
	for {
		if s.r.phoneResponding.Load() {
			err := s.uploadPhotos(ctx)
			switch {
			case ctx.Err() != nil:
				return
			case errors.Is(err, errPhotosUnsupported):
				if !warnedOld {
					s.log.Warn().Msg("The Worker doesn't take photos yet (it updates itself from GitHub); trying again in 10 minutes")
					warnedOld = true
				}
				if !sleepCtx(ctx, photoUnsupportedWait) {
					return
				}
				continue
			case err != nil:
				s.log.Debug().Err(err).Msg("Photo upload paused")
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-q.wake:
			// Let a burst of texts settle.
			if !sleepCtx(ctx, 3*time.Second) {
				return
			}
		}
	}
}

// uploadPhotos works through the queue until it is empty or something fails.
func (s *session) uploadPhotos(ctx context.Context) error {
	q := s.r.photos
	for ctx.Err() == nil {
		jobs := q.next(photoBatch)
		if len(jobs) == 0 {
			return nil
		}
		ids := make([]string, len(jobs))
		var full []string
		for i, j := range jobs {
			ids[i] = j.ID
			if !j.Thumb {
				full = append(full, j.ID)
			}
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		missing, err := s.r.wc.PhotosMissing(cctx, ids, full)
		cancel()
		if err != nil {
			return err
		}
		want := map[string]bool{}
		for _, id := range missing {
			want[id] = true
		}
		for _, j := range jobs {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !want[j.ID] {
				q.finish(j)
				if j.Thumb {
					// Its preview is there; make sure the full photo is asked for.
					s.requestFullPhoto(ctx, j)
				}
				continue
			}
			if err := s.uploadPhoto(ctx, j); err != nil {
				if errors.Is(err, errPhotosUnsupported) {
					return err
				}
				if errors.Is(err, errPhotoFormat) {
					s.log.Info().Err(err).Str("photo", j.ID).Msg("Skipping a photo Claude can't be shown")
					s.r.photosSkipped.Add(1)
					s.r.setPhotoProblem("skipped: " + err.Error())
					q.finish(j)
					continue
				}
				if q.retry(j.ID) {
					s.log.Warn().Err(err).Str("photo", j.ID).Msg("Giving up on a photo after several tries")
					s.r.photosFailed.Add(1)
					s.r.setPhotoProblem("gave up: " + err.Error())
				} else {
					s.log.Debug().Err(err).Str("photo", j.ID).Msg("Photo upload failed; will retry")
				}
				// A failure usually means the network dropped; stop this round.
				return err
			}
			q.finish(j)
			s.log.Debug().Str("photo", j.ID).Bool("preview", j.Thumb).Msg("Photo uploaded")
			if j.Thumb {
				s.r.photosPreview.Add(1)
				s.requestFullPhoto(ctx, j)
			}
			if !sleepCtx(ctx, 500*time.Millisecond) {
				return ctx.Err()
			}
		}
	}
	return ctx.Err()
}

func (s *session) uploadPhoto(ctx context.Context, j photoJob) error {
	raw := j.Inline
	if len(raw) == 0 {
		var err error
		if raw, err = s.downloadPhoto(ctx, j); err != nil {
			return err
		}
	}
	p, err := preparePhoto(raw)
	if err != nil {
		return err
	}
	uctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return s.r.wc.UploadPhoto(uctx, &PhotoUpload{
		ID:             j.ID,
		MessageID:      j.MessageID,
		ConversationID: j.ConversationID,
		TsMs:           j.TsMs,
		Mime:           p.Mime,
		Width:          p.Width,
		Height:         p.Height,
		Data:           base64.StdEncoding.EncodeToString(p.Data),
		Thumb:          j.Thumb,
	})
}

// requestFullPhoto asks the phone to upload the full photo behind a preview,
// once per photo while the bridge runs. Google then sends the message again
// with the full photo, which is queued like any new photo.
func (s *session) requestFullPhoto(ctx context.Context, j photoJob) {
	if j.PartID == "" || ctx.Err() != nil {
		return
	}
	key := j.MessageID + "/" + j.PartID
	s.r.fullAskedMu.Lock()
	if s.r.fullAsked == nil {
		s.r.fullAsked = map[string]bool{}
	}
	asked := s.r.fullAsked[key]
	s.r.fullAsked[key] = true
	s.r.fullAskedMu.Unlock()
	if asked {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := s.client.GetFullSizeImage(rctx, j.MessageID, j.PartID); err != nil {
		s.log.Debug().Err(err).Str("photo", j.ID).Msg("Could not ask the phone for the full photo")
		return
	}
	s.r.photosFullAsked.Add(1)
	s.log.Debug().Str("photo", j.ID).Msg("Asked the phone for the full photo")
}

// downloadPhoto fetches and decrypts one photo. libgm's download takes no
// context, so a stuck download is abandoned after a few minutes.
func (s *session) downloadPhoto(ctx context.Context, j photoJob) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		rc, err := s.client.DownloadMedia(j.MediaID, j.Key)
		if err != nil {
			ch <- result{err: err}
			return
		}
		defer rc.Close()
		data, err := io.ReadAll(io.LimitReader(rc, photoMaxSourceBytes+1))
		if err == nil && len(data) > photoMaxSourceBytes {
			err = errPhotoFormat
		}
		ch <- result{data, err}
	}()
	select {
	case r := <-ch:
		return r.data, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(4 * time.Minute):
		return nil, errors.New("photo download timed out")
	}
}

// photoBackfill looks through recently active conversations for photos from
// the last photoMaxAgeDays, so photos that arrived before this version (or
// while the queue was lost) are fetched too. Runs at most once a day.
func (s *session) photoBackfill(ctx context.Context) error {
	syncMu.Lock()
	defer syncMu.Unlock()
	cutoff := photoCutoff(time.Now())
	found := 0
	checked := 0
	for _, folder := range []gmproto.ListConversationsRequest_Folder{
		gmproto.ListConversationsRequest_INBOX, gmproto.ListConversationsRequest_ARCHIVE,
	} {
		lctx, cancel := context.WithTimeout(ctx, time.Minute)
		resp, err := s.client.ListConversations(lctx, &gmproto.ListConversationsRequest{Count: syncConversations, Folder: folder})
		cancel()
		if err != nil {
			return err
		}
		for _, c := range resp.GetConversations() {
			if skipConversation(c) || c.GetLastMessageTimestamp()/1000 < cutoff {
				continue
			}
			if checked >= photoBackfillConversations {
				break
			}
			checked++
			n, err := s.photoBackfillConversation(ctx, c.GetConversationID(), cutoff)
			if err != nil {
				return err
			}
			found += n
		}
	}
	s.r.setPhotoBackfillAt(time.Now().UnixMilli())
	s.log.Info().Int("conversations", checked).Int("photos", found).Msg("Checked recent conversations for photos")
	return nil
}

func (s *session) photoBackfillConversation(ctx context.Context, convID string, cutoff int64) (int, error) {
	var cursor *gmproto.Cursor
	found := 0
	for page := 0; page < photoBackfillPages; page++ {
		fctx, cancel := context.WithTimeout(ctx, time.Minute)
		resp, err := s.client.FetchMessages(fctx, convID, syncPageSize, cursor)
		cancel()
		if err != nil {
			return found, err
		}
		msgs := resp.GetMessages()
		oldest := int64(1<<62 - 1)
		for _, m := range msgs {
			ts := m.GetTimestamp() / 1000
			oldest = min(oldest, ts)
			if ts < cutoff || classifyMessage(m) != actionStore {
				continue
			}
			jobs := photoJobsFor(m)
			found += len(jobs)
			s.r.photos.Add(jobs...)
		}
		next := resp.GetCursor()
		if len(msgs) < syncPageSize || oldest < cutoff || next == nil || next.GetLastItemID() == "" {
			break
		}
		cursor = next
		if !sleepCtx(ctx, 300*time.Millisecond) {
			return found, ctx.Err()
		}
	}
	return found, nil
}

func (r *runner) photoBackfillAt() int64 {
	r.stMu.Lock()
	defer r.stMu.Unlock()
	return r.st.PhotoBackfillAtMs
}

func (r *runner) setPhotoBackfillAt(ms int64) {
	r.stMu.Lock()
	r.st.PhotoBackfillAtMs = ms
	st := r.st
	r.stMu.Unlock()
	saveBridgeState(st)
}

func (r *runner) setPhotoProblem(msg string) {
	if len(msg) > 200 {
		msg = msg[:200]
	}
	r.photoProblem.Store(msg)
}

func (r *runner) lastPhotoProblem() string {
	v, _ := r.photoProblem.Load().(string)
	return v
}
