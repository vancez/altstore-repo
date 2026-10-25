package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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

	// Range requests for IPA metadata. This is separate from the release API
	// client because asset downloads can be slower and carry a different token.
	downloads *http.Client
}

func NewServer(st *Store, cl *Releases, password string) http.Handler {
	s := &Server{
		store:     st,
		cl:        cl,
		password:  password,
		started:   time.Now(),
		downloads: &http.Client{Timeout: 60 * time.Second},
	}

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
	mux.HandleFunc("GET /icon/{bundle}", s.handleIcon)
	mux.HandleFunc("GET /image/{name}", s.handleImage)
	mux.HandleFunc("GET /api/config", s.handleGetConfig)
	mux.HandleFunc("PUT /api/config", s.handlePutConfig)
	mux.HandleFunc("POST /api/repo/image", s.handleRepoImage)
	mux.HandleFunc("POST /api/repo/image/upload", s.handleRepoImageUpload)
	mux.HandleFunc("GET /api/sites", s.handleSites)
	mux.HandleFunc("GET /api/groups", s.handleListGroups)
	mux.HandleFunc("POST /api/groups", s.handleCreateGroup)
	mux.HandleFunc("PUT /api/groups/{slug}", s.handleUpdateGroup)
	mux.HandleFunc("DELETE /api/groups/{slug}", s.handleDeleteGroup)
	mux.HandleFunc("GET /api/apps", s.handleListApps)
	mux.HandleFunc("POST /api/apps", s.handleCreateApp)
	mux.HandleFunc("POST /api/apps/info", s.handleAppInfo)
	mux.HandleFunc("PUT /api/apps/{id}", s.handleUpdateApp)
	mux.HandleFunc("DELETE /api/apps/{id}", s.handleDeleteApp)
	mux.HandleFunc("GET /api/apps/{id}/versions", s.handleAppVersions)
	mux.HandleFunc("POST /api/resolve", s.handleResolve)

	return s.withAuth(mux)
}

