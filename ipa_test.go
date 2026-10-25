package main

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"howett.net/plist"
)

func testPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.RGBA{R: 40, G: 80, B: 160, A: 255}}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func testIPA(t *testing.T, withITunesArtwork bool) map[string][]byte {
	t.Helper()
	meta := map[string]any{
		"CFBundleDisplayName":       "Example App",
		"CFBundleName":              "Example",
		"CFBundleIdentifier":        "com.example.app",
		"LSApplicationCategoryType": "public.app-category.developer-tools",
		"CFBundleIcons": map[string]any{
			"CFBundlePrimaryIcon": map[string]any{
				"CFBundleIconFiles": []any{"AppIcon60x60"},
			},
		},
	}
	plistBytes, err := plist.Marshal(meta, plist.XMLFormat)
	if err != nil {
		t.Fatalf("marshal plist: %v", err)
	}

	files := map[string][]byte{
		"Payload/Example.app/Info.plist":                   plistBytes,
		"Payload/Example.app/AppIcon60x60@2x.png":          testPNG(t, 120, 120),
		"Payload/Example.app/AppIcon60x60@3x.png":          testPNG(t, 180, 180),
		"Payload/Example.app/Assets.car":                   []byte("compiled assets, not a plist"),
		"Payload/Example.app/PlugIns/Today.app/Info.plist": []byte("not the app"),
	}
	if withITunesArtwork {
		files["iTunesArtwork@2x"] = testPNG(t, 1024, 1024)
	}
	return files
}

func writePNGChunk(w *bytes.Buffer, typ string, data []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(data)))
	w.Write(length[:])
	w.WriteString(typ)
	w.Write(data)
	crc := crc32.NewIEEE()
	crc.Write([]byte(typ))
	crc.Write(data)
	var sum [4]byte
	binary.BigEndian.PutUint32(sum[:], crc.Sum32())
	w.Write(sum[:])
}

// testCgBI builds an Apple-optimized PNG: raw-deflate BGRA scanlines behind a
// private CgBI chunk. It is deliberately not a valid standard PNG, which is
// exactly the case standardPNG exists to fix.
func testCgBI(t *testing.T, width, height int) []byte {
	t.Helper()
	return testCgBIWithMarker(t, width, height, []byte{0x50, 0x00, 0x20, 0x06})
}

func testCgBIWithMarker(t *testing.T, width, height int, marker []byte) []byte {
	t.Helper()
	var raw bytes.Buffer
	for y := 0; y < height; y++ {
		raw.WriteByte(0) // filter: None
		for x := 0; x < width; x++ {
			raw.Write([]byte{50, 100, 200, 255}) // B, G, R, A
		}
	}
	var deflated bytes.Buffer
	zw, err := flate.NewWriter(&deflated, flate.DefaultCompression)
	if err != nil {
		t.Fatalf("flate writer: %v", err)
	}
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatalf("deflate: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close deflate: %v", err)
	}

	var out bytes.Buffer
	out.WriteString("\x89PNG\r\n\x1a\n")
	writePNGChunk(&out, "CgBI", marker)
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], uint32(width))
	binary.BigEndian.PutUint32(ihdr[4:8], uint32(height))
	ihdr[8] = 8 // bit depth
	ihdr[9] = 6 // true colour with alpha
	writePNGChunk(&out, "IHDR", ihdr)
	writePNGChunk(&out, "IDAT", deflated.Bytes())
	writePNGChunk(&out, "IEND", nil)
	return out.Bytes()
}

