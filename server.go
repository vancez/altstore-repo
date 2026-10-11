package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

type Server struct {
	store    *Store
	cl       *Releases
	password string // empty = wide open, intended for localhost use
	started  time.Time
}

func NewServer(st *Store, cl *Releases, password string) http.Handler {
	s := &Server{store: st, cl: cl, password: password, started: time.Now()}

	// Everything under web/ is embedded, so the binary stays self-contained.
	// Serving it from a sub-FS keeps the "web/" prefix out of the URLs.
	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err) // impossible: web/ is embedded by the go:embed above
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(webRoot)))
	mux.HandleFunc("GET /source.json", s.handleSource) // legacy alias for all.json
	mux.HandleFunc("GET /source/{file}", s.handleSourceFile)
	mux.HandleFunc("GET /api/config", s.handleGetConfig)
	mux.HandleFunc("PUT /api/config", s.handlePutConfig)
	mux.HandleFunc("GET /api/sites", s.handleSites)
	mux.HandleFunc("GET /api/groups", s.handleListGroups)
	mux.HandleFunc("POST /api/groups", s.handleCreateGroup)
	mux.HandleFunc("PUT /api/groups/{slug}", s.handleUpdateGroup)
	mux.HandleFunc("DELETE /api/groups/{slug}", s.handleDeleteGroup)
	mux.HandleFunc("GET /api/apps", s.handleListApps)
	mux.HandleFunc("POST /api/apps", s.handleCreateApp)
	mux.HandleFunc("PUT /api/apps/{id}", s.handleUpdateApp)
	mux.HandleFunc("DELETE /api/apps/{id}", s.handleDeleteApp)
	mux.HandleFunc("GET /api/apps/{id}/versions", s.handleAppVersions)
	mux.HandleFunc("POST /api/resolve", s.handleResolve)

	return s.withAuth(mux)
}

// withAuth gates everything except the published sources, which are the one
// thing a phone has to be able to fetch without credentials.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every handler downstream reads the language from the context, so the
		// Accept-Language header is resolved exactly once, here.
		r = r.WithContext(withLang(r.Context(), langOf(r)))

		p := r.URL.Path
		if s.password == "" || p == "/source.json" || strings.HasPrefix(p, "/source/") {
			next.ServeHTTP(w, r)
			return
		}
		_, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(s.password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="altstore-repo"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	page, err := fs.ReadFile(webFS, "web/index.html")
	if err != nil {
		http.Error(w, "ui missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(page)
}

func (s *Server) handleSource(w http.ResponseWriter, r *http.Request) {
	s.writeSource(w, r, "")
}

// handleSourceFile serves /source/all.json and /source/<slug>.json. Go's mux
// wildcards must own a whole segment, so "{slug}.json" is not expressible as a
// pattern and the suffix is stripped here instead.
func (s *Server) handleSourceFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	slug, ok := strings.CutSuffix(name, ".json")
	if !ok {
		writeErrCtx(w, r, http.StatusNotFound, msgSourceNeedsJSON)
		return
	}
	if slug == allSlug {
		slug = ""
	}
	s.writeSource(w, r, slug)
}

func (s *Server) writeSource(w http.ResponseWriter, r *http.Request, group string) {
	ctx, cancel := timeoutCtx(r, 60*time.Second)
	defer cancel()

	doc, found := BuildSource(ctx, s.store, s.cl, group)
	if !found {
		writeErrCtx(w, r, http.StatusNotFound, msgSourceNoGroup, group)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=60")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		log.Printf("encode source: %v", err)
	}
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Repo())
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var cfg RepoConfig
	if !readJSON(w, r, &cfg) {
		return
	}
	// A zero TTL would hammer the release APIs on every request.
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 300
	}
	if err := s.store.SetRepo(cfg); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// handleSites lists the hosts builds can be read from, so the admin UI builds
// its pickers out of repoHosts instead of keeping a second copy of the table.
func (s *Server) handleSites(w http.ResponseWriter, r *http.Request) {
	type site struct {
		ID      string `json:"id"`
		Label   string `json:"label"`
		Domain  string `json:"domain"`
		Dialect string `json:"dialect"`
	}
	sites := make([]site, 0, len(repoHosts))
	for _, h := range repoHosts {
		sites = append(sites, site{ID: h.id, Label: h.label, Domain: h.domain, Dialect: string(h.dialect)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"default": defaultRepoType, "sites": sites})
}

// ---------------------------------------------------------------------------
// groups
// ---------------------------------------------------------------------------

func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"groups": s.store.Groups(),
		"counts": s.store.GroupCounts(),
	})
}

func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	var g GroupConfig
	if !readJSON(w, r, &g) {
		return
	}
	slug, err := normalizeSlug(r.Context(), g.Slug)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	g.Slug = slug
	if err := s.checkGroupIdentifier(r.Context(), g); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := s.store.AddGroup(r.Context(), g)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, created)
}

func (s *Server) handleUpdateGroup(w http.ResponseWriter, r *http.Request) {
	var g GroupConfig
	if !readJSON(w, r, &g) {
		return
	}
	slug := r.PathValue("slug")
	g.Slug = slug
	if err := s.checkGroupIdentifier(r.Context(), g); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := s.store.UpdateGroup(slug, g)
	if errors.Is(err, os.ErrNotExist) {
		writeErrCtx(w, r, http.StatusNotFound, msgGroupNotFound)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	released, err := s.store.DeleteGroup(r.PathValue("slug"))
	if err != nil {
		writeErrCtx(w, r, http.StatusNotFound, msgGroupNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "released": released})
}

// checkGroupIdentifier rejects a group whose identifier would collide with the
// repo's or another group's. AltStore keys sources by identifier, so a collision
// makes two feeds show up as one on the device.
func (s *Server) checkGroupIdentifier(ctx context.Context, g GroupConfig) error {
	if g.Identifier == "" {
		return nil // derived from the slug at render time
	}
	if g.Identifier == s.store.Repo().Identifier {
		return trf(ctx, msgGroupIdentClash)
	}
	for _, other := range s.store.Groups() {
		if other.Slug != g.Slug && other.Identifier == g.Identifier {
			return trf(ctx, msgGroupIdentTaken, other.Slug)
		}
	}
	return nil
}

func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Apps())
}

