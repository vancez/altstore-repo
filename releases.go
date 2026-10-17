package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ghVersion       = "2022-11-28" // GitHub API version header
	ghPageSize      = 100
	forgejoPageSize = 50 // Forgejo caps a page at MAX_RESPONSE_ITEMS, 50 by default
	maxPages        = 3  // 300 releases is far more than any source needs
	userAgent       = "altstore-repo-generator"
)

// asset is one file attached to a release. Both APIs use these field names;
// "type" is Forgejo's and stays empty on GitHub.
type asset struct {
	Name               string    `json:"name"`
	Size               int64     `json:"size"`
	BrowserDownloadURL string    `json:"browser_download_url"`
	ContentType        string    `json:"content_type"`
	Type               string    `json:"type"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// external reports whether the file only exists as a link to another host.
// Forgejo marks the assets of a mirrored release that way, and it cannot know
// their size — those come back as 0.
func (a asset) external() bool { return a.Type == "external" }

type release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"` // the release notes, as typed; markdown on both hosts
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	CreatedAt   time.Time `json:"created_at"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []asset   `json:"assets"`
}

// Releases is a caching client for the release API of every configured host.
// One cache serves them all; the key carries the host, so the same owner/name
// on two hosts cannot collide.
type Releases struct {
	client *http.Client

	mu    sync.Mutex
	cache map[string]*cacheEntry
}

type cacheEntry struct {
	releases []release
	etag     string // GitHub only: Forgejo has no conditional requests
	at       time.Time
}

func NewReleases() *Releases {
	return &Releases{
		client: &http.Client{Timeout: 20 * time.Second},
		cache:  map[string]*cacheEntry{},
	}
}

// Releases returns the releases for owner/name on one host, newest first. A
// cached copy younger than ttl is served directly; an expired copy is
// revalidated where the host supports it, so a 304 costs nothing against the
// rate limit. force skips the freshness check but still revalidates.
func (r *Releases) Releases(ctx context.Context, h repoHost, repo, token string, ttl time.Duration, force bool) ([]release, error) {
	key := h.id + "/" + repo

	r.mu.Lock()
	entry := r.cache[key]
	r.mu.Unlock()

	if entry != nil && !force && time.Since(entry.at) < ttl {
		return entry.releases, nil
	}

	etag := ""
	if entry != nil {
		etag = entry.etag
	}
	releases, newETag, notModified, err := r.fetch(ctx, h, repo, token, etag)
	if err != nil {
		// Serve stale rather than fail, so a rate-limit blip doesn't blank the source.
		if entry != nil {
			return entry.releases, nil
		}
		return nil, err
	}
	if notModified && entry != nil {
		r.mu.Lock()
		entry.at = time.Now()
		r.mu.Unlock()
		return entry.releases, nil
	}

	r.mu.Lock()
	r.cache[key] = &cacheEntry{releases: releases, etag: newETag, at: time.Now()}
	r.mu.Unlock()
	return releases, nil
}

func (r *Releases) fetch(ctx context.Context, h repoHost, repo, token, etag string) ([]release, string, bool, error) {
	if h.dialect == dialectForgejo {
		releases, err := r.fetchForgejo(ctx, h, repo, token)
		return releases, "", false, err
	}
	return r.fetchGitHub(ctx, h, repo, token, etag)
}

func (r *Releases) fetchGitHub(ctx context.Context, h repoHost, repo, token, etag string) (releases []release, newETag string, notModified bool, err error) {
	next := fmt.Sprintf("%s/repos/%s/releases?per_page=%d", h.apiBase, escapeRepo(repo), ghPageSize)

	for page := 0; page < maxPages && next != ""; page++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, "", false, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", ghVersion)
		req.Header.Set("User-Agent", userAgent)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		// Only revalidate the first page; that is where we normally stop.
		if page == 0 && etag != "" {
			req.Header.Set("If-None-Match", etag)
		}

		resp, err := r.client.Do(req)
		if err != nil {
			return nil, "", false, trf(ctx, msgRepoRequestFail, h.domain, err)
		}

		if page == 0 && resp.StatusCode == http.StatusNotModified {
			resp.Body.Close()
			return nil, etag, true, nil
		}
		if page == 0 {
			newETag = resp.Header.Get("ETag")
		}

		if resp.StatusCode != http.StatusOK {
			msg := apiErrorMessage(ctx, resp, h)
			resp.Body.Close()
			return nil, "", false, fmt.Errorf("%s: %s", repo, msg)
		}

		var page_ []release
		dec := json.NewDecoder(io.LimitReader(resp.Body, 16<<20))
		bodyErr := dec.Decode(&page_)
		link := resp.Header.Get("Link")
		resp.Body.Close()
		if bodyErr != nil {
			return nil, "", false, trf(ctx, msgRepoDecodeFail, h.domain, bodyErr)
		}

		releases = append(releases, page_...)
		if len(page_) < ghPageSize {
			break
		}
		next = nextLink(link, h.apiBase)
	}
	return releases, newETag, false, nil
}

// fetchForgejo reads a Forgejo/Gitea release list. The payload is close enough
// to GitHub's to share the types, with three differences that matter here:
// pages are capped at 50, there are no ETags (every refresh is a full read),
// and a mirrored release links its files to another host instead of uploading
// them, which is what asset.external reports.
func (r *Releases) fetchForgejo(ctx context.Context, h repoHost, repo, token string) ([]release, error) {
	next := fmt.Sprintf("%s/repos/%s/releases?limit=%d", h.apiBase, escapeRepo(repo), forgejoPageSize)

	var releases []release
	for page := 0; page < maxPages && next != ""; page++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", userAgent)
		if token != "" {
			// Gitea and Forgejo spell the bearer token "token", not "Bearer".
			req.Header.Set("Authorization", "token "+token)
		}

		resp, err := r.client.Do(req)
		if err != nil {
			return nil, trf(ctx, msgRepoRequestFail, h.domain, err)
		}

		if resp.StatusCode != http.StatusOK {
			msg := apiErrorMessage(ctx, resp, h)
			resp.Body.Close()
			return nil, fmt.Errorf("%s: %s", repo, msg)
		}

		var page_ []release
		dec := json.NewDecoder(io.LimitReader(resp.Body, 16<<20))
		bodyErr := dec.Decode(&page_)
		link := resp.Header.Get("Link")
		resp.Body.Close()
		if bodyErr != nil {
			return nil, trf(ctx, msgRepoDecodeFail, h.domain, bodyErr)
		}

		releases = append(releases, page_...)
		if len(page_) < forgejoPageSize {
			break
		}
		if next = nextLink(link, h.apiBase); next == "" {
			// Older Gitea builds send no Link header. Page numbers are the one
			// thing both APIs agree on, and maxPages bounds the guessing.
			next = fmt.Sprintf("%s/repos/%s/releases?limit=%d&page=%d", h.apiBase, escapeRepo(repo), forgejoPageSize, page+2)
		}
	}
	return releases, nil
}

// escapeRepo escapes owner and name for use in an API path. normalizeRepo
// already refuses anything odd; this is the belt to that pair of braces.
func escapeRepo(repo string) string {
	owner, name, _ := strings.Cut(repo, "/")
	return url.PathEscape(owner) + "/" + url.PathEscape(name)
}

// apiErrorMessage turns an error payload into something a human can act on.
// Both APIs answer with a "message" field; only GitHub reports rate limits in
// headers, so that part stays behind the dialect check.
func apiErrorMessage(ctx context.Context, resp *http.Response, h repoHost) string {
	var payload struct {
		Message string `json:"message"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&payload)
	msg := payload.Message
	if msg == "" {
		msg = resp.Status
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return tr(ctx, msgRepoNotFound, h.domain)
	case http.StatusForbidden, http.StatusTooManyRequests:
		if h.dialect == dialectGitHub && resp.Header.Get("X-RateLimit-Remaining") == "0" {
			reset := resp.Header.Get("X-RateLimit-Reset")
			if ts, err := strconv.ParseInt(reset, 10, 64); err == nil {
				return tr(ctx, msgRepoRateLimited, h.domain, time.Until(time.Unix(ts, 0)).Round(time.Minute))
			}
			return tr(ctx, msgRepoRateLimitedB, h.domain)
		}
		return tr(ctx, msgRepoForbidden, h.domain, msg)
	case http.StatusUnauthorized:
		return tr(ctx, msgRepoBadToken, h.domain)
	}
	return msg
}

