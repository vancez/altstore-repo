package main

import (
	"archive/zip"
	"bytes"
	"context"
	"image"
	"path"
	"strings"

	carfile "github.com/devcxm/carfile-go"
)

// A compiled asset catalog can be large, but it is still a single zip member;
// past this size it is not worth the download and the loose primary icon is a
// better fallback.
const maxAssetsCarSize = 64 << 20

// primaryIconNames returns the names under CFBundleIcons/CFBundleIcons~ipad's
// CFBundlePrimaryIcon. Both the icon-set name (for Assets.car) and the listed
// loose file base names (for legacy bundles) are included.
func primaryIconNames(meta map[string]any) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = normalizeIconBase(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, key := range []string{"CFBundleIcons", "CFBundleIcons~ipad"} {
		icons, ok := meta[key].(map[string]any)
		if !ok {
			continue
		}
		primary, ok := icons["CFBundlePrimaryIcon"].(map[string]any)
		if !ok {
			continue
		}
		collectIconStrings(primary["CFBundleIconFiles"], add)
		collectIconStrings(primary["CFBundleIconName"], add)
	}
	return out
}

// legacyIconBases covers entries that predate CFBundleIcons. The generic
// AppIcon/Icon/iTunesArtwork search in iconCandidates is a further fallback.
func legacyIconBases(meta map[string]any) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = normalizeIconBase(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	collectIconStrings(meta["CFBundleIconFile"], add)
	collectIconStrings(meta["CFBundleIconFiles"], add)
	return out
}

func collectIconStrings(v any, add func(string)) {
	switch s := v.(type) {
	case string:
		add(s)
	case []string:
		for _, e := range s {
			add(e)
		}
	case []any:
		for _, e := range s {
			collectIconStrings(e, add)
		}
	}
}

// pickPrimaryIcon looks only at the primary icon family. Within it, a default
// appearance is preferred over a dark/tinted rendition, and the largest
// candidate wins; only when the whole primary family is unusable does pickIcon
// fall back to the generic loose search.
func pickPrimaryIcon(ctx context.Context, zr *zip.Reader, z *remoteZip, appRoot string, names []string) (iconPick, bool) {
	var bestLoose iconPick
	haveLoose := false
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !strings.HasPrefix(f.Name, appRoot) {
			continue
		}
		rest := strings.TrimPrefix(f.Name, appRoot)
		if strings.Contains(rest, "/") || !primaryLooseName(rest, names) {
			continue
		}
		p, ok := readLooseIcon(ctx, z, f)
		if !ok {
			continue
		}
		if !haveLoose || p.area > bestLoose.area ||
			(p.area == bestLoose.area && len(p.data) > len(bestLoose.data)) {
			bestLoose, haveLoose = p, true
		}
	}

	var bestDefault, bestDark image.Image
	var defaultArea, darkArea int64 = -1, -1
	if car := assetsCarFile(zr, appRoot); car != nil {
		if raw, err := readZipFile(ctx, z, car, maxAssetsCarSize); err == nil {
			if catalog, err := carfile.Parse(bytes.NewReader(raw), int64(len(raw))); err == nil {
				for _, rendition := range catalog.Renditions {
					if rendition.CSI.Payload.CompressionType == nil ||
						!renditionMatchesIconNames(rendition, names) {
						continue
					}
					img, err := carfile.DecodeRenditionImage(rendition)
					if err != nil {
						continue
					}
					b := img.Bounds()
					area := int64(b.Dx()) * int64(b.Dy())
					if area <= 0 {
						continue
					}
					if renditionIsDark(rendition) {
						if area > darkArea {
							darkArea, bestDark = area, img
						}
					} else if area > defaultArea {
						defaultArea, bestDefault = area, img
					}
				}
			}
		}
	}

	switch {
	case bestDefault != nil:
		if !haveLoose || bestLoose.area <= defaultArea {
			if p, ok := pngPick(bestDefault); ok {
				return p, true
			}
		}
		if haveLoose {
			return bestLoose, true
		}
	case haveLoose:
		// A default loose icon is preferable to a dark-only catalog rendition.
		return bestLoose, true
	case bestDark != nil:
		if p, ok := pngPick(bestDark); ok {
			return p, true
		}
	}
	return iconPick{}, false
}

func primaryLooseName(name string, names []string) bool {
	ext := strings.ToLower(path.Ext(name))
	if ext != "" && ext != ".png" && ext != ".jpg" && ext != ".jpeg" {
		return false
	}
	stem := strings.ToLower(strings.TrimSuffix(name, path.Ext(name)))
	for _, n := range names {
		base := strings.ToLower(n)
		if stem == base || strings.HasPrefix(stem, base+"@") || strings.HasPrefix(stem, base+"~") {
			return true
		}
	}
	return false
}

// assetsCarFile finds the app bundle's own catalog, not a framework's or an
// extension's.
func assetsCarFile(zr *zip.Reader, appRoot string) *zip.File {
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !strings.HasPrefix(f.Name, appRoot) {
			continue
		}
		rest := strings.TrimPrefix(f.Name, appRoot)
		if !strings.Contains(rest, "/") && strings.EqualFold(rest, "Assets.car") {
			return f
		}
	}
	return nil
}

func renditionMatchesIconNames(r carfile.Rendition, names []string) bool {
	if r.AssetName != "" {
		for _, n := range names {
			if strings.EqualFold(r.AssetName, n) {
				return true
			}
		}
	}
	if r.CSI.Name != "" {
		stem := strings.TrimSuffix(r.CSI.Name, path.Ext(r.CSI.Name))
		for _, n := range names {
			if strings.EqualFold(stem, n) {
				return true
			}
		}
	}
	return false
}

// renditionIsDark reports a non-default appearance. Theme Appearance 0 is the
// normal icon; 1 is the dark variant. A rendition without the key is normal.
func renditionIsDark(r carfile.Rendition) bool {
	for _, k := range r.Key {
		if k.Name == "Theme Appearance" {
			return k.Value != 0
		}
	}
	return false
}
