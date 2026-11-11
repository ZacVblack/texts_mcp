package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// workerClient talks to the bridge endpoints of the Cloudflare Worker.
type workerClient struct {
	base  string
	token string
	http  *http.Client
}

func newWorkerClient(cfg *Config) *workerClient {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = newDialer().DialContext
	return &workerClient{
		base:  cfg.WorkerURL,
		token: deriveToken(cfg.Passphrase, purposeBridge),
		http:  &http.Client{Timeout: 60 * time.Second, Transport: tr},
	}
}

var errNoSessionInWorker = errors.New("the Worker has no saved pairing yet")

var errWorkerRefused = errors.New("the Worker refused the passphrase (check that the PASSPHRASE secret matches the passphrase you typed in setup)")

type workerStatus struct {
	OK            bool   `json:"ok"`
	HasSession    bool   `json:"has_session"`
	State         string `json:"state"`
	LastHeartbeat string `json:"last_heartbeat"`
}

func (w *workerClient) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, w.base+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+w.token)
	req.Header.Set("User-Agent", "texts-bridge/"+Version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := w.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return resp.StatusCode, errWorkerRefused
	}
	if resp.StatusCode == http.StatusNotFound && path == "/bridge/session" && method == http.MethodGet {
		return resp.StatusCode, errNoSessionInWorker
	}
	if resp.StatusCode == http.StatusNotFound {
		return resp.StatusCode, fmt.Errorf("the Worker answered 404 for %s; check the Worker address", path)
	}
	if resp.StatusCode >= 300 {
		msg := string(data)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return resp.StatusCode, fmt.Errorf("worker answered HTTP %d: %s", resp.StatusCode, msg)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("worker answer was not JSON: %w", err)
		}
	}
	return resp.StatusCode, nil
}

func (w *workerClient) Ping(ctx context.Context) (*workerStatus, error) {
	var st workerStatus
	_, err := w.do(ctx, http.MethodGet, "/bridge/ping", nil, &st)
	if err != nil {
		return nil, err
	}
	if !st.OK {
		return nil, errors.New("unexpected answer from the Worker")
	}
	return &st, nil
}

func (w *workerClient) GetSession(ctx context.Context) (*sealedSession, error) {
	var ss sealedSession
	_, err := w.do(ctx, http.MethodGet, "/bridge/session", nil, &ss)
	if err != nil {
		return nil, err
	}
	return &ss, nil
}

func (w *workerClient) PutSession(ctx context.Context, ss *sealedSession) error {
	_, err := w.do(ctx, http.MethodPut, "/bridge/session", ss, nil)
	return err
}

// IngestBatch is one upload of texts and conversation details.
type IngestBatch struct {
	Messages      []TextRecord `json:"messages,omitempty"`
	Conversations []ConvRecord `json:"conversations,omitempty"`
	DeletedIDs    []string     `json:"deleted_ids,omitempty"`
}

func (b *IngestBatch) empty() bool {
	return len(b.Messages) == 0 && len(b.Conversations) == 0 && len(b.DeletedIDs) == 0
}

func (w *workerClient) Ingest(ctx context.Context, b *IngestBatch) error {
	_, err := w.do(ctx, http.MethodPost, "/bridge/ingest", b, nil)
	return err
}

// Heartbeat tells the Worker how the link is doing, so Claude can say when it is down.
type Heartbeat struct {
	State          string `json:"state"`
	Detail         string `json:"detail,omitempty"`
	PhoneResponds  bool   `json:"phone_responding"`
	BatteryLow     bool   `json:"battery_low"`
	LastTextAtMs   int64  `json:"last_text_at_ms,omitempty"`
	LastSyncAtMs   int64  `json:"last_sync_at_ms,omitempty"`
	BridgeVersion  string `json:"bridge_version"`
	PendingUploads int    `json:"pending_uploads"`
}

func (w *workerClient) SendHeartbeat(ctx context.Context, hb *Heartbeat) error {
	_, err := w.do(ctx, http.MethodPost, "/bridge/heartbeat", hb, nil)
	return err
}
