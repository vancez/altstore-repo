# AltStore Source Manager

*[中文文档](README.zh-CN.md)*

A live AltStore / SideStore source generator written in Go, with an admin UI that works on desktop and mobile.

You give it a repository and one regex. The version number, download URL, upload time and file size are pulled from that repository's release API on every request — GitHub by default, or a Forgejo instance such as starforgejo.dev, picked per app. The things the host cannot know — app name, bundle ID and so on — you fill in through the web UI.

## Running

```bash
go build -o bin/altstore-repo .
./bin/altstore-repo -addr :8080 -data ./data
```

| Flag | Default | Meaning |
|---|---|---|
| `-addr` | `:8080` | Listen address |
| `-data` | `data` | Config directory; state lives in `config.json` |
| `-password` | empty | Admin UI password, or set `ALTSTORE_PASSWORD` |

Open `http://localhost:8080/` for the admin UI. The feed is at `http://localhost:8080/source.json`.

Outbound requests go through Go's default transport, so `HTTPS_PROXY` / `HTTP_PROXY` are honoured if the hosts are slow or blocked where this runs.

With a password set, every path except the published feeds requires HTTP Basic auth. A phone has to be able to fetch a source anonymously; the admin UI should not be reachable without credentials.

## Adding an app

1. **Repository** — `owner/repo`, or paste a full link. A link carries its host with it and sets **Source host** for you; a bare `owner/repo` uses whatever that dropdown says.
2. **Asset regex** — picks the file to publish out of the release's assets. It matches against asset **filenames**; if several match, the first in the release's order wins. Empty means `(?i)\.ipa$`. Start the pattern with `(?i)` to ignore case.
3. Press **Test**. It fetches the releases immediately and lists, for each one, which file it resolved to or why it was skipped. Save once that looks right.

The version number defaults to the release tag with a leading `v` stripped. For a tag like `build-1234`, put a regex with a capture group under Version extraction and group 1 becomes the version.

**Release notes become the changelog.** Each version carries its own release's notes in AltStore's `localizedDescription`, which is the text the device shows under a version's history. A release whose notes are empty simply has no changelog; nothing is invented to fill the gap.

## Source hosts

Each app reads its releases from one host, chosen per app:

| Source host | API | Notes |
|---|---|---|
| GitHub | REST v3 | the default |
| Starforge (starforgejo.dev) | Forgejo / Gitea | for mirrors and builds kept off GitHub |

One source can mix both — apps are resolved independently, and the two hosts never share a cache entry.

What is different on a Forgejo host:

- **External links.** A mirrored release usually does not upload its files; it links them to wherever they really live, GitHub most of the time. Those files report **no size** (Forgejo cannot know it) and are only as durable as the site they point at, so they are skipped by default. Tick **Include external links** in the app form to accept them anyway; the regex tester then shows them as picked rather than skipped.
- **No ETags.** Forgejo has no conditional requests, so once the cache window is up the release list is read in full. The cache time under Repo settings still applies and is still what keeps this cheap.
- **Version dates** come from the file's `created_at`. Forgejo does not report `updated_at`, which is what GitHub builds use to date a version.
- **Tokens.** Public repos need nothing. Private ones need a Forgejo access token under Repo settings; it is only ever sent to starforgejo.dev, never to GitHub.

## Groups: one source, several feeds

Create groups under **Groups**. Each one gets its own feed URL containing only the apps assigned to it. Pick one from the **Group** dropdown in the app form, or leave it empty.

That dropdown also offers **New group…**, which creates the group as part of saving the app: the group takes the app's name, its slug is that name with the spaces turned into `-`, and its subtitle, description, icon and colour are copied from the app. The identifier is left empty. Saving fails if a group with that name or slug already exists.

A name that has no `a-z`, `0-9`, `-` or `_` in it at all — 中文 ones, say — slugs to nothing, and the form then asks for a slug by hand.

| URL | Contents |
|---|---|
| `/source/all.json` | every app |
| `/source.json` | same thing, kept as an alias |
| `/source/<slug>.json` | only that group's apps |

Several apps sharing a group slug are merged into that one feed. Apps with no group appear in `all.json` only.

A slug may contain `a-z`, `0-9`, `-` and `_`, up to 64 characters; `all` is reserved. It **cannot be changed after creation**, because apps reference the group by that name.

