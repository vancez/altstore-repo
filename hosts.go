package main

import (
	"context"
	"net/url"
	"strings"
)

// A repoHost is one place builds can be read from.
//
// Three things are kept apart on purpose:
//
//   - the id names the *site* ("starforge"), never the software it runs and
//     never its domain, so a site can move to another domain without
//     invalidating a single stored config;
//   - the domain is only how links and messages spell that site;
//   - the dialect is the software — the API shape. Two Forgejo sites differ in
//     nothing but the row they sit in, so a second one reuses the whole Forgejo
//     path by getting dialectForgejo.
//
// Adding a site takes three edits: a row here, a token field on RepoConfig if
// it needs one, and its name in the settings drawer's markup. The app form's
// picker needs nothing — it is built from this table, which reaches the UI
// through /api/sites.
type repoHost struct {
	id      string  // stored in AppConfig.RepoType
	label   string  // brand name for the picker; never translated
	domain  string  // hostname: what links contain and what messages name
	apiBase string  // REST API root, no trailing slash
	dialect dialect // the API to speak
	token   func(RepoConfig) string
}

// dialect is the API a host speaks. It is the only thing the fetch code
// branches on, which is what lets one implementation serve every host that runs
// the same software.
type dialect string

const (
	dialectGitHub  dialect = "github"
	dialectForgejo dialect = "forgejo"
)

// defaultRepoType is what an app without a repoType means. Config files written
// before the choice existed therefore keep reading from GitHub untouched.
const defaultRepoType = "github"

var repoHosts = []repoHost{
	{
		id:      "github",
		label:   "GitHub",
		domain:  "github.com",
		apiBase: "https://api.github.com",
		dialect: dialectGitHub,
		token:   func(c RepoConfig) string { return c.GitHubToken },
	},
	{
		id:      "starforge",
		label:   "Starforge",
		domain:  "starforgejo.dev",
		apiBase: "https://starforgejo.dev/api/v1",
		dialect: dialectForgejo,
		token:   func(c RepoConfig) string { return c.StarforgeToken },
	},
}

// repoHostByType resolves a stored repoType. An unknown id is an error rather
// than a fallback: quietly reading the wrong host would be worse than saying so.
func repoHostByType(id string) (repoHost, bool) {
	if strings.TrimSpace(id) == "" {
		id = defaultRepoType
	}
	for _, h := range repoHosts {
		if h.id == id {
			return h, true
		}
	}
	return repoHost{}, false
}

// repoHostByDomain matches a hostname found in a pasted link, ignoring case, a
// leading www. and any port.
func repoHostByDomain(name string) (repoHost, bool) {
	name = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(name)), "www.")
	for _, h := range repoHosts {
		if h.domain == name {
			return h, true
		}
	}
	return repoHost{}, false
}

// domainList is the "github.com, starforgejo.dev" tail of the unknown-host
// error, built from the table so it cannot go stale.
func domainList() string {
	names := make([]string, len(repoHosts))
	for i, h := range repoHosts {
		names[i] = h.domain
	}
	return strings.Join(names, ", ")
}

// normalizeRepo turns the many ways people paste a repo into "owner/name" plus
// the host it should be read from. A bare owner/name keeps whatever host the
// form has selected; a link or a host-prefixed path decides the host itself, so
// a stale dropdown cannot silently point somewhere else.
func normalizeRepo(ctx context.Context, in, fallbackType string) (string, repoHost, error) {
	s := strings.TrimSpace(in)
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	if s == "" {
		return "", repoHost{}, trf(ctx, msgRepoFormat)
	}

	host, ok := repoHostByType(fallbackType)
	if !ok {
		return "", repoHost{}, trf(ctx, msgRepoTypeUnknown, fallbackType)
	}

	if name, rest, ok := splitHostPrefix(s); ok {
		known, found := repoHostByDomain(name)
		if !found {
			return "", repoHost{}, trf(ctx, msgRepoUnknownHost, name, domainList())
		}
		host, s = known, rest
	}

	s = strings.Trim(s, "/")
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", repoHost{}, trf(ctx, msgRepoFormat)
	}
	// Reject things that clearly aren't a repo slug. "." and ".." would climb
	// out of the /repos/ path once this is pasted into an API URL.
	for _, p := range parts {
		if strings.ContainsAny(p, " \t?#") || p == "." || p == ".." {
			return "", repoHost{}, trf(ctx, msgRepoBadChars, in)
		}
	}
	return parts[0] + "/" + parts[1], host, nil
}

// splitHostPrefix pulls the hostname out of anything that names one: a URL, a
// git remote, or a "host/owner/repo" path. A bare owner/repo is left alone —
// owner names may contain a dot, so only a name we recognise counts as a host.
func splitHostPrefix(s string) (host, rest string, ok bool) {
	switch {
	case strings.Contains(s, "://"):
		u, err := url.Parse(s)
		if err != nil || u.Hostname() == "" {
			return "", "", false
		}
		return u.Hostname(), strings.TrimPrefix(u.Path, "/"), true
	case strings.HasPrefix(s, "git@"):
		// git@github.com:owner/repo
		name, path, found := strings.Cut(strings.TrimPrefix(s, "git@"), ":")
		if !found {
			return "", "", false
		}
		return name, path, true
	}
	if name, rest, found := strings.Cut(s, "/"); found {
		if _, known := repoHostByDomain(name); known {
			return name, rest, true
		}
	}
	return "", "", false
}
