package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	sessionCookieName = "lfh_session"
	maxSessions       = 1000
	sessionLifetime   = 12 * time.Hour
	sessionIdleLimit  = 2 * time.Hour
)

//go:embed web/index.html
var webFiles embed.FS

type session struct {
	ID       string
	Username string
	CSRF     string
	Created  time.Time
	LastSeen time.Time
}

type loginAttempt struct {
	WindowStart time.Time
	Count       int
	BlockedTill time.Time
}

type contextKey string

const sessionContextKey contextKey = "session"

type App struct {
	dataDir    string
	configPath string
	listenAddr string
	version    string

	configMu sync.RWMutex
	config   *Config

	sessionsMu  sync.Mutex
	sessions    map[string]*session
	attempts    map[string]*loginAttempt
	uploadSlots chan struct{}

	serverMu sync.RWMutex
	server   *http.Server
	stopOnce sync.Once
}

func newApp(dataDir, listenAddr, version string, cfg *Config) *App {
	return &App{
		dataDir:     dataDir,
		configPath:  filepath.Join(dataDir, "config.json"),
		listenAddr:  listenAddr,
		version:     version,
		config:      cfg,
		sessions:    make(map[string]*session),
		attempts:    make(map[string]*loginAttempt),
		uploadSlots: make(chan struct{}, 4),
	}
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleIndex)
	mux.HandleFunc("/api/health", requireMethod(http.MethodGet, a.handleHealth))
	mux.HandleFunc("/api/status", requireMethod(http.MethodGet, a.handleStatus))
	mux.HandleFunc("/api/setup", requireMethod(http.MethodPost, a.handleSetup))
	mux.HandleFunc("/api/pick-folder", requireMethod(http.MethodPost, a.handlePickFolder))
	mux.HandleFunc("/api/login", requireMethod(http.MethodPost, a.handleLogin))
	mux.HandleFunc("/api/logout", requireMethod(http.MethodPost, a.authWrite(a.handleLogout)))
	mux.HandleFunc("/api/me", requireMethod(http.MethodGet, a.authRead(a.handleMe)))
	mux.HandleFunc("/api/files", requireMethod(http.MethodGet, a.authRead(a.handleList)))
	mux.HandleFunc("/api/download", requireMethod(http.MethodGet, a.authRead(a.handleDownload)))
	mux.HandleFunc("/api/upload", requireMethod(http.MethodPost, a.authWrite(a.handleUpload)))
	mux.HandleFunc("/api/mkdir", requireMethod(http.MethodPost, a.authWrite(a.handleMkdir)))
	mux.HandleFunc("/api/rename", requireMethod(http.MethodPost, a.authWrite(a.handleRename)))
	mux.HandleFunc("/api/delete", requireMethod(http.MethodPost, a.authWrite(a.handleDelete)))
	mux.HandleFunc("/api/settings/folder", requireMethod(http.MethodPost, a.authWrite(a.handleChangeFolder)))
	mux.HandleFunc("/api/shutdown", requireMethod(http.MethodPost, a.authWrite(a.handleShutdown)))
	return a.securityHeaders(a.recoverPanics(mux))
}

func requireMethod(method string, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			writeAPIError(w, http.StatusMethodNotAllowed, "请求方法不支持")
			return
		}
		handler(w, r)
	}
}

