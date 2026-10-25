package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestServer(t *testing.T, password string) (*Store, *httptest.Server) {
	t.Helper()
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	srv := httptest.NewServer(NewServer(st, NewReleases(), password))
	t.Cleanup(srv.Close)
	return st, srv
}

func TestStoreIconLifecycle(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if err := st.SaveIcon("com.example.app", []byte("old png"), ".png"); err != nil {
		t.Fatalf("SaveIcon png: %v", err)
	}
	pngPath, ok := st.IconPath("com.example.app")
	if !ok || filepath.Ext(pngPath) != ".png" {
		t.Fatalf("IconPath after png = %q, %v", pngPath, ok)
	}

	// Switching format must not leave the old file behind: /icon has no
	// extension, so a stale .png would win over the fresh .jpg.
	if err := st.SaveIcon("com.example.app", []byte("new jpg"), ".jpg"); err != nil {
		t.Fatalf("SaveIcon jpg: %v", err)
	}
	if _, err := os.Stat(pngPath); !os.IsNotExist(err) {
		t.Errorf("old .png still exists: %v", err)
	}
	jpgPath, ok := st.IconPath("com.example.app")
	if !ok || filepath.Ext(jpgPath) != ".jpg" {
		t.Fatalf("IconPath after jpg = %q, %v", jpgPath, ok)
	}
	if data, err := os.ReadFile(jpgPath); err != nil || string(data) != "new jpg" {
		t.Fatalf("icon contents = %q, %v", data, err)
	}

	if err := st.DeleteIcon("com.example.app"); err != nil {
		t.Fatalf("DeleteIcon: %v", err)
	}
	if _, ok := st.IconPath("com.example.app"); ok {
		t.Error("icon still resolves after DeleteIcon")
	}
}

func TestIconFileName(t *testing.T) {
	good := []string{"com.example.app", "com.example.app-1_2", "A.B.C"}
	for _, id := range good {
		if _, ok := iconFileName(id); !ok {
			t.Errorf("iconFileName(%q) rejected a usable ID", id)
		}
	}
	bad := []string{"", ".", "..", "../evil", "com/example", `com\example`, "com example", "...", strings.Repeat("a", 129)}
	for _, id := range bad {
		if _, ok := iconFileName(id); ok {
			t.Errorf("iconFileName(%q) accepted an unsafe ID", id)
		}
	}

	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := st.SaveIcon("../evil", []byte("x"), ".png"); err == nil {
		t.Error("SaveIcon accepted a traversal bundle ID")
	}
}

func TestSourceImageStore(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := st.SaveSourceImage("nope", []byte("x"), ".png"); err == nil {
		t.Error("SaveSourceImage accepted an unknown slot")
	}

	if _, err := st.SaveSourceImage("icon", []byte("old png"), ".png"); err != nil {
		t.Fatalf("SaveSourceImage png: %v", err)
	}
	pngPath, ok := st.SourceImagePath("source-icon")
	if !ok || filepath.Ext(pngPath) != ".png" {
		t.Fatalf("SourceImagePath = %q, %v", pngPath, ok)
	}
	if _, ok := st.SourceImagePath("source-header"); ok {
		t.Error("header slot exists before it was saved")
	}

	if _, err := st.SaveSourceImage("icon", []byte("new jpg"), ".jpg"); err != nil {
		t.Fatalf("SaveSourceImage jpg: %v", err)
	}
	if _, err := os.Stat(pngPath); !os.IsNotExist(err) {
		t.Errorf("old source png survived replacement: %v", err)
	}
	if p, ok := st.SourceImagePath("source-icon"); !ok || filepath.Ext(p) != ".jpg" {
		t.Errorf("source icon did not switch to jpg: %q, %v", p, ok)
	}
	if _, err := st.SaveSourceImage("header", []byte("header"), ".png"); err != nil {
		t.Fatalf("SaveSourceImage header: %v", err)
	}
	if _, ok := st.SourceImagePath("source-header"); !ok {
		t.Error("header slot missing after save")
	}
	if _, ok := st.SourceImagePath("unknown"); ok {
		t.Error("unknown source image name resolved")
	}
}

func TestRepoImageEndpoint(t *testing.T) {
	st, srv := newTestServer(t, "")
	icon := testPNG(t, 40, 20)
	header := testPNG(t, 80, 30)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		if r.URL.Path == "/header.png" {
			w.Write(header)
			return
		}
		w.Write(icon)
	}))
	t.Cleanup(source.Close)

	for _, tc := range []struct {
		kind string
		path string
		want string
		data []byte
	}{
		{"icon", "/icon.png", "/image/source-icon", icon},
		{"header", "/header.png", "/image/source-header", header},
	} {
		body, _ := json.Marshal(map[string]any{"url": source.URL + tc.path, "kind": tc.kind})
		resp, err := srv.Client().Post(srv.URL+"/api/repo/image", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("%s: POST: %v", tc.kind, err)
		}
		var out struct {
			URL string `json:"url"`
		}
		err = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("%s: decode: %v", tc.kind, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", tc.kind, resp.StatusCode)
		}
		if !strings.HasPrefix(out.URL, srv.URL+tc.want) {
			t.Errorf("%s: url = %q, want prefix %q", tc.kind, out.URL, srv.URL+tc.want)
		}

		// The returned URL is public and serves exactly the downloaded bytes.
		got, err := srv.Client().Get(out.URL)
		if err != nil {
			t.Fatalf("%s: GET local image: %v", tc.kind, err)
		}
		data, _ := io.ReadAll(got.Body)
		got.Body.Close()
		if got.StatusCode != http.StatusOK || !bytes.Equal(data, tc.data) {
			t.Errorf("%s: local image status %d, bytes match %v", tc.kind, got.StatusCode, bytes.Equal(data, tc.data))
		}
	}

	if p, ok := st.SourceImagePath("source-icon"); !ok || filepath.Ext(p) != ".png" {
		t.Errorf("source icon file missing after endpoint: %q, %v", p, ok)
	}
}

