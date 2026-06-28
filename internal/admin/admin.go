// Package admin serves the server-rendered admin dashboard: analytics plus
// initiator/receiver administration. It is gated by a single ADMIN_TOKEN and
// is mounted only when that token is configured. The dashboard reuses the core
// service, so it shares the exact same data path as the broker itself.
package admin

import (
	"bytes"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/artemavrin/broker/internal/auth"
	"github.com/artemavrin/broker/internal/core"
	"github.com/artemavrin/broker/internal/secret"
	"github.com/artemavrin/broker/internal/wshub"
)

//go:embed templates/*.html
var tmplFS embed.FS

//go:embed static/*
var staticFS embed.FS

const cookieName = "broker_admin"

// Handler renders and serves the dashboard.
type Handler struct {
	svc        *core.Service
	signer     *auth.Signer
	hub        *wshub.Hub
	tmpl       *template.Template
	adminHash  []byte
	sessionTTL time.Duration
	log        *slog.Logger
}

// New builds the dashboard handler. adminToken gates access; the caller must
// only construct this when the token is non-empty.
func New(svc *core.Service, signer *auth.Signer, hub *wshub.Hub, adminToken string, sessionTTL time.Duration, log *slog.Logger) *Handler {
	funcs := template.FuncMap{
		"sub":      func(a, b int) int { return a - b },
		"shortID":  shortID,
		"pct":      pct,
		"humanAge": humanAge,
		"datetime": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04") },
	}
	tmpl := template.Must(template.New("").Funcs(funcs).ParseFS(tmplFS, "templates/*.html"))
	return &Handler{
		svc:        svc,
		signer:     signer,
		hub:        hub,
		tmpl:       tmpl,
		adminHash:  secret.Hash(adminToken),
		sessionTTL: sessionTTL,
		log:        log,
	}
}

// Routes returns the dashboard's mux, with paths under /admin.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Public.
	mux.HandleFunc("GET /admin/login", h.loginPage)
	mux.HandleFunc("POST /admin/login", h.loginSubmit)
	mux.HandleFunc("POST /admin/logout", h.logout)
	if sub, err := fs.Sub(staticFS, "static"); err == nil {
		mux.Handle("GET /admin/static/", http.StripPrefix("/admin/static/", http.FileServer(http.FS(sub))))
	}

	// Authenticated.
	mux.HandleFunc("GET /admin/{$}", h.authed(h.overview))
	mux.HandleFunc("GET /admin/initiators", h.authed(h.initiators))
	mux.HandleFunc("POST /admin/initiators", h.authed(h.createInitiator))
	mux.HandleFunc("POST /admin/initiators/{id}/revoke", h.authed(h.revokeInitiator))
	mux.HandleFunc("GET /admin/receivers", h.authed(h.receivers))
	mux.HandleFunc("POST /admin/receivers/{id}/revoke", h.authed(h.revokeReceiver))
	return mux
}

// ---- auth ----

// authed wraps a handler, redirecting to the login page unless a valid admin
// session cookie is present.
func (h *Handler) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.authenticated(r) {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func (h *Handler) authenticated(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	id, err := h.signer.Verify(c.Value)
	return err == nil && id.Role == auth.RoleAdmin
}

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	if h.authenticated(r) {
		http.Redirect(w, r, "/admin/", http.StatusSeeOther)
		return
	}
	h.render(w, "login", map[string]any{"Title": "Sign in"})
}

func (h *Handler) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !secret.Equal(secret.Hash(r.PostFormValue("token")), h.adminHash) {
		w.WriteHeader(http.StatusUnauthorized)
		h.render(w, "login", map[string]any{"Title": "Sign in", "Error": "Invalid admin token."})
		return
	}
	token, _, err := h.signer.IssueWithTTL("admin", auth.RoleAdmin, h.sessionTTL)
	if err != nil {
		h.fail(w, "issue session", err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
		MaxAge:   int(h.sessionTTL.Seconds()),
	})
	http.Redirect(w, r, "/admin/", http.StatusSeeOther)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/admin", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

// ---- pages ----