func (a *App) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func (a *App) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("请求处理异常: %v\n%s", recovered, debug.Stack())
				writeAPIError(w, http.StatusInternalServerError, "服务器处理请求时发生异常")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := webFiles.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "页面资源不可用", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (a *App) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("X-Lan-File-Hub", "1")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": a.version})
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	configured := a.isConfigured()
	local := isLocalRequest(r)
	response := map[string]any{
		"configured": configured,
		"local":      local,
		"version":    a.version,
		"addresses":  a.accessAddresses(),
	}
	if configured {
		cfg := a.configSnapshot()
		_, err := os.Stat(cfg.ShareDir)
		response["shareAvailable"] = err == nil
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *App) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeAPIError(w, http.StatusForbidden, "请在主电脑上完成首次设置")
		return
	}
	if !sameOrigin(r) {
		writeAPIError(w, http.StatusForbidden, "请求来源无效")
		return
	}
	if a.isConfigured() {
		writeAPIError(w, http.StatusConflict, "程序已经完成设置")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Folder   string `json:"folder"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	username, err := validateUsername(input.Username)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	folder, err := validateShareDirectory(input.Folder)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	password, err := newPasswordRecord(input.Password)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg := &Config{Version: configVersion, Username: username, Password: password, ShareDir: folder}
	if err := saveConfig(a.configPath, cfg); err != nil {
		log.Printf("保存配置失败: %v", err)
		writeAPIError(w, http.StatusInternalServerError, "无法保存设置")
		return
	}
	a.configMu.Lock()
	a.config = cfg
	a.configMu.Unlock()
	sess, err := a.createSession(username)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "创建登录状态失败")
		return
	}
	setSessionCookie(w, sess.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "csrf": sess.CSRF})
}

func (a *App) handlePickFolder(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeAPIError(w, http.StatusForbidden, "只能在主电脑上选择文件夹")
		return
	}
	if !sameOrigin(r) {
		writeAPIError(w, http.StatusForbidden, "请求来源无效")
		return
	}
	if a.isConfigured() {
		sess := a.sessionFromRequest(r)
		if sess == nil || r.Header.Get("X-CSRF-Token") != sess.CSRF {
			writeAPIError(w, http.StatusUnauthorized, "请先登录")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	folder, err := pickFolder(ctx)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"folder": folder})
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeAPIError(w, http.StatusForbidden, "请求来源无效")
		return
	}
	if !a.isConfigured() {
		writeAPIError(w, http.StatusPreconditionRequired, "请先在主电脑完成首次设置")
		return
	}
	ip := remoteIP(r)
	if !a.loginAllowed(ip) {
		writeAPIError(w, http.StatusTooManyRequests, "登录尝试过多，请 5 分钟后再试")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	cfg := a.configSnapshot()
	usernameOK := subtleUsernameCompare(cfg.Username, strings.TrimSpace(input.Username))
	passwordOK := verifyPassword(cfg.Password, input.Password)
	if !usernameOK || !passwordOK {
		a.recordLoginFailure(ip)
		writeAPIError(w, http.StatusUnauthorized, "账号或密码错误")
		return
	}
	a.clearLoginFailures(ip)
	sess, err := a.createSession(cfg.Username)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "创建登录状态失败")
		return
	}
	setSessionCookie(w, sess.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "csrf": sess.CSRF})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		a.sessionsMu.Lock()
		delete(a.sessions, cookie.Value)
		a.sessionsMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) handleMe(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	response := map[string]any{
		"username":  sess.Username,
		"csrf":      sess.CSRF,
		"local":     isLocalRequest(r),
		"addresses": a.accessAddresses(),
	}
	if isLocalRequest(r) {
		response["shareDir"] = a.shareDirectory()
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *App) handleChangeFolder(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeAPIError(w, http.StatusForbidden, "只能在主电脑上修改共享目录")
		return
	}
	var input struct {
		Folder string `json:"folder"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	folder, err := validateShareDirectory(input.Folder)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg := a.configSnapshot()
	cfg.ShareDir = folder
	if err := saveConfig(a.configPath, cfg); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "保存共享目录失败")
		return
	}
	a.configMu.Lock()
	a.config = cfg
	a.configMu.Unlock()
	a.audit(r, "change-share-folder", folder)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeAPIError(w, http.StatusForbidden, "只能在主电脑上停止服务")
		return
	}
	a.audit(r, "shutdown", "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	a.stopOnce.Do(func() {
		go func() {
			time.Sleep(300 * time.Millisecond)
			a.serverMu.RLock()
			server := a.server
			a.serverMu.RUnlock()
			if server == nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = server.Shutdown(ctx)
		}()
	})
}

