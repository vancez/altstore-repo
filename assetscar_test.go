package main

import (
	"testing"

	carfile "github.com/devcxm/carfile-go"
)

func TestPrimaryIconNamesExcludeAlternates(t *testing.T) {
	meta := map[string]any{
		"CFBundleIcons": map[string]any{
			"CFBundlePrimaryIcon": map[string]any{
				"CFBundleIconFiles": []any{"AppIconPrimary60x60"},
				"CFBundleIconName":  "AppIconFamily",
			},
			"CFBundleAlternateIcons": map[string]any{
				"AppIconAlt": map[string]any{"CFBundleIconFiles": []any{"AppIconAlt"}},
			},
		},
		"CFBundleIcons~ipad": map[string]any{
			"CFBundlePrimaryIcon": map[string]any{
				"CFBundleIconFiles": []any{"AppIconPrimary76x76"},
			},
		},
		"CFBundleIconFiles": []any{"LegacyIcon"},
	}

	names := primaryIconNames(meta)
	want := map[string]bool{
		"AppIconPrimary60x60": true,
		"AppIconFamily":       true,
		"AppIconPrimary76x76": true,
	}
	if len(names) != len(want) {
		t.Fatalf("primaryIconNames = %v, want %v", names, want)
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected primary name %q", n)
		}
	}

	legacy := legacyIconBases(meta)
	if len(legacy) != 1 || legacy[0] != "LegacyIcon" {
		t.Errorf("legacyIconBases = %v, want [LegacyIcon]", legacy)
	}
}

func TestRenditionMatchingAndAppearance(t *testing.T) {
	defaultRendition := carfile.Rendition{
		AssetName: "AppIconFamily",
		CSI:       carfile.CSI{Name: "Icon-1024.png"},
	}
	if !renditionMatchesIconNames(defaultRendition, []string{"AppIconFamily"}) {
		t.Error("primary asset name did not match")
	}
	if renditionIsDark(defaultRendition) {
		t.Error("rendition without an appearance key was treated as dark")
	}

	darkRendition := carfile.Rendition{
		AssetName: "AppIconFamily",
		Key: []carfile.AttributeValue{
			{Name: "Theme Appearance", Value: 1},
		},
		CSI: carfile.CSI{Name: "Icon-1024-dark.png"},
	}
	if !renditionMatchesIconNames(darkRendition, []string{"AppIconFamily"}) {
		t.Error("dark primary asset name did not match")
	}
	if !renditionIsDark(darkRendition) {
		t.Error("Theme Appearance=1 was not treated as dark")
	}
	if renditionMatchesIconNames(darkRendition, []string{"SomeOtherIcon"}) {
		t.Error("unrelated icon name matched")
	}
}

func TestPrimaryLooseName(t *testing.T) {
	names := []string{"AppIconOpaline60x60", "AppIconOpaline76x76", "AppIconOpaline"}
	for _, name := range []string{
		"AppIconOpaline60x60@2x.png",
		"AppIconOpaline60x60@3x.png",
		"AppIconOpaline76x76@2x~ipad.png",
	} {
		if !primaryLooseName(name, names) {
			t.Errorf("%s was not treated as a primary icon", name)
		}
	}
	for _, name := range []string{
		"AppIconDark@3x.png",
		"AppIconClearLight@2x.png",
		"Assets.car",
		"Info.plist",
	} {
		if primaryLooseName(name, names) {
			t.Errorf("%s was treated as a primary icon", name)
		}
	}
}