// nextLink pulls the rel="next" URL out of a Link header, but only when it
// points back at the same API: a link to another host is not ours to follow.
func nextLink(link, apiBase string) string {
	want, err := url.Parse(apiBase)
	if err != nil {
		return ""
	}
	for _, part := range strings.Split(link, ",") {
		sections := strings.Split(part, ";")
		if len(sections) < 2 {
			continue
		}
		if !strings.Contains(sections[1], `rel="next"`) {
			continue
		}
		raw := strings.TrimSpace(sections[0])
		raw = strings.TrimPrefix(raw, "<")
		raw = strings.TrimSuffix(raw, ">")
		if u, err := url.Parse(raw); err == nil && u.Host == want.Host {
			return raw
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Resolution: releases -> AltStore versions
// ---------------------------------------------------------------------------

// ResolvedVersion is one entry in the AltStore "versions" array.
type ResolvedVersion struct {
	Version     string `json:"version"`
	Date        string `json:"date"`
	DownloadURL string `json:"downloadURL"`
	Size        int64  `json:"size"`
	AssetName   string `json:"assetName"`
	TagName     string `json:"tagName"`
	ReleaseName string `json:"releaseName"`
	ReleaseURL  string `json:"releaseUrl"`
	Prerelease  bool   `json:"prerelease"`

	// Changelog is the release's own notes, which is what AltStore shows under
	// a version. It stays out of the JSON here: the feed carries the full text
	// as the version's localizedDescription, and the per-app call that fills the
	// app list would otherwise drag every release body along with it.
	Changelog string `json:"-"`
}

// ReleasePreview describes what the regex did to a single release. The UI shows
// this so it is obvious which file wins and why.
type ReleasePreview struct {
	TagName     string   `json:"tagName"`
	ReleaseName string   `json:"releaseName"`
	ReleaseURL  string   `json:"releaseUrl"`
	PublishedAt string   `json:"publishedAt"`
	Prerelease  bool     `json:"prerelease"`
	Draft       bool     `json:"draft"`
	Skipped     string   `json:"skipped,omitempty"` // why this release produced no version
	Assets      []string `json:"assets"`
	Matched     []string `json:"matched"`
	Selected    string   `json:"selected"`
	Version     string   `json:"version"`
	Size        int64    `json:"size"`
	Date        string   `json:"date"`
	Changelog   string   `json:"changelog,omitempty"` // opening of the release notes, flattened to one line
}

// ResolveResult is the dry-run payload behind the regex tester.
type ResolveResult struct {
	Repo     string            `json:"repo"`
	Versions []ResolvedVersion `json:"versions"`
	Preview  []ReleasePreview  `json:"preview"`
	Warnings []string          `json:"warnings"`
}

// defaultAssetRegex is what an empty pattern means. An AltStore source can only
// ever point at an .ipa, so an empty box should not silently pick a .zip.
const defaultAssetRegex = `(?i)\.ipa$`

// previewChangelogRunes caps the release notes carried in the tester preview.
// The feed gets the whole text; the preview only has to show that notes are
// there, and a repo with hundreds of releases would otherwise bloat the JSON.
const previewChangelogRunes = 160

// Resolve turns releases into AltStore versions using the app's regexes. The
// host comes from the app itself, and its token from the matching field in cfg.
// It never returns an error for "nothing matched" — that is reported as a
// warning so the UI can still render the per-release breakdown.
func Resolve(ctx context.Context, cl *Releases, app AppConfig, cfg RepoConfig, ttl time.Duration, force bool) (*ResolveResult, error) {
	repo, host, err := normalizeRepo(ctx, app.Repo, app.RepoType)
	if err != nil {
		return nil, err
	}

	assetSrc := strings.TrimSpace(app.AssetRegex)
	if assetSrc == "" {
		assetSrc = defaultAssetRegex
	}
	assetRe, err := regexp.Compile(assetSrc)
	if err != nil {
		return nil, trf(ctx, msgAssetRegexBad, err)
	}

	var versionRe *regexp.Regexp
	if s := strings.TrimSpace(app.VersionRegex); s != "" {
		versionRe, err = regexp.Compile(s)
		if err != nil {
			return nil, trf(ctx, msgVersionRegexBad, err)
		}
	}

	releases, err := cl.Releases(ctx, host, repo, host.token(cfg), ttl, force)
	if err != nil {
		return nil, err
	}

	res := &ResolveResult{
		Repo:     repo,
		Versions: []ResolvedVersion{},
		Preview:  []ReleasePreview{},
		Warnings: []string{},
	}
	seen := map[string]bool{}

	for _, rel := range releases {
		pv := ReleasePreview{
			TagName:     rel.TagName,
			ReleaseName: rel.Name,
			ReleaseURL:  rel.HTMLURL,
			PublishedAt: rel.PublishedAt.Format(time.RFC3339),
			Prerelease:  rel.Prerelease,
			Draft:       rel.Draft,
			Assets:      []string{},
			Matched:     []string{},
		}
		for _, a := range rel.Assets {
			pv.Assets = append(pv.Assets, a.Name)
		}
		// Filled in for every release so the preview never shows a blank date.
		// Overwritten below once the regex has picked the file that counts.
		pv.Date = releaseFallbackDate(rel).UTC().Format(time.RFC3339)

		if rel.Draft {
			pv.Skipped = tr(ctx, msgSkipDraft)
			res.Preview = append(res.Preview, pv)
			continue
		}
		if rel.Prerelease && !app.IncludePrerelease {
			pv.Skipped = tr(ctx, msgSkipPrerelease)
			res.Preview = append(res.Preview, pv)
			continue
		}

		var picked *asset
		excluded := false // a file matched, but only as an external link
		for i := range rel.Assets {
			a := &rel.Assets[i]
			if a.external() && !app.IncludeExternal {
				if assetRe.MatchString(a.Name) {
					excluded = true
				}
				continue
			}
			if assetRe.MatchString(a.Name) {
				pv.Matched = append(pv.Matched, a.Name)
				if picked == nil {
					picked = a // first match wins, in the release's order
				}
			}
		}
		if picked == nil {
			switch {
			case len(rel.Assets) == 0:
				pv.Skipped = tr(ctx, msgSkipNoAssets)
			case excluded:
				pv.Skipped = tr(ctx, msgSkipExternal)
			default:
				pv.Skipped = tr(ctx, msgSkipNoMatch)
			}
			res.Preview = append(res.Preview, pv)
			continue
		}

		version := versionFromTag(rel.TagName, versionRe)
		if version == "" {
			pv.Skipped = tr(ctx, msgSkipNoVersion)
			res.Preview = append(res.Preview, pv)
			continue
		}
		if seen[version] {
			pv.Skipped = tr(ctx, msgSkipDuplicate, version)
			res.Preview = append(res.Preview, pv)
			continue
		}
		seen[version] = true

		// The version date is the matched file's own timestamp.
		date := assetDate(*picked)
		pv.Date = date.UTC().Format(time.RFC3339)
		pv.Selected = picked.Name
		pv.Version = version
		pv.Size = picked.Size

		// GitHub bodies normally use CRLF; the feed is read as text, so keep it LF.
		notes := strings.ReplaceAll(strings.TrimSpace(rel.Body), "\r\n", "\n")
		if notes != "" {
			pv.Changelog = truncateRunes(collapseSpace(notes), previewChangelogRunes)
		}

		res.Versions = append(res.Versions, ResolvedVersion{
			Version:     version,
			Date:        pv.Date,
			DownloadURL: picked.BrowserDownloadURL,
			Size:        picked.Size,
			AssetName:   picked.Name,
			TagName:     rel.TagName,
			ReleaseName: rel.Name,
			ReleaseURL:  rel.HTMLURL,
			Prerelease:  rel.Prerelease,
			Changelog:   notes,
		})
		res.Preview = append(res.Preview, pv)
	}

	sort.SliceStable(res.Versions, func(i, j int) bool {
		return res.Versions[i].Date > res.Versions[j].Date
	})
	if app.MaxVersions > 0 && len(res.Versions) > app.MaxVersions {
		res.Versions = res.Versions[:app.MaxVersions]
	}

	if len(res.Versions) == 0 {
		res.Warnings = append(res.Warnings, tr(ctx, msgNoVersions))
	}
	return res, nil
}

// assetDate reports when one uploaded file was last touched. updated_at moves
// when the file is re-uploaded, which published_at does not. Forgejo does not
// report it at all, so those assets fall back to created_at.
func assetDate(a asset) time.Time {
	if !a.UpdatedAt.IsZero() {
		return a.UpdatedAt
	}
	return a.CreatedAt
}

// releaseFallbackDate is only for releases the regex never picked a file from,
// so the preview can still show a date next to them. It never reaches the
// generated source: the version date is always the matched file's own
// timestamp, because that is the file the device will actually download.
func releaseFallbackDate(rel release) time.Time {
	var newest time.Time
	for _, a := range rel.Assets {
		if t := assetDate(a); t.After(newest) {
			newest = t
		}
	}
	if !newest.IsZero() {
		return newest
	}
	if !rel.PublishedAt.IsZero() {
		return rel.PublishedAt
	}
	return rel.CreatedAt
}

// collapseSpace folds every run of whitespace into a single space, so a
// multi-line markdown body fits on the tester's one preview line.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateRunes cuts s down to n runes, marking the cut. Runes rather than
// bytes, so a CJK release note is not chopped mid-character.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

// versionFromTag derives a version string. With a versionRegex the first
// capture group wins; otherwise a leading "v" is stripped.
func versionFromTag(tag string, re *regexp.Regexp) string {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return ""
	}
	if re != nil {
		m := re.FindStringSubmatch(tag)
		if len(m) > 1 {
			return strings.TrimSpace(m[1])
		}
		if len(m) == 1 {
			return strings.TrimSpace(m[0])
		}
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(tag, "v"), "V"))
}
