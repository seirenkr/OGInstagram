package main

import (
	"os"
	"path/filepath"
)

type assetFile struct {
	contentType string
	body        []byte
}

type Assets struct {
	homeTemplate string
	files        map[string]assetFile
}

type staticRoute struct {
	name        string
	contentType string
}

var staticFileRoutes = map[string]staticRoute{
	"/favicon.ico":     {"favicon-32.png", "image/png"},
	"/favicon-16.png":  {"favicon-16.png", "image/png"},
	"/favicon-24.png":  {"favicon-24.png", "image/png"},
	"/favicon-32.png":  {"favicon-32.png", "image/png"},
	"/favicon-48.png":  {"favicon-48.png", "image/png"},
	"/favicon-64.png":  {"favicon-64.png", "image/png"},
	"/favicon-192.png": {"favicon-192.png", "image/png"},
	"/main.js":         {"main.js", "application/javascript; charset=utf-8"},
	"/main.css":        {"main.css", "text/css; charset=utf-8"},
}

func loadAssets(dir string) (*Assets, error) {
	home, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		return nil, err
	}
	a := &Assets{homeTemplate: string(home), files: map[string]assetFile{}}
	for path, route := range staticFileRoutes {
		body, err := os.ReadFile(filepath.Join(dir, route.name))
		if err != nil {
			return nil, err
		}
		a.files[path] = assetFile{contentType: route.contentType, body: body}
	}
	return a, nil
}

func (a *Assets) static(path string) (assetFile, bool) {
	f, ok := a.files[path]
	return f, ok
}
