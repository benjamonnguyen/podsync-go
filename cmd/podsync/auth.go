package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// auth guards the management portal with a login page backed by a signed
// session cookie, instead of HTTP Basic auth. The signing key is derived
// from the configured username/password, so a session survives a restart
// but every session is invalidated the moment either credential changes.
type auth struct {
	username, password string
	key                [32]byte
	secureCookie       bool // Secure flag on the cookie; set when TLS is enabled
	tmpl               *template.Template
}

const (
	sessionCookieName = "podsync_session"
	sessionTTL        = 30 * 24 * time.Hour
)

func newAuth(username, password string, secureCookie bool) (*auth, error) {
	tmpl, err := template.ParseFS(assets, "templates/login.html")
	if err != nil {
		return nil, err
	}
	return &auth{
		username: username, password: password,
		key:          sha256.Sum256([]byte(username + "\x00" + password)),
		secureCookie: secureCookie,
		tmpl:         tmpl,
	}, nil
}

func (a *auth) sign(exp int64) string {
	mac := hmac.New(sha256.New, a.key[:])
	mac.Write([]byte(strconv.FormatInt(exp, 10)))
	return strconv.FormatInt(exp, 10) + "." + hex.EncodeToString(mac.Sum(nil))
}

// valid reports whether a cookie value is a session this process signed,
// that has not expired.
func (a *auth) valid(value string) bool {
	exp, _, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	expInt, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() > expInt {
		return false
	}
	want := a.sign(expInt)
	return subtle.ConstantTimeCompare([]byte(value), []byte(want)) == 1
}

func (a *auth) hasSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookieName)
	return err == nil && a.valid(c.Value)
}

func (a *auth) setSession(w http.ResponseWriter) {
	exp := time.Now().Add(sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: a.sign(exp.Unix()), Path: "/", Expires: exp,
		HttpOnly: true, Secure: a.secureCookie, SameSite: http.SameSiteLaxMode,
	})
}

func (a *auth) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: a.secureCookie, SameSite: http.SameSiteLaxMode,
	})
}

// validCreds compares constant-time and hashes the candidate either way, so
// neither a missing field nor a wrong guess leaks timing information about
// the real credentials.
func (a *auth) validCreds(user, password string) bool {
	wantUser := sha256.Sum256([]byte(a.username))
	wantPass := sha256.Sum256([]byte(a.password))
	gotUser := sha256.Sum256([]byte(user))
	gotPass := sha256.Sum256([]byte(password))
	userOK := subtle.ConstantTimeCompare(gotUser[:], wantUser[:]) == 1
	passOK := subtle.ConstantTimeCompare(gotPass[:], wantPass[:]) == 1
	return userOK && passOK
}

// safeNext keeps ?next= from being used as an open redirect: only a path on
// this site is allowed.
func safeNext(v string) string {
	if v == "" || !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") || strings.Contains(v, "://") {
		return "/"
	}
	return v
}

func (a *auth) render(w http.ResponseWriter, code int, errText, next string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_ = a.tmpl.Execute(w, map[string]any{"Err": errText, "Next": next, "AssetV": assetVersion})
}

func (a *auth) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if a.hasSession(r) {
		redirect(w, r, next)
		return
	}
	a.render(w, http.StatusOK, "", next)
}

func (a *auth) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	next := safeNext(r.FormValue("next"))
	if !a.validCreds(r.FormValue("username"), r.FormValue("password")) {
		a.render(w, http.StatusUnauthorized, "Invalid username or password.", next)
		return
	}
	a.setSession(w)
	redirect(w, r, next)
}

func (a *auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	a.clearSession(w)
	redirect(w, r, "/login")
}

// require protects everything else in the management portal. Static assets
// pass straight through (the login page needs its stylesheet); everything
// else without a valid session is sent to the login page, carrying the
// original URL as ?next= so a GET lands back where the user started.
func (a *auth) require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") || a.hasSession(r) {
			next.ServeHTTP(w, r)
			return
		}
		to := "/login"
		if r.Method == http.MethodGet {
			to = "/login?next=" + url.QueryEscape(r.URL.RequestURI())
		}
		redirect(w, r, to)
	})
}
