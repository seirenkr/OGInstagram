package main

import (
	"mime"
	"os"
	"path/filepath"
	"strings"
)

type assetFile struct {
	contentType string
	immutable   bool
	body        []byte
}

type Assets struct {
	homeTemplate string
	mainJS       string
	mainCSS      string
	files        map[string]assetFile
}

func contentTypeFor(name string) string {
	ext := filepath.Ext(name)
	switch ext {
	case ".woff2":
		return "font/woff2"
	case ".js", ".css", ".png", ".jpg":
		return mime.TypeByExtension(ext)
	default:
		return ""
	}
}

func loadAssets(dir string) (*Assets, error) {
	home, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		return nil, err
	}
	a := &Assets{homeTemplate: string(home), files: map[string]assetFile{}}

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
		a.files["/"+name] = assetFile{contentType: ct, immutable: hashed, body: body}
		switch {
		case hashed && strings.HasSuffix(name, ".js"):
			a.mainJS = "/" + name
		case hashed && strings.HasSuffix(name, ".css"):
			a.mainCSS = "/" + name
		}
	}
	if f, ok := a.files["/favicon-32.png"]; ok {
		a.files["/favicon.ico"] = f
	}
	return a, nil
}

func (a *Assets) static(path string) (assetFile, bool) {
	f, ok := a.files[path]
	return f, ok
}