func testZip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range names {
		method := zip.Deflate
		if name == "Payload/Example.app/Info.plist" {
			method = zip.Store // exercise both member encodings
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method})
		if err != nil {
			t.Fatalf("create zip member %s: %v", name, err)
		}
		if _, err := w.Write(files[name]); err != nil {
			t.Fatalf("write zip member %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func rangeServer(t *testing.T, data []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "app.ipa", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestReadIPAInfoPrefersPrimaryIconOverLargerLegacyArtwork(t *testing.T) {
	files := testIPA(t, true) // primary AppIcon60x60 120/180, iTunesArtwork@2x 1024
	zipData := testZip(t, files)
	srv := rangeServer(t, zipData)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := readIPAInfo(ctx, srv.Client(), srv.URL+"/app.ipa", "")
	if err != nil {
		t.Fatalf("readIPAInfo: %v", err)
	}

	if info.Name != "Example App" {
		t.Errorf("name = %q, want Example App", info.Name)
	}
	if info.BundleID != "com.example.app" {
		t.Errorf("bundle ID = %q, want com.example.app", info.BundleID)
	}
	if info.Category != "developer" {
		t.Errorf("category = %q, want developer", info.Category)
	}
	// The 1024x1024 iTunesArtwork is legacy artwork, not the primary icon.
	if info.IconWidth != 180 || info.IconHeight != 180 {
		t.Errorf("icon = %dx%d, want the primary 180x180 AppIcon", info.IconWidth, info.IconHeight)
	}
	if !bytes.Equal(info.Icon, files["Payload/Example.app/AppIcon60x60@3x.png"]) {
		t.Error("icon bytes are not the primary 180x180 AppIcon")
	}
}

func TestReadIPAInfoIgnoresAlternateIcons(t *testing.T) {
	files := testIPA(t, true)
	raw := files["Payload/Example.app/Info.plist"]
	var meta map[string]any
	if _, err := plist.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("unmarshal fixture plist: %v", err)
	}
	icons := meta["CFBundleIcons"].(map[string]any)
	icons["CFBundlePrimaryIcon"] = map[string]any{
		"CFBundleIconFiles": []any{"AppIconPrimary"},
		"CFBundleIconName":  "AppIconPrimary",
	}
	icons["CFBundleAlternateIcons"] = map[string]any{
		"AppIconAlt": map[string]any{"CFBundleIconFiles": []any{"AppIconAlt"}},
	}
	raw, err := plist.Marshal(meta, plist.XMLFormat)
	if err != nil {
		t.Fatalf("marshal fixture plist: %v", err)
	}
	files["Payload/Example.app/Info.plist"] = raw
	files["Payload/Example.app/AppIconPrimary@2x.png"] = testPNG(t, 120, 120)
	files["Payload/Example.app/AppIconAlt@3x.png"] = testPNG(t, 180, 180)
	srv := rangeServer(t, testZip(t, files))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := readIPAInfo(ctx, srv.Client(), srv.URL+"/app.ipa", "")
	if err != nil {
		t.Fatalf("readIPAInfo: %v", err)
	}
	if info.IconWidth != 120 || info.IconHeight != 120 {
		t.Errorf("icon = %dx%d, want the primary 120x120 icon", info.IconWidth, info.IconHeight)
	}
	if !bytes.Equal(info.Icon, files["Payload/Example.app/AppIconPrimary@2x.png"]) {
		t.Error("icon bytes are not the primary icon")
	}
}

func TestReadIPAInfoLegacyFallback(t *testing.T) {
	files := testIPA(t, true)
	var meta map[string]any
	if _, err := plist.Unmarshal(files["Payload/Example.app/Info.plist"], &meta); err != nil {
		t.Fatalf("unmarshal fixture plist: %v", err)
	}
	delete(meta, "CFBundleIcons")
	raw, err := plist.Marshal(meta, plist.XMLFormat)
	if err != nil {
		t.Fatalf("marshal fixture plist: %v", err)
	}
	files["Payload/Example.app/Info.plist"] = raw
	srv := rangeServer(t, testZip(t, files))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := readIPAInfo(ctx, srv.Client(), srv.URL+"/app.ipa", "")
	if err != nil {
		t.Fatalf("readIPAInfo: %v", err)
	}
	// No primary icon is declared, so the generic search may use the larger
	// iTunesArtwork.
	if info.IconWidth != 1024 || info.IconHeight != 1024 {
		t.Errorf("icon = %dx%d, want the 1024x1024 fallback", info.IconWidth, info.IconHeight)
	}
}

func TestReadIPAInfoWithoutMarketingArtwork(t *testing.T) {
	files := testIPA(t, false)
	srv := rangeServer(t, testZip(t, files))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := readIPAInfo(ctx, srv.Client(), srv.URL+"/app.ipa", "")
	if err != nil {
		t.Fatalf("readIPAInfo: %v", err)
	}
	if info.IconWidth != 180 || info.IconHeight != 180 {
		t.Errorf("icon = %dx%d, want the 180x180 AppIcon", info.IconWidth, info.IconHeight)
	}
	if !bytes.Equal(info.Icon, files["Payload/Example.app/AppIcon60x60@3x.png"]) {
		t.Error("icon bytes are not the 180x180 AppIcon")
	}
}

func TestStandardPNGConvertsCgBI(t *testing.T) {
	raw := testCgBI(t, 120, 120)
	out, err := standardPNG(raw)
	if err != nil {
		t.Fatalf("standardPNG: %v", err)
	}
	if bytes.HasPrefix(out, cgbiHeader) {
		t.Fatal("icon is still CgBI after conversion")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode converted icon: %v", err)
	}
	if format != "png" || cfg.Width != 120 || cfg.Height != 120 {
		t.Fatalf("converted icon = %s %dx%d, want png 120x120", format, cfg.Width, cfg.Height)
	}

	plain := testPNG(t, 10, 10)
	if got, err := standardPNG(plain); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("standard PNG was altered: %v", err)
	}
}

func TestStandardPNGConvertsCgBIVariant(t *testing.T) {
	// Older Xcode versions stamp a different marker into the CgBI chunk. The
	// decoder rejects it unless the marker is normalised first.
	raw := testCgBIWithMarker(t, 120, 120, []byte{0x50, 0x00, 0x20, 0x02})
	out, err := standardPNG(raw)
	if err != nil {
		t.Fatalf("standardPNG: %v", err)
	}
	if bytes.HasPrefix(out, cgbiHeader) {
		t.Fatal("icon is still CgBI after conversion")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode converted icon: %v", err)
	}
	if format != "png" || cfg.Width != 120 || cfg.Height != 120 {
		t.Fatalf("converted icon = %s %dx%d, want png 120x120", format, cfg.Width, cfg.Height)
	}
}

