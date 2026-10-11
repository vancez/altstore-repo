package main

import (
	"context"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// AltStore source document (https://docs.altstore.io/developers/apps/app-sources)
// ---------------------------------------------------------------------------

type SourceDoc struct {
	Name         string      `json:"name"`
	Identifier   string      `json:"identifier"`
	Subtitle     string      `json:"subtitle,omitempty"`
	Description  string      `json:"description,omitempty"`
	IconURL      string      `json:"iconURL,omitempty"`
	HeaderURL    string      `json:"headerURL,omitempty"`
	Website      string      `json:"website,omitempty"`
	TintColor    string      `json:"tintColor,omitempty"`
	FeaturedApps []string    `json:"featuredApps,omitempty"`
	Apps         []SourceApp `json:"apps"`
	News         []any       `json:"news"`
}

type SourceApp struct {
	Name                 string   `json:"name"`
	BundleIdentifier     string   `json:"bundleIdentifier"`
	DeveloperName        string   `json:"developerName"`
	Subtitle             string   `json:"subtitle,omitempty"`
	LocalizedDescription string   `json:"localizedDescription,omitempty"`
	IconURL              string   `json:"iconURL,omitempty"`
	TintColor            string   `json:"tintColor,omitempty"`
	Category             string   `json:"category,omitempty"`
	Screenshots          []string `json:"screenshots,omitempty"`

	// The newest version repeated at the app level, which is how Feather and
	// older AltStore builds read it. AltStore itself ignores these in favour of
	// the versions array, but Feather keys an app's identity off the app-level
	// downloadURL and falls back to a random id when it is missing.
	Version     string `json:"version,omitempty"`
	VersionDate string `json:"versionDate,omitempty"`
	Size        int64  `json:"size,omitempty"`
	DownloadURL string `json:"downloadURL,omitempty"`

	Versions []SourceVersion `json:"versions"`
}

type SourceVersion struct {
	Version              string `json:"version"`
	Date                 string `json:"date"`
	DownloadURL          string `json:"downloadURL"`
	Size                 int64  `json:"size"`
	MinOSVersion         string `json:"minOSVersion,omitempty"`
	LocalizedDescription string `json:"localizedDescription,omitempty"`
}

// appResult pairs an app with its resolved versions, or with the error that
// stopped it. One broken repo must not take down the whole source.
type appResult struct {
	app      AppConfig
	versions []ResolvedVersion
	err      error
}

// BuildSource resolves apps concurrently and assembles one feed. group == ""
// means every app (the all.json feed). found is false when the named group does
// not exist, which the caller turns into a 404.
//
// Resolving is network-bound, so a feed with a dozen apps would be painfully
// slow serially.
func BuildSource(ctx context.Context, st *Store, cl *Releases, group string) (doc SourceDoc, found bool) {
	cfg := st.Repo()
	apps := st.Apps()

	if group != "" {
		g, ok := st.Group(group)
		if !ok {
			return SourceDoc{}, false
		}
		cfg = applyGroup(cfg, g)
	}

	ttl := time.Duration(cfg.CacheTTL) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}

	results := make([]appResult, len(apps))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6) // be polite to the release APIs

	for i, app := range apps {
		if !app.Enabled || (group != "" && app.Group != group) {
			continue
		}
		wg.Add(1)
		go func(i int, app AppConfig) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r, err := Resolve(ctx, cl, app, cfg, ttl, false)
			if err != nil {
				results[i] = appResult{app: app, err: err}
				return
			}
			results[i] = appResult{app: app, versions: r.Versions}
		}(i, app)
	}
	wg.Wait()

	doc = SourceDoc{
		Name:        cfg.Name,
		Identifier:  cfg.Identifier,
		Subtitle:    cfg.Subtitle,
		Description: cfg.Description,
		IconURL:     cfg.IconURL,
		HeaderURL:   cfg.HeaderURL,
		Website:     cfg.Website,
		TintColor:   cfg.TintColor,
		Apps:        []SourceApp{},
		News:        []any{},
	}

	for _, r := range results {
		// An enabled app with nothing to install would show up as a broken
		// entry on the device, so drop it rather than ship it empty.
		if r.app.ID == "" || r.err != nil || len(r.versions) == 0 {
			continue
		}
		doc.Apps = append(doc.Apps, toSourceApp(r.app, r.versions))
	}

	// Featured apps are repo-level, so a group only advertises the ones it
	// actually contains.
	if len(cfg.FeaturedApps) > 0 {
		present := make(map[string]bool, len(doc.Apps))
		for _, a := range doc.Apps {
			present[a.BundleIdentifier] = true
		}
		for _, id := range cfg.FeaturedApps {
			if present[id] {
				doc.FeaturedApps = append(doc.FeaturedApps, id)
			}
		}
	}
	return doc, true
}

