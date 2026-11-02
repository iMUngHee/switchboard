package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoticesCoverEverythingLinked: the notices name Go and every module
// switchboard links, each with its license text, and not switchboard itself.
func TestNoticesCoverEverythingLinked(t *testing.T) {
	var b bytes.Buffer
	if err := write(&b, "github.com/iMUngHee/switchboard/cmd/switchboard"); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"Go (runtime and standard library) go",
		"golang.org/x/sys v",
		"github.com/pelletier/go-toml/v2 v",
		"Redistribution and use in source and binary forms", // BSD: Go and x/sys
		"Permission is hereby granted, free of charge",      // MIT: go-toml
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the notices lack %q", want)
		}
	}
	if strings.Contains(out, "github.com/iMUngHee/switchboard ") {
		t.Error("the notices list switchboard itself")
	}
}

// TestNoticesFailWithoutALicense: a component with no license file stops
// the release rather than shipping without its notice.
func TestNoticesFailWithoutALicense(t *testing.T) {
	dir := t.TempDir()
	if _, err := license(filepath.Join(dir, "a", "b")); err == nil {
		t.Fatal("found a license in an empty directory")
	}
	os.MkdirAll(filepath.Join(dir, "a"), 0o700)
	os.WriteFile(filepath.Join(dir, "a", "COPYING"), []byte("terms"), 0o600)
	if b, err := license(filepath.Join(dir, "a", "b")); err != nil || string(b) != "terms" {
		t.Fatalf("the license one directory up: %q %v", b, err)
	}
	empty := t.TempDir()
	var out bytes.Buffer
	comps := []component{{"example.com/licensed", "v1", filepath.Join(dir, "a")}, {"example.com/bare", "v1", filepath.Join(empty, "x", "y")}}
	if err := render(&out, comps); err == nil || !strings.Contains(err.Error(), "example.com/bare") {
		t.Fatalf("a component without a license was let through: %v", err)
	}
}
