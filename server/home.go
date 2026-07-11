package main

import (
	"embed"
	"encoding/json"
	"html"
	"strings"
)

type homeJS struct {
	Successful       string `json:"successful"`
	Failed           string `json:"failed"`
	Restricted       string `json:"restricted"`
	Avg              string `json:"avg"`
	Ms               string `json:"ms"`
	NoDataYet        string `json:"noDataYet"`
	StatsUnavailable string `json:"statsUnavailable"`
	SkipToContent    string `json:"skipToContent"`
	TimeUTC          string `json:"timeUTC"`
	Language         string `json:"language"`
}

type homeStrings struct {
	Lang         string   `json:"lang"`
	Tagline      string   `json:"tagline"`
	SupportCta   string   `json:"supportCta"`
	DarkMode     string   `json:"darkMode"`
	LightMode    string   `json:"lightMode"`
	UsageH2      string   `json:"usageH2"`
	NormalView   string   `json:"normalView"`
	NormalDesc   string   `json:"normalDesc"`
	GalleryView  string   `json:"galleryView"`
	GalleryDesc  string   `json:"galleryDesc"`
	DirectView   string   `json:"directView"`
	DirectDesc   string   `json:"directDesc"`
	SupportedH2  string   `json:"supportedH2"`
	SupportNote  string   `json:"supportNote"`
	Posts        string   `json:"posts"`
	UserProfile  string   `json:"userProfile"`
	Reels        string   `json:"reels"`
	StatusH2     string   `json:"statusH2"`
	StatusSub    string   `json:"statusSub"`
	Requests     string   `json:"requests"`
	ResponseTime string   `json:"responseTime"`
	Disclaimer   string   `json:"disclaimer"`
	Hero         homeHero `json:"hero"`
	JS           homeJS   `json:"js"`
}

type homeHero struct {
	Line1            string `json:"line1"`
	Line2            string `json:"line2"`
	Rich             string `json:"rich"`
	Channel          string `json:"channel"`
	Placeholder      string `json:"placeholder"`
	Invalid          string `json:"invalid"`
	FetchError       string `json:"fetchError"`
	RateLimited      string `json:"rateLimited"`
	Submit           string `json:"submit"`
	DemoDesc         string `json:"demoDesc"`
	You              string `json:"you"`
	VideoUnavailable string `json:"videoUnavailable"`
}

type homeAppData struct {
	Brand            string `json:"brand"`
	Version          string `json:"version"`
	Host             string `json:"host"`
	SupportURL       string `json:"supportURL"`
	GitHubURL        string `json:"githubURL"`
	TurnstileSiteKey string `json:"turnstileSiteKey"`
	homeStrings
}

//go:embed locales/*.json
var localeFS embed.FS

// homeStringsByLocale loads once at startup from the Crowdin-managed files in
// locales/; a missing or malformed file fails the boot loudly.
var homeStringsByLocale = func() map[HomeLocale]homeStrings {
	m := make(map[HomeLocale]homeStrings, len(homeLocales))
	for _, l := range homeLocales {
		raw, err := localeFS.ReadFile("locales/" + string(l) + ".json")
		if err != nil {
			panic(err)
		}
		var s homeStrings
		if err := json.Unmarshal(raw, &s); err != nil {
			panic("locale " + string(l) + ": " + err.Error())
		}
		m[l] = s
	}
	return m
}()

func (a *App) buildHomeHTML(host, acceptLanguage, hl string) string {
	if host == "" {
		host = "this domain"
	}
	locale := resolveHomeLocale(acceptLanguage)
	forced, ok := asHomeLocale(strings.ToLower(hl))
	if ok {
		locale = forced
	}
	t := homeStringsByLocale[locale]
	appJSON, _ := json.Marshal(homeAppData{
		Brand: a.cfg.BrandName, Version: a.cfg.Version, Host: host,
		SupportURL: a.cfg.SupportURL, GitHubURL: a.cfg.GitHubURL, TurnstileSiteKey: a.cfg.TurnstileSiteKey,
		homeStrings: t,
	})

	base := a.publicBaseURLFromHost(host)
	canonical := base + "/"
	if ok {
		canonical = base + "/?hl=" + string(locale)
	}
	links := []string{
		`<link rel="canonical" href="` + html.EscapeString(canonical) + `">`,
		`<meta name="theme-color" content="` + html.EscapeString(a.cfg.BrandColor) + `">`,
		`<meta property="og:url" content="` + html.EscapeString(canonical) + `">`,
		`<meta property="og:image" content="` + html.EscapeString(base+"/favicon-192.png") + `">`,
		`<meta property="twitter:image" content="` + html.EscapeString(base+"/favicon-192.png") + `">`,
	}
	for _, l := range homeLocales {
		links = append(links, `<link rel="alternate" hreflang="`+homeStringsByLocale[l].Lang+`" href="`+html.EscapeString(base+"/?hl="+string(l))+`">`)
	}
	links = append(links, `<link rel="alternate" hreflang="x-default" href="`+html.EscapeString(base+"/")+`">`)
	headLinks := strings.Join(links, "")

	repl := strings.NewReplacer(
		"{{APP_JSON}}", string(appJSON),
		"{{HEAD_LINKS}}", headLinks,
		"{{BRAND}}", html.EscapeString(a.cfg.BrandName),
		"{{LANG}}", t.Lang,
		"{{T_TAGLINE}}", html.EscapeString(t.Tagline),
		"{{MAIN_JS}}", a.assets.mainJS,
		"{{MAIN_CSS}}", a.assets.mainCSS,
	)
	return repl.Replace(a.assets.homeTemplate)
}