func TestReadIPAInfoConvertsCgBI(t *testing.T) {
	files := testIPA(t, false)
	delete(files, "Payload/Example.app/AppIcon60x60@3x.png")
	// Use the older marker so the whole extraction path covers that variant.
	files["Payload/Example.app/AppIcon60x60@2x.png"] = testCgBIWithMarker(t, 200, 200, []byte{0x50, 0x00, 0x20, 0x02})
	srv := rangeServer(t, testZip(t, files))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := readIPAInfo(ctx, srv.Client(), srv.URL+"/app.ipa", "")
	if err != nil {
		t.Fatalf("readIPAInfo: %v", err)
	}
	if info.IconWidth != 200 || info.IconHeight != 200 {
		t.Errorf("icon = %dx%d, want the 200x200 CgBI icon", info.IconWidth, info.IconHeight)
	}
	if bytes.HasPrefix(info.Icon, cgbiHeader) {
		t.Error("stored icon is still CgBI")
	}
	if _, format, err := image.DecodeConfig(bytes.NewReader(info.Icon)); err != nil || format != "png" {
		t.Errorf("stored icon is not a standard PNG: %v, %s", err, format)
	}
}

func TestReadIPAInfoNeedsRangeSupport(t *testing.T) {
	data := testZip(t, testIPA(t, true))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A host that ignores Range and answers 200 with the whole file.
		w.Header().Set("Content-Type", "application/zip")
		w.Write(data)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := readIPAInfo(ctx, srv.Client(), srv.URL+"/app.ipa", "")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "range") {
		t.Fatalf("error = %v, want a range-support failure", err)
	}
}

func TestReadIPAInfoCategoryFromITunesMetadata(t *testing.T) {
	files := testIPA(t, false)
	var meta map[string]any
	if _, err := plist.Unmarshal(files["Payload/Example.app/Info.plist"], &meta); err != nil {
		t.Fatalf("unmarshal fixture plist: %v", err)
	}
	delete(meta, "LSApplicationCategoryType")
	raw, err := plist.Marshal(meta, plist.XMLFormat)
	if err != nil {
		t.Fatalf("marshal fixture plist: %v", err)
	}
	files["Payload/Example.app/Info.plist"] = raw
	storeMeta, err := plist.Marshal(map[string]any{"genre": "Photo & Video"}, plist.XMLFormat)
	if err != nil {
		t.Fatalf("marshal iTunesMetadata: %v", err)
	}
	files["iTunesMetadata.plist"] = storeMeta
	srv := rangeServer(t, testZip(t, files))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := readIPAInfo(ctx, srv.Client(), srv.URL+"/app.ipa", "")
	if err != nil {
		t.Fatalf("readIPAInfo: %v", err)
	}
	if info.Category != "photography" {
		t.Errorf("category = %q, want photography from iTunesMetadata", info.Category)
	}
}

func TestRemoteZipReadsAcrossBlocks(t *testing.T) {
	files := testIPA(t, true)
	big := make([]byte, remoteZipBlockSize*2+1234)
	if _, err := rand.New(rand.NewSource(42)).Read(big); err != nil {
		t.Fatalf("make random data: %v", err)
	}
	const bigName = "Payload/Example.app/large.bin"
	files[bigName] = big
	srv := rangeServer(t, testZip(t, files))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	z, err := newRemoteZip(ctx, srv.Client(), srv.URL+"/app.ipa", "")
	if err != nil {
		t.Fatalf("newRemoteZip: %v", err)
	}
	zr, err := zip.NewReader(z, z.size)
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	for _, f := range zr.File {
		if f.Name != bigName {
			continue
		}
		got, err := readZipFile(ctx, z, f, int64(len(big))+1024)
		if err != nil {
			t.Fatalf("readZipFile: %v", err)
		}
		if !bytes.Equal(got, big) {
			t.Fatalf("large member came back corrupted (%d bytes vs %d)", len(got), len(big))
		}
		return
	}
	t.Fatalf("%s not found in the test archive", bigName)
}

func TestAltstoreCategory(t *testing.T) {
	cases := map[string]string{
		"":                                      "",
		"public.app-category.developer-tools":   "developer",
		"public.app-category.utilities":         "utilities",
		"public.app-category.action-games":      "games",
		"public.app-category.social-networking": "social",
		"public.app-category.finance":           "other",
		"com.example.unrelated":                 "",
	}
	for in, want := range cases {
		if got := altstoreCategory(in); got != want {
			t.Errorf("altstoreCategory(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAltstoreGenre(t *testing.T) {
	cases := map[string]string{
		"":                "",
		"Developer Tools": "developer",
		"Photo & Video":   "photography",
		"Games":           "games",
		"Role Playing":    "games",
		"Utilities":       "utilities",
		"Finance":         "other",
		"Sports":          "other",
		"Kids":            "other",
	}
	for in, want := range cases {
		if got := altstoreGenre(in); got != want {
			t.Errorf("altstoreGenre(%q) = %q, want %q", in, got, want)
		}
	}
}
