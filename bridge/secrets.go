package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/crypto/scrypt"
)

// Token purposes. The Worker derives the same values from its PASSPHRASE secret,
// so the passphrase itself never travels over the network.
const (
	purposeBridge = "texts-mcp:bridge:v1"
	purposeMCP    = "texts-mcp:mcp:v1"
)

// deriveToken returns the first 48 hex characters of HMAC-SHA256(passphrase, purpose).
func deriveToken(passphrase, purpose string) string {
	mac := hmac.New(sha256.New, []byte(passphrase))
	mac.Write([]byte(purpose))
	return hex.EncodeToString(mac.Sum(nil))[:48]
}

// sealedSession is the encrypted Google Messages pairing as stored in the Worker
// and on disk. Only devices that know the passphrase can open it.
type sealedSession struct {
	V       int    `json:"v"`
	Salt    string `json:"salt"`
	Nonce   string `json:"nonce"`
	Data    string `json:"data"`
	SavedAt int64  `json:"saved_at"` // unix milliseconds
	// PairingID lets devices tell a new pairing from a refreshed copy of the same one.
	PairingID string `json:"pairing_id"`
}

type sessionSealer struct {
	passphrase string

	mu   sync.Mutex
	salt []byte
	key  []byte
}

func newSessionSealer(passphrase string) *sessionSealer {
	return &sessionSealer{passphrase: passphrase}
}

// scrypt cost: about 100 ms on a phone, done once per process thanks to the cache.
func (s *sessionSealer) keyFor(salt []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.key != nil && hmac.Equal(s.salt, salt) {
		return s.key, nil
	}
	key, err := scrypt.Key([]byte(s.passphrase), salt, 1<<15, 8, 1, 32)
	if err != nil {
		return nil, err
	}
	s.salt = append([]byte(nil), salt...)
	s.key = key
	return key, nil
}

func (s *sessionSealer) currentSalt() ([]byte, error) {
	s.mu.Lock()
	salt := s.salt
	s.mu.Unlock()
	if salt != nil {
		return salt, nil
	}
	salt = make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return salt, nil
}

func (s *sessionSealer) Seal(plain []byte, pairingID string) (*sealedSession, error) {
	salt, err := s.currentSalt()
	if err != nil {
		return nil, err
	}
	key, err := s.keyFor(salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, plain, []byte("texts-mcp-session-v1"))
	return &sealedSession{
		V:         1,
		Salt:      base64.StdEncoding.EncodeToString(salt),
		Nonce:     base64.StdEncoding.EncodeToString(nonce),
		Data:      base64.StdEncoding.EncodeToString(ct),
		SavedAt:   time.Now().UnixMilli(),
		PairingID: pairingID,
	}, nil
}

var errWrongPassphrase = errors.New("could not open the saved pairing; the passphrase is different from the one used when pairing")

func (s *sessionSealer) Open(ss *sealedSession) ([]byte, error) {
	if ss == nil {
		return nil, errors.New("no saved pairing")
	}
	if ss.V != 1 {
		return nil, fmt.Errorf("saved pairing has unknown format version %d", ss.V)
	}
	salt, err := base64.StdEncoding.DecodeString(ss.Salt)
	if err != nil {
		return nil, fmt.Errorf("saved pairing salt: %w", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(ss.Nonce)
	if err != nil {
		return nil, fmt.Errorf("saved pairing nonce: %w", err)
	}
	ct, err := base64.StdEncoding.DecodeString(ss.Data)
	if err != nil {
		return nil, fmt.Errorf("saved pairing data: %w", err)
	}
	key, err := s.keyFor(salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, errors.New("saved pairing nonce has the wrong length")
	}
	plain, err := gcm.Open(nil, nonce, ct, []byte("texts-mcp-session-v1"))
	if err != nil {
		return nil, errWrongPassphrase
	}
	return plain, nil
}

func (ss *sealedSession) marshal() ([]byte, error) {
	return json.Marshal(ss)
}
