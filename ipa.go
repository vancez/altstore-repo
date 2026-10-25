package main

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/jpeg" // iTunesArtwork and some marketing icons are JPEG
	"image/png"  // re-encoding Apple-optimized icons
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"

	cgbi "github.com/928799934/go-png-cgbi"
	"howett.net/plist"
)

const (
	maxPlistSize = 8 << 20
	maxIconSize  = 24 << 20
)

// Only the top-level bundle's Info.plist counts: every extension, watch app and
// framework inside Payload/ has its own, and those describe the helper rather
// than the app.
var appPlistPattern = regexp.MustCompile(`(?i)^Payload/[^/]+\.app/Info\.plist$`)

// ipaInfo is what the newest build says about itself.
type ipaInfo struct {
	Name       string
	BundleID   string
	Category   string
	Icon       []byte
	IconExt    string // ".png" or ".jpg"
	IconWidth  int
	IconHeight int
}

// readIPAInfo resolves a downloadable IPA as a remote zip and reads the parts
// the admin form needs. Nothing is written here; the caller decides what to do
// with the icon bytes.
func readIPAInfo(ctx context.Context, client *http.Client, downloadURL, auth string) (*ipaInfo, error) {
	z, err := newRemoteZip(ctx, client, downloadURL, auth)
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(z, z.size)
	if err != nil {
		return nil, trf(ctx, msgIPAInvalid, err)
	}

	var plistFile *zip.File
	for _, f := range zr.File {
		if appPlistPattern.MatchString(f.Name) {
			plistFile = f
			break
		}
	}
	if plistFile == nil {
		return nil, trf(ctx, msgIPANoPlist)
	}

	raw, err := readZipFile(ctx, z, plistFile, maxPlistSize)
	if err != nil {
		return nil, err
	}
	var meta map[string]any
	if _, err := plist.Unmarshal(raw, &meta); err != nil {
		return nil, trf(ctx, msgIPAPlist, err)
	}

	info := &ipaInfo{
		Name:     firstNonEmpty(plistString(meta, "CFBundleDisplayName"), plistString(meta, "CFBundleName")),
		BundleID: plistString(meta, "CFBundleIdentifier"),
		Category: altstoreCategory(plistString(meta, "LSApplicationCategoryType")),
	}
	appRoot := plistFile.Name[:strings.LastIndexByte(plistFile.Name, '/')+1]
	if info.Name == "" {
		// Some bundles carry no display or bundle name at all. The .app
		// directory name is still better than leaving the field untouched.
		dir := strings.TrimSuffix(appRoot, "/")
		base := path.Base(dir)
		info.Name = strings.TrimSuffix(base, path.Ext(base))
	}
	if info.Category == "" {
		// iOS bundles usually have no LSApplicationCategoryType; the category
		// is App Store metadata. IPAs packaged for the store also carry that
		// metadata at the archive root, so use it when it is there.
		info.Category = categoryFromITunesMetadata(ctx, zr, z)
	}

	info.Icon, info.IconExt, info.IconWidth, info.IconHeight = pickIcon(ctx, zr, z, appRoot, meta)
	return info, nil
}

// categoryFromITunesMetadata reads the root-level iTunesMetadata.plist that
// App Store IPAs carry. Its genre is a display name ("Photo & Video"), not the
// UTI Info.plist would use, so it has its own mapping.
func categoryFromITunesMetadata(ctx context.Context, zr *zip.Reader, z *remoteZip) string {
	var f *zip.File
	for _, candidate := range zr.File {
		if strings.EqualFold(candidate.Name, "iTunesMetadata.plist") {
			f = candidate
			break
		}
	}
	if f == nil {
		return ""
	}
	raw, err := readZipFile(ctx, z, f, maxPlistSize)
	if err != nil {
		return ""
	}
	var meta map[string]any
	if _, err := plist.Unmarshal(raw, &meta); err != nil {
		return ""
	}
	return altstoreGenre(plistString(meta, "genre"))
}

