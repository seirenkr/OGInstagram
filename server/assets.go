package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var faviconRE = regexp.MustCompile(`^favicon-(\d+)(-[0-9a-f]+)?\.png$`)

type assetFile struct {
	contentType string
	immutable   bool
	body        []byte
}

type Assets struct {
	homeTemplate string
	mainJS       string
	mainCSS      string
	favicons     map[string]string // size -> "/favicon-<size>-<hash>.png"
	files        map[string]assetFile
}

func (a *Assets) faviconPath(size string) string {
	if p, ok := a.favicons[size]; ok {
		return p
	}
	return "/favicon-" + size + ".png"
}

func contentTypeFor(name string) string {
	switch filepath.Ext(name) {
	case ".js":
		return "application/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".png":
		return "image/png"
	case ".ico":
		return "image/x-icon"
	default:
		return ""
	}
}

func loadAssets(dir string) (*Assets, error) {
	home, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		return nil, err
	}
	a := &Assets{homeTemplate: string(home), favicons: map[string]string{}, files: map[string]assetFile{}}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == "index.html" {
			continue
		}
		ct := contentTypeFor(name)
		if ct == "" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		hashed := strings.HasPrefix(name, "main-")
		if m := faviconRE.FindStringSubmatch(name); m != nil {
			a.favicons[m[1]] = "/" + name
			if m[2] != "" {
				hashed = true
			}
		}
		a.files["/"+name] = assetFile{contentType: ct, immutable: hashed, body: body}
		switch {
		case hashed && strings.HasSuffix(name, ".js"):
			a.mainJS = "/" + name
		case hashed && strings.HasSuffix(name, ".css"):
			a.mainCSS = "/" + name
		}
	}
	// Browsers auto-request /favicon.ico, so it keeps a fixed (revalidated) URL
	// rather than a hashed one.
	if f, ok := a.files[a.faviconPath("32")]; ok {
		a.files["/favicon.ico"] = assetFile{contentType: "image/x-icon", body: f.body}
	}
	return a, nil
}

func (a *Assets) static(path string) (assetFile, bool) {
	f, ok := a.files[path]
	return f, ok
}