func (a *App) authRead(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := a.sessionFromRequest(r)
		if sess == nil {
			writeAPIError(w, http.StatusUnauthorized, "登录已失效，请重新登录")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), sessionContextKey, sess)))
	}
}

func (a *App) authWrite(next http.HandlerFunc) http.HandlerFunc {
	return a.authRead(func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			writeAPIError(w, http.StatusForbidden, "请求来源无效")
			return
		}
		sess := sessionFromContext(r.Context())
		if sess == nil || r.Header.Get("X-CSRF-Token") != sess.CSRF {
			writeAPIError(w, http.StatusForbidden, "页面状态已失效，请刷新后重试")
			return
		}
		next(w, r)
	})
}

func sessionFromContext(ctx context.Context) *session {
	sess, _ := ctx.Value(sessionContextKey).(*session)
	return sess
}

func (a *App) createSession(username string) (*session, error) {
	id, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	csrf, err := randomToken(24)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	sess := &session{ID: id, Username: username, CSRF: csrf, Created: now, LastSeen: now}
	a.sessionsMu.Lock()
	defer a.sessionsMu.Unlock()
	if len(a.sessions) >= maxSessions {
		var oldestID string
		var oldest time.Time
		for existingID, existing := range a.sessions {
			if oldestID == "" || existing.LastSeen.Before(oldest) {
				oldestID, oldest = existingID, existing.LastSeen
			}
		}
		delete(a.sessions, oldestID)
	}
	a.sessions[id] = sess
	copy := *sess
	return &copy, nil
}

func (a *App) sessionFromRequest(r *http.Request) *session {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || len(cookie.Value) > 128 {
		return nil
	}
	now := time.Now()
	a.sessionsMu.Lock()
	defer a.sessionsMu.Unlock()
	sess := a.sessions[cookie.Value]
	if sess == nil || now.Sub(sess.Created) > sessionLifetime || now.Sub(sess.LastSeen) > sessionIdleLimit {
		delete(a.sessions, cookie.Value)
		return nil
	}
	sess.LastSeen = now
	copy := *sess
	return &copy
}

func setSessionCookie(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    id,
		Path:     "/",
		MaxAge:   int(sessionLifetime.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

func (a *App) sessionJanitor(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			a.sessionsMu.Lock()
			for id, sess := range a.sessions {
				if now.Sub(sess.Created) > sessionLifetime || now.Sub(sess.LastSeen) > sessionIdleLimit {
					delete(a.sessions, id)
				}
			}
			for ip, attempt := range a.attempts {
				if now.Sub(attempt.WindowStart) > 15*time.Minute && now.After(attempt.BlockedTill) {
					delete(a.attempts, ip)
				}
			}
			a.sessionsMu.Unlock()
		}
	}
}

func (a *App) loginAllowed(ip string) bool {
	now := time.Now()
	a.sessionsMu.Lock()
	defer a.sessionsMu.Unlock()
	attempt := a.attempts[ip]
	return attempt == nil || !now.Before(attempt.BlockedTill)
}

func (a *App) recordLoginFailure(ip string) {
	now := time.Now()
	a.sessionsMu.Lock()
	defer a.sessionsMu.Unlock()
	if len(a.attempts) >= maxSessions {
		for key, attempt := range a.attempts {
			if now.Sub(attempt.WindowStart) > 15*time.Minute {
				delete(a.attempts, key)
			}
		}
		if len(a.attempts) >= maxSessions {
			for key := range a.attempts {
				delete(a.attempts, key)
				break
			}
		}
	}
	attempt := a.attempts[ip]
	if attempt == nil || now.Sub(attempt.WindowStart) > 5*time.Minute {
		attempt = &loginAttempt{WindowStart: now}
		a.attempts[ip] = attempt
	}
	attempt.Count++
	if attempt.Count >= 10 {
		attempt.BlockedTill = now.Add(5 * time.Minute)
	}
}

