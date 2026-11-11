package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/util/exhttp"
)

func httpSettings() exhttp.ClientSettings {
	return exhttp.SensibleClientSettings.WithDial(newDialer().DialContext)
}

// sessionStore keeps the encrypted pairing in two places: a local file on this
// device and the Worker. The newest copy wins when loading.
type sessionStore struct {
	cfg    *Config
	wc     *workerClient
	sealer *sessionSealer
	log    zerolog.Logger
	path   string
}

func newSessionStore(cfg *Config, wc *workerClient, log zerolog.Logger) *sessionStore {
	return &sessionStore{
		cfg:    cfg,
		wc:     wc,
		sealer: newSessionSealer(cfg.Passphrase),
		log:    log,
		path:   filepath.Join(defaultConfigDir(), "session.json"),
	}
}

func (s *sessionStore) readLocal() *sealedSession {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil
	}
	var ss sealedSession
	if json.Unmarshal(data, &ss) != nil {
		return nil
	}
	return &ss
}

func (s *sessionStore) RemoveLocal() {
	_ = os.Remove(s.path)
}

// Load returns the newest saved pairing, or errNoSessionInWorker when there is none.
func (s *sessionStore) Load(ctx context.Context) (*libgm.AuthData, *sealedSession, error) {
	local := s.readLocal()
	remote, err := s.wc.GetSession(ctx)
	if err != nil && !errors.Is(err, errNoSessionInWorker) {
		if local == nil {
			return nil, nil, err
		}
		s.log.Warn().Err(err).Msg("Could not reach the Worker; using the pairing saved on this device")
	}
	chosen := local
	if remote != nil && (chosen == nil || remote.SavedAt > chosen.SavedAt) {
		chosen = remote
	}
	if chosen == nil {
		return nil, nil, errNoSessionInWorker
	}
	plain, err := s.sealer.Open(chosen)
	if err != nil {
		return nil, nil, err
	}
	ad := libgm.NewAuthData()
	if err := json.Unmarshal(plain, ad); err != nil {
		return nil, nil, fmt.Errorf("saved pairing could not be read: %w", err)
	}
	return ad, chosen, nil
}

func marshalAuthData(ad *libgm.AuthData) ([]byte, error) {
	ad.CookiesLock.RLock()
	defer ad.CookiesLock.RUnlock()
	return json.Marshal(ad)
}

// Save writes the pairing locally and to the Worker. With requireRemote, a Worker
// failure is an error; otherwise it is logged and retried on the next save.
func (s *sessionStore) Save(ctx context.Context, ad *libgm.AuthData, requireRemote bool) (*sealedSession, error) {
	plain, err := marshalAuthData(ad)
	if err != nil {
		return nil, err
	}
	ss, err := s.sealer.Seal(plain, ad.PairingID.String())
	if err != nil {
		return nil, err
	}
	data, err := ss.marshal()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(s.path, data, 0o600); err != nil {
		s.log.Warn().Err(err).Msg("Could not save the pairing on this device")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.wc.PutSession(ctx, ss); err != nil {
		if requireRemote {
			return nil, err
		}
		s.log.Warn().Err(err).Msg("Could not save the pairing to the Worker; will retry")
	}
	return ss, nil
}

// bridgeState is small bookkeeping kept between runs.
type bridgeState struct {
	WatermarkMs int64 `json:"watermark_ms"`
}

func statePath() string { return filepath.Join(defaultConfigDir(), "state.json") }

func loadBridgeState() bridgeState {
	var st bridgeState
	data, err := os.ReadFile(statePath())
	if err == nil {
		_ = json.Unmarshal(data, &st)
	}
	return st
}

func saveBridgeState(st bridgeState) {
	data, _ := json.Marshal(st)
	_ = os.MkdirAll(defaultConfigDir(), 0o700)
	_ = writeFileAtomic(statePath(), data, 0o600)
}