// withAuth gates everything except the published sources and their icons,
// which are the things a phone has to be able to fetch without credentials.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every handler downstream reads the language from the context, so the
		// Accept-Language header is resolved exactly once, here.
		r = r.WithContext(withLang(r.Context(), langOf(r)))

		p := r.URL.Path
		// Icons and header images are part of the published feed: a device
		// fetching a source has to be able to load them without the password.
		if s.password == "" || p == "/source.json" || strings.HasPrefix(p, "/source/") ||
			strings.HasPrefix(p, "/icon/") || strings.HasPrefix(p, "/image/") {
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

// handleIcon serves the artwork fetched from an IPA.
func (s *Server) handleIcon(w http.ResponseWriter, r *http.Request) {
	file, ok := s.store.IconPath(r.PathValue("bundle"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	serveImageFile(w, r, file)
}

// handleImage serves the repo icon or header image saved from a URL.
func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	file, ok := s.store.SourceImagePath(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	serveImageFile(w, r, file)
}

// The URL carries no extension because the slot or bundle ID is the whole key;
// the file on disk does, so ServeContent can name the content type. no-cache
// plus an ETag makes a client revalidate after the file is replaced, and the
// ETag carries the nanosecond mtime so a replacement never keeps the old
// validator.
func serveImageFile(w http.ResponseWriter, r *http.Request, file string) {
	f, err := os.Open(file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("ETag", fmt.Sprintf(`"%x-%x"`, st.Size(), st.ModTime().UnixNano()))
	http.ServeContent(w, r, filepath.Base(file), st.ModTime(), f)
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

// maxSourceImageSize caps a downloaded repo icon or header. These are small
// website images; anything larger is a mistake or a hostile URL.
const maxSourceImageSize = 10 << 20

// handleRepoImage downloads a repo icon or header image and stores it next to
// the config, so the published feed can point at this server instead of a
// third-party URL that may move, rate-limit or require a referer.
func (s *Server) handleRepoImage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL  string `json:"url"`
		Kind string `json:"kind"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	kind := strings.TrimSpace(body.Kind)
	if _, ok := sourceImageNames[kind]; !ok {
		writeErrCtx(w, r, http.StatusBadRequest, msgImageKindBad, kind)
		return
	}
	rawURL := strings.TrimSpace(body.URL)
	if rawURL == "" {
		writeErrCtx(w, r, http.StatusBadRequest, msgImageURLRequired)
		return
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		writeErrCtx(w, r, http.StatusBadRequest, msgImageURLBad)
		return
	}
	scheme := strings.ToLower(u.Scheme)
	if u.Host == "" || (scheme != "http" && scheme != "https") {
		writeErrCtx(w, r, http.StatusBadRequest, msgImageURLBad)
		return
	}

	ctx, cancel := timeoutCtx(r, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		writeErrCtx(w, r, http.StatusBadRequest, msgImageURLBad)
		return
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "image/png,image/jpeg")

	resp, err := s.downloads.Do(req)
	if err != nil {
		writeErrCtx(w, r, http.StatusBadGateway, msgImageFetchFail, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeErrCtx(w, r, http.StatusBadGateway, msgImageFetchFail, resp.Status)
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSourceImageSize+1))
	if err != nil {
		writeErrCtx(w, r, http.StatusBadGateway, msgImageFetchFail, err)
		return
	}
	if len(data) > maxSourceImageSize {
		writeErrCtx(w, r, http.StatusBadRequest, msgImageTooLarge, maxSourceImageSize>>20)
		return
	}
	s.saveRepoImage(w, r, kind, data)
}

// handleRepoImageUpload is the same operation with the file coming from the
// admin's machine instead of a URL. The multipart body is capped before parsing
// so an oversized upload cannot fill memory or a temp file.
func (s *Server) handleRepoImageUpload(w http.ResponseWriter, r *http.Request) {
	const maxBody = maxSourceImageSize + (1 << 20) // image plus multipart framing
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseMultipartForm(maxBody); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeErrCtx(w, r, http.StatusBadRequest, msgImageTooLarge, maxSourceImageSize>>20)
		} else {
			writeErrCtx(w, r, http.StatusBadRequest, msgImageUploadBad, err)
		}
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	kind := strings.TrimSpace(r.FormValue("kind"))
	if _, ok := sourceImageNames[kind]; !ok {
		writeErrCtx(w, r, http.StatusBadRequest, msgImageKindBad, kind)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeErrCtx(w, r, http.StatusBadRequest, msgImageFileRequired)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxSourceImageSize+1))
	if err != nil {
		writeErrCtx(w, r, http.StatusBadRequest, msgImageUploadBad, err)
		return
	}
	if len(data) > maxSourceImageSize {
		writeErrCtx(w, r, http.StatusBadRequest, msgImageTooLarge, maxSourceImageSize>>20)
		return
	}
	s.saveRepoImage(w, r, kind, data)
}

// saveRepoImage validates the bytes as a supported image and writes the fixed
// slot, returning the local URL the form should use.
func (s *Server) saveRepoImage(w http.ResponseWriter, r *http.Request, kind string, data []byte) {
	_, format, err := decodeIconConfig(data)
	if err != nil {
		writeErrCtx(w, r, http.StatusBadRequest, msgImageUnsupported)
		return
	}
	ext := iconExtFor(format)
	if ext == "" {
		writeErrCtx(w, r, http.StatusBadRequest, msgImageUnsupported)
		return
	}
	name, err := s.store.SaveSourceImage(kind, data, ext)
	if err != nil {
		writeErrCtx(w, r, http.StatusInternalServerError, msgImageSaveFail, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": publicURL(r, "/image/"+name)})
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

// appInfoResult is the payload behind the form's Fetch info button. Empty
// fields mean "nothing better was found", which the UI leaves alone rather
// than clearing a value the admin typed; everything present overwrites.
type appInfoResult struct {
	Name                 string   `json:"name,omitempty"`
	BundleIdentifier     string   `json:"bundleIdentifier,omitempty"`
	Category             string   `json:"category,omitempty"`
	LocalizedDescription string   `json:"localizedDescription,omitempty"`
	IconURL              string   `json:"iconURL,omitempty"`
	Warnings             []string `json:"warnings,omitempty"`
}

// handleAppInfo reads the repository About and the newest IPA. It is a dry run
// like /api/resolve, except that the icon is written to disk immediately: the
// form only has the bundle ID to key it by after the IPA has been read, and the
// icon URL has to point somewhere the device can fetch it.
func (s *Server) handleAppInfo(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Repo              string `json:"repo"`
		RepoType          string `json:"repoType"`
		AssetRegex        string `json:"assetRegex"`
		VersionRegex      string `json:"versionRegex"`
		IncludePrerelease bool   `json:"includePrerelease"`
		IncludeExternal   bool   `json:"includeExternal"`
		DownloadOverride  string `json:"downloadOverride"`
	}
	if !readJSON(w, r, &body) {
		return
	}

	ctx, cancel := timeoutCtx(r, 75*time.Second)
	defer cancel()

	// normalizeRepo owns the host choice, including a link pasted into the repo
	// box; Resolve below repeats the normalisation but needs the same host here
	// for the About request.
	_, host, err := normalizeRepo(ctx, body.Repo, body.RepoType)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	app := AppConfig{
		Repo:              body.Repo,
		RepoType:          body.RepoType,
		AssetRegex:        body.AssetRegex,
		VersionRegex:      body.VersionRegex,
		IncludePrerelease: body.IncludePrerelease,
		IncludeExternal:   body.IncludeExternal,
	}
	cfg := s.store.Repo()
	ttl := time.Duration(cfg.CacheTTL) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}

	// Clicking Fetch info asks "what is current?", so the release cache is
	// bypassed. The tester stays cheap; this one is a deliberate action.
	res, err := Resolve(ctx, s.cl, app, cfg, ttl, true)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if len(res.Versions) == 0 {
		writeErrCtx(w, r, http.StatusBadRequest, msgInfoNoVersion)
		return
	}

	downloadURL := strings.TrimSpace(res.Versions[0].DownloadURL)
	if override := strings.TrimSpace(body.DownloadOverride); override != "" {
		downloadURL = override
	}
	if downloadURL == "" {
		writeErrCtx(w, r, http.StatusBadRequest, msgInfoNoVersion)
		return
	}

	info, err := readIPAInfo(ctx, s.downloads, downloadURL, downloadAuthHeader(cfg, downloadURL))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}

	out := appInfoResult{
		Name:             info.Name,
		BundleIdentifier: info.BundleID,
		Category:         info.Category,
	}
	switch {
	case info.BundleID == "":
		out.Warnings = append(out.Warnings, tr(ctx, msgIPANoBundle))
	case len(info.Icon) == 0:
		out.Warnings = append(out.Warnings, tr(ctx, msgIPANoIcon))
	default:
		if _, ok := iconFileName(info.BundleID); !ok {
			out.Warnings = append(out.Warnings, tr(ctx, msgIconUnsafe, info.BundleID))
		} else if err := s.store.SaveIcon(info.BundleID, info.Icon, info.IconExt); err != nil {
			log.Printf("save icon for %s: %v", info.BundleID, err)
			out.Warnings = append(out.Warnings, tr(ctx, msgIconSave, err))
		} else {
			out.IconURL = publicURL(r, "/icon/"+info.BundleID)
		}
	}

	// A missing About line is normal. A request that fails is worth surfacing,
	// but it must not throw away the information the IPA just supplied.
	desc, err := s.cl.RepoDescription(ctx, host, res.Repo, host.token(cfg))
	if err != nil {
		log.Printf("read description for %s: %v", res.Repo, err)
		out.Warnings = append(out.Warnings, tr(ctx, msgAboutFail, err))
	} else if desc != "" {
		out.LocalizedDescription = desc
	}

	writeJSON(w, http.StatusOK, out)
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
	id := r.PathValue("id")
	old, hadOld := s.store.App(id)
	updated, err := s.store.UpdateApp(id, app)
	if errors.Is(err, os.ErrNotExist) {
		writeErrCtx(w, r, http.StatusNotFound, msgAppNotFound)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The icon is keyed by bundle ID. Changing that ID is the only way an
	// update can strand a file; delete the old one unless an app or group still
	// references it, this app's URL included.
	if hadOld && old.BundleID != "" && old.BundleID != updated.BundleID &&
		!s.store.IconInUse(old.BundleID) {
		if err := s.store.DeleteIcon(old.BundleID); err != nil {
			log.Printf("delete icon for %s: %v", old.BundleID, err)
		}
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	app, ok := s.store.App(id)
	if !ok {
		writeErrCtx(w, r, http.StatusNotFound, msgAppNotFound)
		return
	}
	if err := s.store.DeleteApp(id); err != nil {
		writeErrCtx(w, r, http.StatusNotFound, msgAppNotFound)
		return
	}
	if app.BundleID != "" && !s.store.IconInUse(app.BundleID) {
		if err := s.store.DeleteIcon(app.BundleID); err != nil {
			log.Printf("delete icon for %s: %v", app.BundleID, err)
		}
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

// downloadAuthHeader attaches the token of the host that actually serves the
// download, not the host the releases came from: a mirrored Forgejo release can
// link its file to GitHub, and the Starforge token must never follow it there.
func downloadAuthHeader(cfg RepoConfig, rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	h, ok := repoHostByDomain(u.Hostname())
	if !ok {
		return ""
	}
	token := h.token(cfg)
	if token == "" {
		return ""
	}
	if h.dialect == dialectGitHub {
		return "Bearer " + token
	}
	return "token " + token
}

// publicURL builds an absolute URL for the published feed. A device cannot
// make use of a relative one, so this carries the same public host the admin UI
// was reached through; a reverse proxy should pass X-Forwarded-Proto and
// X-Forwarded-Host for that to be the externally visible name.
func publicURL(r *http.Request, path string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := strings.ToLower(firstHeaderValue(r.Header.Get("X-Forwarded-Proto"))); v == "http" || v == "https" {
		scheme = v
	}
	host := r.Host
	if v := firstHeaderValue(r.Header.Get("X-Forwarded-Host")); v != "" {
		host = v
	}
	u := url.URL{Scheme: scheme, Host: host, Path: path}
	return u.String()
}

func firstHeaderValue(v string) string {
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
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
