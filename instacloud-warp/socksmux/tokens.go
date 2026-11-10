package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const sessionCookie = "warp_pool_session"

type apiTokenEntry struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Token     string `json:"token"`
	CreatedAt int64  `json:"created_at"`
}

type tokenStore struct {
	mu      sync.Mutex
	path    string
	legacy  string
	entries []apiTokenEntry
}

func newTokenStore(stateDir, legacy string) *tokenStore {
	s := &tokenStore{path: filepath.Join(stateDir, "control", "tokens.json"), legacy: legacy}
	s.load()
	return s
}

func (s *tokenStore) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var entries []apiTokenEntry
	if json.Unmarshal(data, &entries) != nil {
		return
	}
	s.entries = entries
}

func (s *tokenStore) persist() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *tokenStore) list() []apiTokenEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]apiTokenEntry, len(s.entries))
	copy(out, s.entries)
	return out
}

func (s *tokenStore) matches(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.legacy != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.legacy)) == 1 {
		return true
	}
	for _, entry := range s.entries {
		if subtle.ConstantTimeCompare([]byte(token), []byte(entry.Token)) == 1 {
			return true
		}
	}
	return false
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buf)
}

func (s *tokenStore) add(name, token string) (apiTokenEntry, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "token-" + time.Now().Format("0102-1504")
	}
	if len(name) > 64 {
		return apiTokenEntry{}, errors.New("名称最长 64 字符")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		token = "pool-" + randomHex(24)
	}
	if len(token) < 16 {
		return apiTokenEntry{}, errors.New("Token 至少 16 个字符")
	}
	if len(token) > 128 {
		return apiTokenEntry{}, errors.New("Token 最长 128 个字符")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.legacy != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.legacy)) == 1 {
		return apiTokenEntry{}, errors.New("与该池的默认 Token 相同")
	}
	for _, entry := range s.entries {
		if subtle.ConstantTimeCompare([]byte(token), []byte(entry.Token)) == 1 {
			return apiTokenEntry{}, errors.New("Token 已存在")
		}
	}
	entry := apiTokenEntry{ID: "tok-" + randomHex(6), Name: name, Token: token, CreatedAt: time.Now().Unix()}
	s.entries = append(s.entries, entry)
	if err := s.persist(); err != nil {
		s.entries = s.entries[:len(s.entries)-1]
		return apiTokenEntry{}, err
	}
	return entry, nil
}

func (s *tokenStore) rename(id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("名称不能为空")
	}
	if len(name) > 64 {
		return errors.New("名称最长 64 字符")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.entries {
		if s.entries[i].ID == id {
			s.entries[i].Name = name
			return s.persist()
		}
	}
	return errors.New("Token 不存在")
}

func (s *tokenStore) remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.entries {
		if s.entries[i].ID == id {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			return s.persist()
		}
	}
	return errors.New("Token 不存在")
}

// ---- console session (username/password) ----------------------------------

func sessionKey(user, pass, token string) []byte {
	sum := sha256.Sum256([]byte("warp-pool-session|" + user + "|" + pass + "|" + token))
	return sum[:]
}

func (a *apiServer) issueSession(user string, ttl time.Duration) string {
	payload := user + "|" + strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	mac := hmac.New(sha256.New, a.sessionKey)
	mac.Write([]byte(encoded))
	return encoded + "." + hex.EncodeToString(mac.Sum(nil))
}

func (a *apiServer) validSession(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 2 {
		return false
	}
	mac := hmac.New(sha256.New, a.sessionKey)
	mac.Write([]byte(parts[0]))
	want := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(parts[1]), []byte(want)) != 1 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	fields := strings.Split(string(raw), "|")
	if len(fields) != 2 || fields[0] != a.adminUser {
		return false
	}
	exp, err := strconv.ParseInt(fields[1], 10, 64)
	return err == nil && time.Now().Unix() < exp
}

func (a *apiServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	userOK := subtle.ConstantTimeCompare([]byte(strings.TrimSpace(body.Username)), []byte(a.adminUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(body.Password), []byte(a.adminPass)) == 1
	if !userOK || !passOK {
		time.Sleep(400 * time.Millisecond)
		writeErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    a.issueSession(a.adminUser, 12*time.Hour),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
		MaxAge:   12 * 3600,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": a.adminUser})
}

func (a *apiServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func tokenSuffix(token string) string {
	if len(token) <= 4 {
		return token
	}
	return token[len(token)-4:]
}

// handleTokens lists the legacy token plus every extra token, and creates a
// new token. The full value is only returned to an authenticated console.
func (a *apiServer) handleTokens(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"legacy": map[string]any{
				"id":     "default",
				"name":   "容器默认 API_TOKEN",
				"token":  a.token,
				"suffix": tokenSuffix(a.token),
			},
			"tokens": a.tokens.list(),
		})
	case http.MethodPost:
		var body struct {
			Name  string `json:"name"`
			Token string `json:"token"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid body")
			return
		}
		entry, err := a.tokens.add(body.Name, body.Token)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, entry)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *apiServer) handleTokenItem(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" {
		writeErr(w, http.StatusNotFound, "token not found")
		return
	}
	switch r.Method {
	case http.MethodPatch, http.MethodPut:
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid body")
			return
		}
		if err := a.tokens.rename(id, body.Name); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case http.MethodDelete:
		if err := a.tokens.remove(id); err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		w.Header().Set("Allow", "PATCH, DELETE")
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

