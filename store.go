package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// GroupConfig is a named feed. Each group publishes its own source document at
// /source/<slug>.json containing only the apps assigned to it. Empty fields
// fall back to the repo-level value, so a group only needs the bits that differ.
type GroupConfig struct {
	Slug        string `json:"slug"` // URL segment; "all" is reserved
	Name        string `json:"name"`
	Identifier  string `json:"identifier"`
	Subtitle    string `json:"subtitle"`
	Description string `json:"description"`
	IconURL     string `json:"iconURL"`
	HeaderURL   string `json:"headerURL"`
	Website     string `json:"website"`
	TintColor   string `json:"tintColor"`
}

// AppConfig is one managed app: a repo on one of the supported hosts, plus the
// metadata AltStore needs that no release API can tell us.
type AppConfig struct {
	ID string `json:"id"`

	Group string `json:"group"` // GroupConfig.Slug, "" = only in all.json

	// Where the builds come from.
	Repo              string `json:"repo"`         // "owner/name"
	RepoType          string `json:"repoType"`     // repoHosts id, "" = github
	AssetRegex        string `json:"assetRegex"`   // filters release asset filenames
	VersionRegex      string `json:"versionRegex"` // optional; capture group 1 pulls the version out of the tag
	IncludePrerelease bool   `json:"includePrerelease"`
	IncludeExternal   bool   `json:"includeExternal"` // Forgejo: also accept files that are only links elsewhere
	MaxVersions       int    `json:"maxVersions"`     // 0 = all

	// Filled in by hand.
	Name         string   `json:"name"`
	BundleID     string   `json:"bundleIdentifier"`
	Developer    string   `json:"developerName"`
	Subtitle     string   `json:"subtitle"`
	Description  string   `json:"localizedDescription"`
	IconURL      string   `json:"iconURL"`
	TintColor    string   `json:"tintColor"`
	Category     string   `json:"category"`
	Screenshots  []string `json:"screenshots"`
	MinOSVersion string   `json:"minOSVersion"`
	Enabled      bool     `json:"enabled"`

	// Escape hatches, applied to the newest resolved version only.
	VersionOverride  string `json:"versionOverride"`
	DateOverride     string `json:"dateOverride"`
	DownloadOverride string `json:"downloadOverride"`
	SizeOverride     int64  `json:"sizeOverride"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// RepoConfig is the source-level metadata every AltStore source carries.
type RepoConfig struct {
	Name           string   `json:"name"`
	Identifier     string   `json:"identifier"`
	Subtitle       string   `json:"subtitle"`
	Description    string   `json:"description"`
	IconURL        string   `json:"iconURL"`
	HeaderURL      string   `json:"headerURL"`
	Website        string   `json:"website"`
	TintColor      string   `json:"tintColor"`
	FeaturedApps   []string `json:"featuredApps"`
	GitHubToken    string   `json:"githubToken"`
	StarforgeToken string   `json:"starforgeToken"` // starforgejo.dev; empty for public repos
	CacheTTL       int      `json:"cacheTTL"`       // seconds; GitHub allows 60 req/h unauthenticated
}

// DB is the whole on-disk state.
type DB struct {
	Repo   RepoConfig    `json:"repo"`
	Groups []GroupConfig `json:"groups"`
	Apps   []AppConfig   `json:"apps"`
}

func defaultDB() DB {
	return DB{
		Repo: RepoConfig{
			Name:       "My AltStore Source",
			Identifier: "com.example.altstore.source",
			Subtitle:   "Sideloaded builds",
			TintColor:  "#E8B04B",
			CacheTTL:   300,
		},
		Groups: []GroupConfig{},
		Apps:   []AppConfig{},
	}
}

// Store owns the state file. Every mutation rewrites it atomically.
type Store struct {
	mu   sync.RWMutex
	path string
	db   DB
}

func NewStore(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	s := &Store{path: filepath.Join(dataDir, "config.json"), db: defaultDB()}

	raw, err := os.ReadFile(s.path)
	switch {
	case os.IsNotExist(err):
		if err := s.save(); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", s.path, err)
	default:
		if err := json.Unmarshal(raw, &s.db); err != nil {
			return nil, fmt.Errorf("parse %s: %w (move it aside to start fresh)", s.path, err)
		}
		// Configs written before groups existed simply lack the key.
		if s.db.Apps == nil {
			s.db.Apps = []AppConfig{}
		}
		if s.db.Groups == nil {
			s.db.Groups = []GroupConfig{}
		}
		// Same for the source host: an app without one means GitHub, and saying
		// so here keeps the form's picker from coming up empty.
		for i := range s.db.Apps {
			if s.db.Apps[i].RepoType == "" {
				s.db.Apps[i].RepoType = defaultRepoType
			}
		}
		// A file written by an older build may be in hand-picked order.
		sortDB(&s.db)
	}
	return s, nil
}

// save writes the DB to a temp file and renames it into place, so a crash
// mid-write can never leave a truncated config behind. Callers hold the lock.
// Entries are sorted here rather than at each call site, which is what keeps
// the in-memory order, the file and the feeds identical.
func (s *Store) save() error {
	sortDB(&s.db)
	raw, err := json.MarshalIndent(s.db, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) Repo() RepoConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.db.Repo
}

func (s *Store) SetRepo(r RepoConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.Repo = r
	return s.save()
}

func (s *Store) Apps() []AppConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]AppConfig, len(s.db.Apps))
	copy(out, s.db.Apps)
	return out
}

func (s *Store) App(id string) (AppConfig, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.db.Apps {
		if a.ID == id {
			return a, true
		}
	}
	return AppConfig{}, false
}

func (s *Store) AddApp(a AppConfig) (AppConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a.ID = newID()
	a.CreatedAt = time.Now().UTC()
	a.UpdatedAt = a.CreatedAt
	s.db.Apps = append(s.db.Apps, a)
	return a, s.save()
}

func (s *Store) UpdateApp(id string, a AppConfig) (AppConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Apps {
		if s.db.Apps[i].ID != id {
			continue
		}
		a.ID = id
		a.CreatedAt = s.db.Apps[i].CreatedAt
		a.UpdatedAt = time.Now().UTC()
		s.db.Apps[i] = a
		return a, s.save()
	}
	return AppConfig{}, os.ErrNotExist
}

func (s *Store) DeleteApp(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Apps {
		if s.db.Apps[i].ID == id {
			s.db.Apps = append(s.db.Apps[:i], s.db.Apps[i+1:]...)
			return s.save()
		}
	}
	return os.ErrNotExist
}

// ---------------------------------------------------------------------------
// ordering
// ---------------------------------------------------------------------------

// Apps and groups are ordered by name everywhere — the file, the API and the
// generated feeds — so there is no manual order to keep in sync. Sorting runs
// at load and in save, the one point every mutation goes through.
func sortDB(db *DB) {
	sort.SliceStable(db.Apps, func(i, j int) bool {
		return lessName(appSortKey(db.Apps[i]), appSortKey(db.Apps[j]))
	})
	sort.SliceStable(db.Groups, func(i, j int) bool {
		return lessName(groupSortKey(db.Groups[i]), groupSortKey(db.Groups[j]))
	})
}

// appSortKey is what the list sorts on: the name shown in the UI, which falls
// back to the repo when an app has none.
func appSortKey(a AppConfig) string {
	if a.Name != "" {
		return a.Name
	}
	return a.Repo
}

func groupSortKey(g GroupConfig) string {
	if g.Name != "" {
		return g.Name
	}
	return g.Slug
}

// lessName compares case-insensitively, then exactly, so two entries differing
// only in case still come out in a fixed order.
func lessName(a, b string) bool {
	la, lb := strings.ToLower(a), strings.ToLower(b)
	if la != lb {
		return la < lb
	}
	return a < b
}

// ---------------------------------------------------------------------------
// groups
// ---------------------------------------------------------------------------

// allSlug is the feed that always contains every app, so it cannot be taken.
const allSlug = "all"

func (s *Store) Groups() []GroupConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]GroupConfig, len(s.db.Groups))
	copy(out, s.db.Groups)
	return out
}

func (s *Store) Group(slug string) (GroupConfig, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, g := range s.db.Groups {
		if g.Slug == slug {
			return g, true
		}
	}
	return GroupConfig{}, false
}

// GroupCounts returns how many apps are in each group. Ungrouped apps are not
// counted under any key.
func (s *Store) GroupCounts() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]int{}
	for _, a := range s.db.Apps {
		if a.Group != "" {
			out[a.Group]++
		}
	}
	return out
}

func (s *Store) AddGroup(ctx context.Context, g GroupConfig) (GroupConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.db.Groups {
		if existing.Slug == g.Slug {
			return GroupConfig{}, trf(ctx, msgGroupExists, g.Slug)
		}
		// Two groups sharing a name are indistinguishable in the UI, and the
		// name is what the app form derives a new group from.
		if g.Name != "" && strings.EqualFold(existing.Name, g.Name) {
			return GroupConfig{}, trf(ctx, msgGroupNameExists, g.Name)
		}
	}
	s.db.Groups = append(s.db.Groups, g)
	return g, s.save()
}

func (s *Store) UpdateGroup(slug string, g GroupConfig) (GroupConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Groups {
		if s.db.Groups[i].Slug != slug {
			continue
		}
		// Renaming the slug would orphan every app pointing at the old one.
		g.Slug = slug
		s.db.Groups[i] = g
		return g, s.save()
	}
	return GroupConfig{}, os.ErrNotExist
}

// DeleteGroup removes a group and releases its apps to ungrouped rather than
// deleting them. Returns how many apps were affected.
func (s *Store) DeleteGroup(slug string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Groups {
		if s.db.Groups[i].Slug != slug {
			continue
		}
		s.db.Groups = append(s.db.Groups[:i], s.db.Groups[i+1:]...)
		released := 0
		for j := range s.db.Apps {
			if s.db.Apps[j].Group == slug {
				s.db.Apps[j].Group = ""
				released++
			}
		}
		return released, s.save()
	}
	return 0, os.ErrNotExist
}

// normalizeSlug coerces a group name into something safe to put in a URL path.
// Anything that is not a latin letter, digit, dash or underscore is dropped,
// which means a pure-CJK name slugs to empty and the caller must ask for one.
func normalizeSlug(ctx context.Context, in string) (string, error) {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(in)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			dash = false
		case r == '-' || r == ' ' || r == '.':
			if b.Len() > 0 && !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "", trf(ctx, msgGroupSlugEmpty)
	}
	if len(out) > 64 {
		return "", trf(ctx, msgGroupSlugLong)
	}
	if out == allSlug {
		return "", trf(ctx, msgGroupSlugReserved, allSlug)
	}
	return out, nil
}

func newID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("app%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