func postRepoImageUpload(t *testing.T, srv *httptest.Server, kind string, file []byte, filename string) (*http.Response, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if kind != "" {
		if err := mw.WriteField("kind", kind); err != nil {
			t.Fatalf("write kind: %v", err)
		}
	}
	if file != nil {
		fw, err := mw.CreateFormFile("file", filename)
		if err != nil {
			t.Fatalf("create file part: %v", err)
		}
		if _, err := fw.Write(file); err != nil {
			t.Fatalf("write file part: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/repo/image/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(data)
}

func TestRepoImageUploadEndpoint(t *testing.T) {
	st, srv := newTestServer(t, "")
	img := testPNG(t, 40, 20)

	resp, raw := postRepoImageUpload(t, srv, "icon", img, "icon.png")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, raw)
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.HasPrefix(out.URL, srv.URL+"/image/source-icon") {
		t.Errorf("url = %q, want local source-icon URL", out.URL)
	}
	got, err := srv.Client().Get(out.URL)
	if err != nil {
		t.Fatalf("GET uploaded image: %v", err)
	}
	served, _ := io.ReadAll(got.Body)
	got.Body.Close()
	if got.StatusCode != http.StatusOK || !bytes.Equal(served, img) {
		t.Errorf("served upload status %d, bytes match %v", got.StatusCode, bytes.Equal(served, img))
	}
	if p, ok := st.SourceImagePath("source-icon"); !ok || filepath.Ext(p) != ".png" {
		t.Errorf("uploaded source icon not stored: %q, %v", p, ok)
	}
}

func TestRepoImageUploadRejects(t *testing.T) {
	_, srv := newTestServer(t, "")
	for name, tc := range map[string]struct {
		kind string
		file []byte
	}{
		"missing file": {kind: "icon"},
		"wrong kind":   {kind: "nope", file: testPNG(t, 8, 8)},
		"not an image": {kind: "icon", file: []byte("not an image")},
	} {
		resp, raw := postRepoImageUpload(t, srv, tc.kind, tc.file, "upload.bin")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, body %s", name, resp.StatusCode, raw)
		}
	}
}

func TestRepoImageEndpointRejects(t *testing.T) {
	_, srv := newTestServer(t, "")
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html>not an image</html>")
	}))
	t.Cleanup(html.Close)

	for _, body := range []map[string]any{
		{"url": "", "kind": "icon"},
		{"url": "file:///etc/passwd", "kind": "icon"},
		{"url": "http://example.com/icon.png", "kind": "nope"},
		{"url": html.URL, "kind": "icon"},
	} {
		payload, _ := json.Marshal(body)
		resp, err := srv.Client().Post(srv.URL+"/api/repo/image", "application/json", bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %v: status = %d, want 400", body, resp.StatusCode)
		}
	}
}

func TestImageRouteIsPublic(t *testing.T) {
	st, srv := newTestServer(t, "secret")
	img := testPNG(t, 24, 24)
	if _, err := st.SaveSourceImage("icon", img, ".png"); err != nil {
		t.Fatalf("SaveSourceImage: %v", err)
	}

	resp, err := srv.Client().Get(srv.URL + "/image/source-icon")
	if err != nil {
		t.Fatalf("GET source image: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, img) {
		t.Error("served bytes differ from the saved image")
	}

	missing, err := srv.Client().Get(srv.URL + "/image/source-header")
	if err != nil {
		t.Fatalf("GET missing image: %v", err)
	}
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("missing source image status = %d, want 404", missing.StatusCode)
	}
}