func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stats, err := h.svc.DB().AdminStats(ctx)
	if err != nil {
		h.fail(w, "stats", err)
		return
	}
	backlogs, err := h.svc.DB().TopBacklogs(ctx, 6)
	if err != nil {
		h.fail(w, "backlogs", err)
		return
	}
	var maxB int64
	for _, b := range backlogs {
		if b.Count > maxB {
			maxB = b.Count
		}
	}
	h.render(w, "overview", map[string]any{
		"Title": "Overview", "Active": "overview", "H1": "Overview",
		"Sub": "Live queue and participant health", "AutoRefresh": true,
		"Sessions":   h.hub.Count(),
		"Stats":      stats,
		"Tput":       h.svc.Throughput(),
		"Backlogs":   backlogs,
		"MaxBacklog": maxB,
	})
}

func (h *Handler) initiators(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.DB().ListInitiators(r.Context())
	if err != nil {
		h.fail(w, "list initiators", err)
		return
	}
	data := map[string]any{
		"Title": "Initiators", "Active": "initiators", "Sessions": h.hub.Count(),
		"Initiators": list,
	}
	// A freshly created secret is passed via a short-lived, signed flash cookie
	// so it survives the POST→redirect→GET without ever touching storage.
	if rev := h.takeReveal(w, r); rev != nil {
		data["Reveal"] = rev
	}
	h.render(w, "initiators", data)
}

func (h *Handler) createInitiator(w http.ResponseWriter, r *http.Request) {
	id, rawSecret, err := h.svc.NewInitiator(r.Context())
	if err != nil {
		h.fail(w, "create initiator", err)
		return
	}
	h.setReveal(w, id, rawSecret)
	http.Redirect(w, r, "/admin/initiators", http.StatusSeeOther)
}

func (h *Handler) revokeInitiator(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DB().RevokeInitiator(r.Context(), r.PathValue("id")); err != nil {
		h.log.Warn("revoke initiator", "err", err)
	}
	http.Redirect(w, r, "/admin/initiators", http.StatusSeeOther)
}

func (h *Handler) receivers(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.DB().ListAllReceivers(r.Context(), 200)
	if err != nil {
		h.fail(w, "list receivers", err)
		return
	}
	h.render(w, "receivers", map[string]any{
		"Title": "Receivers", "Active": "receivers", "Sessions": h.hub.Count(),
		"Receivers": list,
	})
}

func (h *Handler) revokeReceiver(w http.ResponseWriter, r *http.Request) {
	// Reuse the participant-scoped revoke isn't possible here (no initiator
	// context), so revoke directly by id via a dedicated query.
	if err := h.svc.DB().RevokeReceiverByID(r.Context(), r.PathValue("id")); err != nil {
		h.log.Warn("revoke receiver", "err", err)
	}
	http.Redirect(w, r, "/admin/receivers", http.StatusSeeOther)
}

// ---- helpers ----

func (h *Handler) render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		h.fail(w, "render "+name, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

func (h *Handler) fail(w http.ResponseWriter, what string, err error) {
	h.log.Error("admin: "+what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func shortID(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8] + "…"
}

func pct(n, max int64) int {
	if max <= 0 {
		return 0
	}
	return int(n * 100 / max)
}

func humanAge(seconds float64) string {
	if seconds <= 0 {
		return "—"
	}
	d := time.Duration(seconds) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

const revealCookie = "broker_reveal"

// reveal carries a one-time initiator secret across the POST→redirect→GET, so
// the create flow follows Post/Redirect/Get (no duplicate creation on refresh)
// without persisting the secret anywhere. The cookie is HttpOnly, single-use
// (deleted on read) and short-lived.
type reveal struct {
	ID     string `json:"id"`
	Secret string `json:"secret"`
}

func (h *Handler) setReveal(w http.ResponseWriter, id, secretVal string) {
	b, _ := json.Marshal(reveal{ID: id, Secret: secretVal})
	http.SetCookie(w, &http.Cookie{
		Name:     revealCookie,
		Value:    base64.RawURLEncoding.EncodeToString(b),
		Path:     "/admin/initiators",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   60,
	})
}

func (h *Handler) takeReveal(w http.ResponseWriter, r *http.Request) *reveal {
	c, err := r.Cookie(revealCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	// Consume it immediately, regardless of validity.
	http.SetCookie(w, &http.Cookie{Name: revealCookie, Value: "", Path: "/admin/initiators", MaxAge: -1, HttpOnly: true})
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return nil
	}
	var rev reveal
	if err := json.Unmarshal(raw, &rev); err != nil {
		return nil
	}
	return &rev
}