A group's name and identifier are derived when left empty:

- name → `"<source name> · <slug>"`
- identifier → `"<source identifier>.<slug>"`

The remaining appearance fields (subtitle, description, icon, header image, website, tint colour) fall back to the source's own values. If you do set an identifier, it must not collide with the source itself or another group — AltStore tells sources apart by identifier, and a collision makes two feeds show up as one on the device.

Deleting a group releases its apps to ungrouped. They stay in `all.json`, and no app is deleted.

## Behaviour notes

- **Everything is ordered by name.** Apps sort by their name (falling back to the repo), groups by their name (falling back to the slug), case-insensitively. The app list, `config.json` and every generated feed use that same order, so there is no manual ordering to maintain.
- **Each app carries its newest version twice**: in the `versions` array, which is what AltStore reads, and again at the app level as `version`, `versionDate`, `size` and `downloadURL`, which is where Feather and older AltStore builds look. The two always agree, overrides included. Feather keys an app's identity off the app-level `downloadURL`, so leaving it out makes it treat the app as new on every refresh.
- **Version date** comes from the matched asset's own `updated_at` — not `published_at`, and not some other file in the same release. GitHub does not move the publish time when an asset is re-uploaded, so a release published in 2023 whose ipa was replaced yesterday is honestly "yesterday". Falls back to that asset's `created_at`, which is the only timestamp a Forgejo host reports. This date is also what versions are sorted by.
- In the regex tester, **a release that matched nothing still shows a date**. That is the newest asset time in that release, for orientation only; it never reaches the generated feed.
- **Changelogs are the release's own notes**, verbatim and markdown included, one per version. A version whose release has an empty body has no changelog rather than a copy of the app description. The regex tester shows the opening of each matched release's notes next to the file it picked.
- **Pre-releases** are excluded by default; tick the box under Version extraction to include them.
- **Draft releases** are always skipped.
- **Duplicate version numbers** are deduplicated, keeping the newest.
- **The feed only includes apps with at least one usable version.** An enabled app where nothing matched is left out — otherwise it shows up on the device as an entry that cannot be installed.
- **GitHub rate limits**: 60 requests/hour without a token. There is an ETag-aware cache between this service and GitHub, configurable under Repo settings and 300 seconds by default. When it expires, a conditional request is sent first and a 304 costs nothing. If the quota really does run out, the stale cache keeps being served rather than the feed going empty.
- **Private repos** need a token under Repo settings; a read-only GitHub `repo` scope is enough, and a Forgejo token needs no scopes for public repositories.

## HTTP interface

| Method | Path | Meaning |
|---|---|---|
| `GET` | `/source.json` | Every app. Public, sends CORS headers |
| `GET` | `/source/all.json` | Same as above |
| `GET` | `/source/<slug>.json` | One group |
| `GET` | `/` | Admin UI |
| `GET` `PUT` | `/api/config` | Source metadata, tokens, cache time |
| `GET` `POST` | `/api/groups` | List groups (with app counts) / create |
| `PUT` `DELETE` | `/api/groups/{slug}` | Update / delete |
| `GET` `POST` | `/api/apps` | List apps / create |
| `PUT` `DELETE` | `/api/apps/{id}` | Update / delete |
| `GET` | `/api/apps/{id}/versions` | Resolved versions for one app; `?force=1` bypasses the cache |
| `POST` | `/api/resolve` | Dry run; this is what the regex tester calls |

Error messages are localized: send `Accept-Language: zh-CN` to get Chinese, anything else (or nothing) gets English.

## Interface language

The admin UI ships in English and Chinese. English is the default; the toggle in the header remembers your choice, and `?lang=zh` or `?lang=en` makes a specific language linkable.

Translations live in `web/i18n.js` (UI) and `i18n.go` (backend messages). Both fall back to English for anything untranslated.

## Configuration

All state lives in `data/config.json` — plain JSON, safe to edit by hand. Writes go to a temp file and are renamed into place, so a crash mid-write cannot leave a half-written file behind.

Tokens — GitHub's and Starforge's — are stored in plaintext, and each app records which host it reads from in `repoType`: `github` unless set, or `starforge`. The id names the site rather than its domain, so a site can move without invalidating stored configs. This service is meant to run for you and you alone, so keep `data/` out of any public repository.

## License

GPL-3.0. See [LICENSE](LICENSE).