func TestIconRouteIsPublic(t *testing.T) {
	st, srv := newTestServer(t, "secret")
	icon := testPNG(t, 32, 32)
	if err := st.SaveIcon("com.example.app", icon, ".png"); err != nil {
		t.Fatalf("SaveIcon: %v", err)
	}

	// No Authorization header: the device fetching a source cannot send one.
	resp, err := srv.Client().Get(srv.URL + "/icon/com.example.app")
	if err != nil {
		t.Fatalf("GET icon: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Error("no ETag; repeated fetches would always re-download")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read icon: %v", err)
	}
	if !bytes.Equal(body, icon) {
		t.Error("served bytes differ from the stored icon")
	}

	// Replacing the icon behind the same URL must invalidate the old ETag, or
	// a Fetch info refresh would keep showing the previous artwork.
	fresh := testPNG(t, 48, 48)
	if err := st.SaveIcon("com.example.app", fresh, ".png"); err != nil {
		t.Fatalf("replace icon: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/icon/com.example.app", nil)
	req.Header.Set("If-None-Match", etag)
	replaced, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET replaced icon: %v", err)
	}
	defer replaced.Body.Close()
	if replaced.StatusCode != http.StatusOK {
		t.Fatalf("replaced icon status = %d, want 200 (stale ETag)", replaced.StatusCode)
	}
	if replaced.Header.Get("ETag") == etag {
		t.Error("ETag did not change after the icon was replaced")
	}
	replacedBody, _ := io.ReadAll(replaced.Body)
	if !bytes.Equal(replacedBody, fresh) {
		t.Error("served bytes are not the replacement icon")
	}

	missing, err := srv.Client().Get(srv.URL + "/icon/com.example.missing")
	if err != nil {
		t.Fatalf("GET missing icon: %v", err)
	}
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("missing icon status = %d, want 404", missing.StatusCode)
	}
}

func createApp(t *testing.T, srv *httptest.Server, bundleID string) AppConfig {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"name":             "Example",
		"repo":             "owner/repo",
		"bundleIdentifier": bundleID,
		"enabled":          true,
	})
	resp, err := srv.Client().Post(srv.URL+"/api/apps", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("create app status = %d: %s", resp.StatusCode, msg)
	}
	var created AppConfig
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode created app: %v", err)
	}
	return created
}

func deleteApp(t *testing.T, srv *httptest.Server, id string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/apps/"+id, nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("delete app: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("delete app status = %d: %s", resp.StatusCode, msg)
	}
}

func TestDeleteAppRemovesIconWhenUnused(t *testing.T) {
	st, srv := newTestServer(t, "")
	first := createApp(t, srv, "com.example.shared")
	second := createApp(t, srv, "com.example.shared")
	if err := st.SaveIcon(first.BundleID, testPNG(t, 32, 32), ".png"); err != nil {
		t.Fatalf("SaveIcon: %v", err)
	}

	// A second app shares the bundle ID; deleting the first must leave its
	// icon alone.
	deleteApp(t, srv, first.ID)
	if _, ok := st.IconPath(first.BundleID); !ok {
		t.Error("icon was deleted while another app still uses the bundle ID")
	}

	deleteApp(t, srv, second.ID)
	if _, ok := st.IconPath(second.BundleID); ok {
		t.Error("icon survived deleting the last app using its bundle ID")
	}
}

func updateApp(t *testing.T, srv *httptest.Server, id, bundleID, iconURL string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"name":             "Example",
		"repo":             "owner/repo",
		"bundleIdentifier": bundleID,
		"iconURL":          iconURL,
		"enabled":          true,
	})
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/apps/"+id, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("update app: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("update app status = %d: %s", resp.StatusCode, msg)
	}
}

func TestDeleteAppKeepsIconReferencedByGroup(t *testing.T) {
	st, srv := newTestServer(t, "")
	app := createApp(t, srv, "com.example.app")
	if err := st.SaveIcon(app.BundleID, testPNG(t, 32, 32), ".png"); err != nil {
		t.Fatalf("SaveIcon: %v", err)
	}

	// A group created from this app copies its icon URL; deleting the app must
	// not remove artwork the group feed still points at.
	body, _ := json.Marshal(map[string]any{
		"slug": "test", "name": "Test", "iconURL": "/icon/com.example.app",
	})
	resp, err := srv.Client().Post(srv.URL+"/api/groups", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("create group status = %d: %s", resp.StatusCode, msg)
	}

	deleteApp(t, srv, app.ID)
	if _, ok := st.IconPath(app.BundleID); !ok {
		t.Error("icon was deleted while a group still referenced its URL")
	}
}

func TestUpdateAppBundleChangeRemovesOldIcon(t *testing.T) {
	st, srv := newTestServer(t, "")
	app := createApp(t, srv, "com.example.old")
	if err := st.SaveIcon(app.BundleID, testPNG(t, 32, 32), ".png"); err != nil {
		t.Fatalf("SaveIcon: %v", err)
	}

	updateApp(t, srv, app.ID, "com.example.new", "")
	if _, ok := st.IconPath("com.example.old"); ok {
		t.Error("old bundle ID's icon survived the update")
	}
}

func TestUpdateAppKeepsIconTheURLStillUses(t *testing.T) {
	st, srv := newTestServer(t, "")
	app := createApp(t, srv, "com.example.old")
	if err := st.SaveIcon(app.BundleID, testPNG(t, 32, 32), ".png"); err != nil {
		t.Fatalf("SaveIcon: %v", err)
	}

	// A bundle ID edit that leaves the icon URL alone must not delete the file
	// the URL points at.
	updateApp(t, srv, app.ID, "com.example.new", "/icon/com.example.old")
	if _, ok := st.IconPath("com.example.old"); !ok {
		t.Error("icon was deleted while the app's icon URL still pointed at it")
	}
}