func (a *App) clearLoginFailures(ip string) {
	a.sessionsMu.Lock()
	delete(a.attempts, ip)
	a.sessionsMu.Unlock()
}

func (a *App) isConfigured() bool {
	a.configMu.RLock()
	defer a.configMu.RUnlock()
	return a.config != nil
}

func (a *App) configSnapshot() *Config {
	a.configMu.RLock()
	defer a.configMu.RUnlock()
	if a.config == nil {
		return nil
	}
	copy := *a.config
	return &copy
}

func (a *App) shareDirectory() string {
	cfg := a.configSnapshot()
	if cfg == nil {
		return ""
	}
	return cfg.ShareDir
}

func validateUsername(input string) (string, error) {
	username := strings.TrimSpace(input)
	count := utf8.RuneCountInString(username)
	if count < 2 || count > 32 {
		return "", errors.New("账号需要 2～32 个字符")
	}
	for _, r := range username {
		if unicode.IsControl(r) {
			return "", errors.New("账号包含无效字符")
		}
	}
	return username, nil
}

func subtleUsernameCompare(expected, actual string) bool {
	if len(expected) != len(actual) {
		return false
	}
	var diff byte
	for i := range expected {
		diff |= expected[i] ^ actual[i]
	}
	return diff == 0
}

func validateShareDirectory(input string) (string, error) {
	if strings.TrimSpace(input) == "" {
		return "", errors.New("请选择共享文件夹")
	}
	abs, err := filepath.Abs(strings.TrimSpace(input))
	if err != nil {
		return "", errors.New("共享目录路径无效")
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", errors.New("共享文件夹不存在或无法访问")
	}
	if !info.IsDir() {
		return "", errors.New("共享路径必须是文件夹")
	}
	return filepath.Clean(abs), nil
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && strings.EqualFold(parsed.Host, r.Host) && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

func isLocalRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func (a *App) audit(r *http.Request, action, target string) {
	username := "unknown"
	if sess := sessionFromContext(r.Context()); sess != nil {
		username = sess.Username
	}
	log.Printf("AUDIT user=%q ip=%q action=%q target=%q", username, remoteIP(r), action, target)
}

func (a *App) accessAddresses() []string {
	_, port, err := net.SplitHostPort(a.listenAddr)
	if err != nil {
		port = "3923"
	}
	addresses := []string{"http://127.0.0.1:" + port}
	seen := map[string]bool{"127.0.0.1": true}
	interfaces, _ := net.Interfaces()
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		items, _ := iface.Addrs()
		for _, item := range items {
			ip, _, err := net.ParseCIDR(item.String())
			if err != nil || ip == nil {
				continue
			}
			ipv4 := ip.To4()
			if ipv4 == nil || ipv4.IsLoopback() || seen[ipv4.String()] || !ipv4.IsPrivate() {
				continue
			}
			seen[ipv4.String()] = true
			addresses = append(addresses, "http://"+ipv4.String()+":"+port)
			if len(addresses) >= 8 {
				return addresses
			}
		}
	}
	return addresses
}

func readJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeAPIError(w, http.StatusBadRequest, "请求内容格式错误")
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeAPIError(w, http.StatusBadRequest, "请求只能包含一个 JSON 对象")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message, "status": status})
}

func (a *App) setServer(server *http.Server) {
	a.serverMu.Lock()
	a.server = server
	a.serverMu.Unlock()
}

func (a *App) localURL() string {
	_, port, err := net.SplitHostPort(a.listenAddr)
	if err != nil {
		return "http://127.0.0.1:3923"
	}
	return fmt.Sprintf("http://127.0.0.1:%s", port)
}