// readZipFile decompresses one member. It deliberately does not use
// zip.File.Open: that would wrap a section reader in a flate reader, and every
// small read of the stream would become another range request. Reading the
// compressed bytes in one go and inflating from memory keeps this to the
// handful of requests the block cache needs.
func readZipFile(ctx context.Context, z *remoteZip, f *zip.File, max int64) ([]byte, error) {
	if f.FileInfo().IsDir() {
		return nil, trf(ctx, msgIPARead, f.Name, "it is a directory")
	}
	if f.UncompressedSize64 > uint64(max) || f.CompressedSize64 > uint64(max)+(1<<20) {
		return nil, trf(ctx, msgIPATooLarge, f.Name)
	}

	off, err := f.DataOffset()
	if err != nil {
		return nil, trf(ctx, msgIPARead, f.Name, err)
	}
	const directReadThreshold = 1 << 20
	compSize := int64(f.CompressedSize64)
	var raw []byte
	if compSize > directReadThreshold {
		// A big member (Assets.car) is one contiguous range request instead of
		// one request per block.
		raw, err = z.readRange(off, compSize)
		if err != nil {
			return nil, trf(ctx, msgIPARead, f.Name, err)
		}
	} else {
		raw = make([]byte, compSize)
		if len(raw) > 0 {
			if _, err := io.ReadFull(io.NewSectionReader(z, off, compSize), raw); err != nil {
				return nil, trf(ctx, msgIPARead, f.Name, err)
			}
		}
	}

	var out []byte
	switch f.Method {
	case zip.Store:
		out = raw
	case zip.Deflate:
		fr := flate.NewReader(bytes.NewReader(raw))
		out, err = io.ReadAll(io.LimitReader(fr, int64(f.UncompressedSize64)+1))
		fr.Close()
		if err != nil {
			return nil, trf(ctx, msgIPARead, f.Name, err)
		}
	default:
		return nil, trf(ctx, msgIPAUnsupported, f.Name)
	}

	if uint64(len(out)) != f.UncompressedSize64 {
		return nil, trf(ctx, msgIPARead, f.Name, "unexpected uncompressed size")
	}
	if f.CRC32 != 0 && crc32.ChecksumIEEE(out) != f.CRC32 {
		return nil, trf(ctx, msgIPARead, f.Name, "checksum mismatch")
	}
	return out, nil
}

// Apple's asset compiler stores loose app icons as "CgBI" PNGs: a private
// chunk, raw deflate and premultiplied BGRA pixels. iOS reads them, ordinary
// browsers do not, and the admin UI shows these icons in an <img>. Converting
// once here means the file on disk and /icon/ serve a plain PNG everywhere.
var (
	pngSignature = []byte("\x89PNG\r\n\x1a\n")
	cgbiHeader   = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x04CgBI")
)