// applyGroup overlays a group's metadata on the repo config. An empty group
// field inherits the repo value; name and identifier are derived when unset so
// a brand-new group is immediately usable.
func applyGroup(cfg RepoConfig, g GroupConfig) RepoConfig {
	if g.Name != "" {
		cfg.Name = g.Name
	} else {
		cfg.Name = cfg.Name + " · " + g.Slug
	}
	if g.Identifier != "" {
		cfg.Identifier = g.Identifier
	} else {
		cfg.Identifier = cfg.Identifier + "." + g.Slug
	}
	if g.Subtitle != "" {
		cfg.Subtitle = g.Subtitle
	}
	if g.Description != "" {
		cfg.Description = g.Description
	}
	if g.IconURL != "" {
		cfg.IconURL = g.IconURL
	}
	if g.HeaderURL != "" {
		cfg.HeaderURL = g.HeaderURL
	}
	if g.Website != "" {
		cfg.Website = g.Website
	}
	if g.TintColor != "" {
		cfg.TintColor = g.TintColor
	}
	return cfg
}

func toSourceApp(app AppConfig, versions []ResolvedVersion) SourceApp {
	// Overrides only touch the newest version; older ones stay as the host reported.
	if len(versions) > 0 {
		v := &versions[0]
		if s := strings.TrimSpace(app.VersionOverride); s != "" {
			v.Version = s
		}
		if s := strings.TrimSpace(app.DateOverride); s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				v.Date = t.UTC().Format(time.RFC3339)
			} else if t, err := time.Parse("2006-01-02", s); err == nil {
				v.Date = t.UTC().Format(time.RFC3339)
			}
		}
		if s := strings.TrimSpace(app.DownloadOverride); s != "" {
			v.DownloadURL = s
		}
		if app.SizeOverride > 0 {
			v.Size = app.SizeOverride
		}
	}

	out := SourceApp{
		Name:                 app.Name,
		BundleIdentifier:     app.BundleID,
		DeveloperName:        app.Developer,
		Subtitle:             app.Subtitle,
		LocalizedDescription: app.Description,
		IconURL:              app.IconURL,
		TintColor:            app.TintColor,
		Category:             app.Category,
		Screenshots:          app.Screenshots,
		Versions:             []SourceVersion{},
	}
	if out.Name == "" {
		out.Name = app.Repo
	}
	if out.DeveloperName == "" {
		out.DeveloperName = strings.SplitN(app.Repo, "/", 2)[0]
	}

	for _, v := range versions {
		// The version-level description is AltStore's changelog field; leaving it
		// empty is better than repeating the app description on every release.
		out.Versions = append(out.Versions, SourceVersion{
			Version:      v.Version,
			Date:         v.Date,
			DownloadURL:  v.DownloadURL,
			Size:         v.Size,
			MinOSVersion: app.MinOSVersion,
		})
	}

	// Copy the newest version up for Feather. This happens after the overrides
	// above, so both copies of the version agree.
	if len(out.Versions) > 0 {
		newest := out.Versions[0]
		out.Version = newest.Version
		out.VersionDate = newest.Date
		out.Size = newest.Size
		out.DownloadURL = newest.DownloadURL
	}
	return out
}
