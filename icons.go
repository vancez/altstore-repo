package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Icons live beside config.json, one file per bundle ID, so a source that is
// copied or backed up brings its artwork along. Only formats the extractor can
// produce are served, and the filename is the bundle ID so /icon/<bundleId>
// can find it without any state of its own.
var iconExts = []string{".png", ".jpg"}

// Repo-level artwork uses its own fixed slots and directory, separate from the
// bundle-ID-keyed app icons. The API calls the slots "icon" and "header"; the
// URL path uses the file names below.
var sourceImageNames = map[string]string{
	"icon":   "source-icon",
	"header": "source-header",
}

// iconFileName vets a bundle ID for use as a filename. Bundle IDs are dots and
// letters in practice; anything that could climb out of the icon directory is
// refused rather than sanitised, so the URL and the file always agree.
func iconFileName(bundleID string) (string, bool) {
	bundleID = strings.TrimSpace(bundleID)
	if bundleID == "" || len(bundleID) > 128 {
		return "", false
	}
	if bundleID != filepath.Base(bundleID) || bundleID == "." || bundleID == ".." ||
		strings.Contains(bundleID, "..") {
		return "", false
	}
	for _, r := range bundleID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_':
		default:
			return "", false
		}
	}
	return bundleID, true
}

// iconURLReferences reports whether an icon URL points at the /icon route for
// a bundle ID. An app whose bundle ID changed but whose icon URL still points
// at the old file is still using that file, not freeing it.
func iconURLReferences(raw, bundleID string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Path == "/icon/"+bundleID
}

func (s *Store) iconDir() string {
	return filepath.Join(s.dir, "icon")
}

func (s *Store) imageDir() string {
	return filepath.Join(s.dir, "image")
}

// SaveIcon atomically replaces a bundle's icon. Replacing means any file this
// bundle had before goes away, including one with the other extension, so a
// fetch that switches from JPEG to PNG does not leave a stale file behind.
func (s *Store) SaveIcon(bundleID string, data []byte, ext string) error {
	key, ok := iconFileName(bundleID)
	if !ok {
		return fmt.Errorf("bundle ID %q cannot be used as an icon filename", bundleID)
	}
	return saveImageFile(s.iconDir(), key, data, ext)
}

// SaveSourceImage stores the repo icon or header image in its fixed slot and
// returns the file name used by /image/<name>.
func (s *Store) SaveSourceImage(kind string, data []byte, ext string) (string, error) {
	name, ok := sourceImageNames[kind]
	if !ok {
		return "", fmt.Errorf("unknown image slot %q", kind)
	}
	if err := saveImageFile(s.imageDir(), name, data, ext); err != nil {
		return "", err
	}
	return name, nil
}

// saveImageFile is the shared temp-write-and-rename path for app icons and
// source artwork. Replacing a file removes the other supported extension, so a
// PNG replacing a JPEG cannot leave the old file to win the lookup.
func saveImageFile(dir, key string, data []byte, ext string) error {
	ext = strings.ToLower(ext)
	if !validImageExt(ext) {
		return fmt.Errorf("unsupported image type %q", ext)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, key+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}

	for _, other := range iconExts {
		if other != ext {
			os.Remove(filepath.Join(dir, key+other))
		}
	}
	if err := os.Rename(tmpName, filepath.Join(dir, key+ext)); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// IconPath returns the file backing /icon/<bundleId>, if one exists.
func (s *Store) IconPath(bundleID string) (string, bool) {
	key, ok := iconFileName(bundleID)
	if !ok {
		return "", false
	}
	return imagePathIn(s.iconDir(), key)
}

// SourceImagePath returns the file backing /image/<name>, if one exists. Only
// the two known slot names are served.
func (s *Store) SourceImagePath(name string) (string, bool) {
	known := false
	for _, n := range sourceImageNames {
		if n == name {
			known = true
			break
		}
	}
	if !known {
		return "", false
	}
	return imagePathIn(s.imageDir(), name)
}

func imagePathIn(dir, key string) (string, bool) {
	for _, ext := range iconExts {
		p := filepath.Join(dir, key+ext)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p, true
		}
	}
	return "", false
}

func (s *Store) DeleteIcon(bundleID string) error {
	key, ok := iconFileName(bundleID)
	if !ok {
		return nil
	}
	var firstErr error
	for _, ext := range iconExts {
		err := os.Remove(filepath.Join(s.iconDir(), key+ext))
		if err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func validImageExt(ext string) bool {
	for _, e := range iconExts {
		if e == ext {
			return true
		}
	}
	return false
}