func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	var app AppConfig
	if !readJSON(w, r, &app) {
		return
	}
	if err := s.validateApp(r.Context(), &app); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := s.store.AddApp(app)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, created)
}

func (s *Server) handleUpdateApp(w http.ResponseWriter, r *http.Request) {
	var app AppConfig
	if !readJSON(w, r, &app) {
		return
	}
	if err := s.validateApp(r.Context(), &app); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := s.store.UpdateApp(r.PathValue("id"), app)
	if errors.Is(err, os.ErrNotExist) {
		writeErrCtx(w, r, http.StatusNotFound, msgAppNotFound)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteApp(r.PathValue("id")); err != nil {
		writeErrCtx(w, r, http.StatusNotFound, msgAppNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleResolve is the dry run behind the regex tester. It accepts either a
// saved app id or a raw repo/regex pair from the form.
func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID                string `json:"id"`
		Repo              string `json:"repo"`
		RepoType          string `json:"repoType"`
		AssetRegex        string `json:"assetRegex"`
		VersionRegex      string `json:"versionRegex"`
		IncludePrerelease bool   `json:"includePrerelease"`
		IncludeExternal   bool   `json:"includeExternal"`
		MaxVersions       int    `json:"maxVersions"`
		Force             bool   `json:"force"`
	}
	if !readJSON(w, r, &body) {
		return
	}

	app := AppConfig{
		Repo:              body.Repo,
		RepoType:          body.RepoType,
		AssetRegex:        body.AssetRegex,
		VersionRegex:      body.VersionRegex,
		IncludePrerelease: body.IncludePrerelease,
		IncludeExternal:   body.IncludeExternal,
		MaxVersions:       body.MaxVersions,
	}
	if body.ID != "" {
		saved, ok := s.store.App(body.ID)
		if !ok {
			writeErrCtx(w, r, http.StatusNotFound, msgAppNotFound)
			return
		}
		// The form's values win so the tester reflects unsaved edits.
		if body.Repo == "" {
			app.Repo = saved.Repo
			app.RepoType = saved.RepoType
		}
		if body.AssetRegex == "" {
			app.AssetRegex = saved.AssetRegex
		}
		if body.VersionRegex == "" {
			app.VersionRegex = saved.VersionRegex
		}
	}

	cfg := s.store.Repo()
	ttl := time.Duration(cfg.CacheTTL) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}

	ctx, cancel := timeoutCtx(r, 45*time.Second)
	defer cancel()

	res, err := Resolve(ctx, s.cl, app, cfg, ttl, body.Force)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleAppVersions(w http.ResponseWriter, r *http.Request) {
	app, ok := s.store.App(r.PathValue("id"))
	if !ok {
		writeErrCtx(w, r, http.StatusNotFound, msgAppNotFound)
		return
	}
	cfg := s.store.Repo()
	ttl := time.Duration(cfg.CacheTTL) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}

	ctx, cancel := timeoutCtx(r, 45*time.Second)
	defer cancel()

	res, err := Resolve(ctx, s.cl, app, cfg, ttl, r.URL.Query().Get("force") == "1")
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (s *Server) validateApp(ctx context.Context, app *AppConfig) error {
	// A pasted link decides the host; otherwise it stays whatever the form had.
	repo, host, err := normalizeRepo(ctx, app.Repo, app.RepoType)
	if err != nil {
		return err
	}
	app.Repo, app.RepoType = repo, host.id

	app.Name = strings.TrimSpace(app.Name)
	app.BundleID = strings.TrimSpace(app.BundleID)
	app.AssetRegex = strings.TrimSpace(app.AssetRegex)
	app.VersionRegex = strings.TrimSpace(app.VersionRegex)
	app.Group = strings.TrimSpace(app.Group)

	if app.Group != "" {
		if _, ok := s.store.Group(app.Group); !ok {
			return trf(ctx, msgGroupNotFoundHint, app.Group)
		}
	}
	if app.Name == "" {
		return trf(ctx, msgAppNameRequired)
	}
	if app.BundleID == "" {
		return trf(ctx, msgBundleRequired)
	}
	// AltStore matches installed apps by bundle identifier, so a typo here means
	// the source silently fails to recognise the installed app.
	if !strings.Contains(app.BundleID, ".") || strings.ContainsAny(app.BundleID, " \t/\\") {
		return trf(ctx, msgBundleMalformed)
	}
	if app.MaxVersions < 0 {
		app.MaxVersions = 0
	}
	if len(app.Screenshots) == 0 {
		app.Screenshots = nil
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// writeErrCtx renders a localized error message. Handlers hold the request,
// so the language comes straight from its context.
func writeErrCtx(w http.ResponseWriter, r *http.Request, code int, key msg, args ...any) {
	writeErr(w, code, tr(r.Context(), key, args...))
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		writeErrCtx(w, r, http.StatusBadRequest, msgBadRequest, err)
		return false
	}
	return true
}