func standardPNG(data []byte) ([]byte, error) {
	if !bytes.HasPrefix(data, cgbiHeader) {
		return data, nil
	}
	img, err := cgbi.Decode(bytes.NewReader(normalizeCgBI(data)))
	if err != nil {
		return nil, err
	}
	unpremultiply(img)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// The four-byte CgBI chunk payload is a marker that differs between Xcode
// versions, while the pixel format behind it is the same. The decoder only
// accepts one marker value and rejects the rest as "bad CgBI data" (seen with
// 0x50002002, for example), so replace the marker and recompute that chunk's
// CRC before handing the file over. Only called once cgbiHeader has matched,
// which guarantees this chunk is first, four bytes long and followed by a CRC.
var cgbiMarker = []byte{0x50, 0x00, 0x20, 0x06}

func normalizeCgBI(data []byte) []byte {
	if len(data) < 24 {
		return data
	}
	out := append([]byte(nil), data...)
	copy(out[16:20], cgbiMarker)
	binary.BigEndian.PutUint32(out[20:24], crc32.ChecksumIEEE(out[12:20]))
	return out
}

// The CgBI decoder returns NRGBA for a premultiplied source, so translucent
// pixels would come out too dark if their channels were interpreted as straight
// alpha. Opaque icons — the overwhelming majority — are untouched.
func unpremultiply(img image.Image) {
	nrgba, ok := img.(*image.NRGBA)
	if !ok {
		return
	}
	for y := 0; y < nrgba.Rect.Dy(); y++ {
		row := nrgba.Pix[y*nrgba.Stride : y*nrgba.Stride+nrgba.Rect.Dx()*4]
		for i := 0; i+3 < len(row); i += 4 {
			a := uint32(row[i+3])
			if a == 0 || a == 255 {
				continue
			}
			for c := 0; c < 3; c++ {
				v := uint32(row[i+c]) * 255 / a
				if v > 255 {
					v = 255
				}
				row[i+c] = uint8(v)
			}
		}
	}
}

// decodeIconConfig asks the standard PNG/JPEG decoders directly. Going through
// image.DecodeConfig would also consider the CgBI format the conversion library
// registers, which could let an unconverted Apple PNG through and produce a
// file browsers cannot display.
func decodeIconConfig(data []byte) (image.Config, string, error) {
	switch {
	case bytes.HasPrefix(data, pngSignature):
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		return cfg, "png", err
	case len(data) >= 2 && data[0] == 0xFF && data[1] == 0xD8:
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
		return cfg, "jpeg", err
	default:
		return image.DecodeConfig(bytes.NewReader(data))
	}
}

// iconPick is one decoded icon, from a loose file or an Assets.car rendition.
type iconPick struct {
	data []byte
	ext  string // ".png" or ".jpg"
	w, h int
	area int64
}

func readLooseIcon(ctx context.Context, z *remoteZip, f *zip.File) (iconPick, bool) {
	data, err := readZipFile(ctx, z, f, maxIconSize)
	if err != nil {
		return iconPick{}, false
	}
	data, err = standardPNG(data)
	if err != nil {
		return iconPick{}, false
	}
	cfg, format, err := decodeIconConfig(data)
	if err != nil {
		return iconPick{}, false
	}
	ext := iconExtFor(format)
	if ext == "" || cfg.Width <= 0 || cfg.Height <= 0 {
		return iconPick{}, false
	}
	return iconPick{
		data: data, ext: ext, w: cfg.Width, h: cfg.Height,
		area: int64(cfg.Width) * int64(cfg.Height),
	}, true
}

func pngPick(img image.Image) (iconPick, bool) {
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return iconPick{}, false
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return iconPick{}, false
	}
	return iconPick{
		data: buf.Bytes(), ext: ".png", w: b.Dx(), h: b.Dy(),
		area: int64(b.Dx()) * int64(b.Dy()),
	}, true
}

// pickIcon prefers the icon named by CFBundlePrimaryIcon: its renditions in
// Assets.car first, then the loose files the primary icon names. Only when the
// bundle names no primary icon, or none of those candidates can be decoded,
// does it fall back to the loose AppIcon/Icon/iTunesArtwork search.
func pickIcon(ctx context.Context, zr *zip.Reader, z *remoteZip, appRoot string, meta map[string]any) ([]byte, string, int, int) {
	if names := primaryIconNames(meta); len(names) > 0 {
		if p, ok := pickPrimaryIcon(ctx, zr, z, appRoot, names); ok {
			return p.data, p.ext, p.w, p.h
		}
	}
	if p, ok := pickLegacyIcon(ctx, zr, z, appRoot, meta); ok {
		return p.data, p.ext, p.w, p.h
	}
	return nil, "", 0, 0
}

func pickLegacyIcon(ctx context.Context, zr *zip.Reader, z *remoteZip, appRoot string, meta map[string]any) (iconPick, bool) {
	var best iconPick
	found := false
	for _, f := range iconCandidates(zr, appRoot, meta) {
		p, ok := readLooseIcon(ctx, z, f)
		if !ok {
			continue
		}
		if !found || p.area > best.area || (p.area == best.area && len(p.data) > len(best.data)) {
			best, found = p, true
		}
	}
	return best, found
}

// iconCandidates returns the legacy loose-icon candidates: direct children of
// the .app bundle whose names look like icons, plus iTunesArtwork at the
// archive root. The primary-icon search in assetscar.go is tried first.
func iconCandidates(zr *zip.Reader, appRoot string, meta map[string]any) []*zip.File {
	bases := legacyIconBases(meta)
	seen := map[string]bool{}
	var out []*zip.File
	add := func(f *zip.File) {
		if f.FileInfo().IsDir() || seen[f.Name] {
			return
		}
		seen[f.Name] = true
		out = append(out, f)
	}

	for _, f := range zr.File {
		name := f.Name
		if strings.HasPrefix(name, appRoot) {
			rest := strings.TrimPrefix(name, appRoot)
			if strings.Contains(rest, "/") {
				continue // PlugIns, Watch, frameworks; not the app's own icon
			}
			if looksLikeIcon(rest, bases) {
				add(f)
			}
			continue
		}
		if !strings.Contains(name, "/") && strings.HasPrefix(path.Base(name), "iTunesArtwork") {
			add(f)
		}
	}
	return out
}

