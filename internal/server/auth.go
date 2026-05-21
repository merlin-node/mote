package server

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

const (
	sessionCookieName = "zk_session"
	sessionTTL        = 7 * 24 * time.Hour
)

type sessionManager struct {
	mu       sync.Mutex
	sessions map[string]time.Time
}

func newSessionManager() *sessionManager {
	return &sessionManager{sessions: make(map[string]time.Time)}
}

func (m *sessionManager) create() (string, time.Time, error) {
	token, err := randomSessionToken()
	if err != nil {
		return "", time.Time{}, err
	}
	expiresAt := time.Now().Add(sessionTTL)
	m.mu.Lock()
	m.sessions[token] = expiresAt
	m.mu.Unlock()
	return token, expiresAt, nil
}

func (m *sessionManager) valid(token string) bool {
	if token == "" {
		return false
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	expiresAt, ok := m.sessions[token]
	if !ok {
		return false
	}
	if now.After(expiresAt) {
		delete(m.sessions, token)
		return false
	}
	return true
}

func (m *sessionManager) revoke(token string) {
	if token == "" {
		return
	}
	m.mu.Lock()
	delete(m.sessions, token)
	m.mu.Unlock()
}

func (m *sessionManager) revokeAll() {
	m.mu.Lock()
	m.sessions = make(map[string]time.Time)
	m.mu.Unlock()
}

func (m *sessionManager) setCookie(w http.ResponseWriter, token string, expiresAt time.Time, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		Expires:  expiresAt,
		MaxAge:   int(time.Until(expiresAt).Seconds()),
	})
}

func clearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
	})
}

func randomSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