// looksLikeIcon accepts the names Info.plist points at, plus the two naming
// families Xcode uses for loose icons. Anything with a non-image extension is
// left alone, and image decoding is the final filter.
func looksLikeIcon(name string, bases []string) bool {
	ext := strings.ToLower(path.Ext(name))
	if ext != "" && ext != ".png" && ext != ".jpg" && ext != ".jpeg" {
		return false
	}
	stem := strings.TrimSuffix(name, path.Ext(name))
	lower := strings.ToLower(stem)
	for _, b := range bases {
		base := strings.ToLower(b)
		if lower == base || strings.HasPrefix(lower, base+"@") {
			return true
		}
	}
	return strings.HasPrefix(lower, "appicon") ||
		strings.HasPrefix(lower, "icon") ||
		strings.HasPrefix(lower, "itunesartwork")
}

func normalizeIconBase(s string) string {
	s = strings.TrimSpace(s)
	switch ext := strings.ToLower(path.Ext(s)); ext {
	case ".png", ".jpg", ".jpeg":
		s = strings.TrimSuffix(s, path.Ext(s))
	}
	return s
}

func iconExtFor(format string) string {
	switch format {
	case "png":
		return ".png"
	case "jpeg":
		return ".jpg"
	}
	return ""
}

// altstoreCategory maps the App Store category UTI in Info.plist onto
// AltStore's fixed vocabulary. A UTI we cannot place still says the app has a
// category, so "other" is more honest than leaving a stale value in the form.
func altstoreCategory(uti string) string {
	uti = strings.TrimSpace(uti)
	if uti == "" {
		return ""
	}
	if c, ok := categoryByUTI[uti]; ok {
		return c
	}
	if strings.HasPrefix(uti, "public.app-category.") && strings.HasSuffix(uti, "-games") {
		return "games"
	}
	if strings.HasPrefix(uti, "public.app-category.") {
		return "other"
	}
	return ""
}

// altstoreGenre maps the App Store genre names found in iTunesMetadata.plist.
// A genre with no AltStore equivalent still means the app has a category, so
// "other" is better than leaving a stale value in the form.
func altstoreGenre(genre string) string {
	genre = strings.TrimSpace(genre)
	if genre == "" {
		return ""
	}
	if c, ok := categoryByGenre[genre]; ok {
		return c
	}
	// Game subgenres are normally secondary to a primary genre of "Games", but
	// some metadata puts one in the genre field. Sports and Kids are also
	// top-level categories, so those two are deliberately absent here.
	switch strings.ToLower(genre) {
	case "action", "adventure", "arcade", "board", "card", "casino", "dice",
		"educational", "family", "music games", "puzzle", "racing",
		"role playing", "simulation", "strategy", "trivia", "word":
		return "games"
	}
	return "other"
}

var categoryByGenre = map[string]string{
	"Developer Tools":   "developer",
	"Entertainment":     "entertainment",
	"Games":             "games",
	"Music":             "music",
	"News":              "news",
	"Photo & Video":     "photography",
	"Photography":       "photography",
	"Productivity":      "productivity",
	"Social Networking": "social",
	"Utilities":         "utilities",
	"Weather":           "weather",
}

var categoryByUTI = map[string]string{
	"public.app-category.developer-tools":   "developer",
	"public.app-category.entertainment":     "entertainment",
	"public.app-category.games":             "games",
	"public.app-category.music":             "music",
	"public.app-category.news":              "news",
	"public.app-category.photography":       "photography",
	"public.app-category.productivity":      "productivity",
	"public.app-category.social-networking": "social",
	"public.app-category.utilities":         "utilities",
	"public.app-category.weather":           "weather",
}

func plistString(meta map[string]any, key string) string {
	if s, ok := meta[key].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
