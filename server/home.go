package main

import (
	"encoding/json"
	"strings"
)

type homeJS struct {
	Successful       string `json:"successful"`
	Failed           string `json:"failed"`
	Avg              string `json:"avg"`
	Ms               string `json:"ms"`
	NoData           string `json:"noData"`
	NoDataYet        string `json:"noDataYet"`
	StatsUnavailable string `json:"statsUnavailable"`
	SkipToContent    string `json:"skipToContent"`
	TimeUTC          string `json:"timeUTC"`
	Language         string `json:"language"`
}

type homeStrings struct {
	Lang         string
	Tagline      string
	SupportCta   string
	DarkMode     string
	LightMode    string
	UsageH2      string
	NormalView   string
	NormalDesc   string
	GalleryView  string
	GalleryDesc  string
	DirectView   string
	DirectDesc   string
	SupportedH2  string
	SupportNote  string
	Posts        string
	UserProfile  string
	Reels        string
	StatusH2     string
	StatusSub    string
	Requests     string
	ResponseTime string
	Disclaimer   string
	JS           homeJS
}

type homeAppData struct {
	Brand        string `json:"brand"`
	Version      string `json:"version"`
	Host         string `json:"host"`
	Lang         string `json:"lang"`
	Tagline      string `json:"tagline"`
	SupportURL   string `json:"supportURL"`
	SupportCta   string `json:"supportCta"`
	GitHubURL    string `json:"githubURL"`
	DarkMode     string `json:"darkMode"`
	LightMode    string `json:"lightMode"`
	UsageH2      string `json:"usageH2"`
	NormalView   string `json:"normalView"`
	NormalDesc   string `json:"normalDesc"`
	GalleryView  string `json:"galleryView"`
	GalleryDesc  string `json:"galleryDesc"`
	DirectView   string `json:"directView"`
	DirectDesc   string `json:"directDesc"`
	SupportedH2  string `json:"supportedH2"`
	SupportNote  string `json:"supportNote"`
	Posts        string `json:"posts"`
	UserProfile  string `json:"userProfile"`
	Reels        string `json:"reels"`
	StatusH2     string `json:"statusH2"`
	StatusSub    string `json:"statusSub"`
	Requests     string `json:"requests"`
	ResponseTime string `json:"responseTime"`
	Disclaimer   string `json:"disclaimer"`
	JS           homeJS `json:"js"`
}

var homeStringsByLocale = map[HomeLocale]homeStrings{
	localeEN: {
		Lang: "en", Tagline: "Instagram embed proxy for Discord, Telegram, and anything that supports Open Graph Protocol or ActivityPub — with rich previews: media, caption, and stats.",
		SupportCta: "Support me on Ko-fi", DarkMode: "Use dark mode", LightMode: "Use light mode", UsageH2: "Usage", NormalView: "Normal View", NormalDesc: "Embeds the creator's profile, caption, stats, and media.",
		GalleryView: "Gallery View", GalleryDesc: "Embeds the creator's profile and media.",
		DirectView: "Direct View", DirectDesc: "Embeds only the direct media URL.", SupportedH2: "Supported URLs", Posts: "Posts", UserProfile: "User Profile", Reels: "Reels",
		SupportNote: "Private posts, age-restricted posts, and posts unavailable in the United States are not supported.",
		StatusH2:    "Status", StatusSub: "Last 24 hours", Requests: "Requests", ResponseTime: "Latency",
		Disclaimer: "Instagram is a trademark of Meta Platforms, Inc. This service is not affiliated with, endorsed, or sponsored by Instagram or Meta.",
		JS:         homeJS{"Successful", "Failed", "Avg", "ms", "No data", "No data yet.", "Stats unavailable.", "Skip to content", "Time (UTC)", "Language"},
	},
	localeJA: {
		Lang: "ja", Tagline: "Discord、Telegram、および Open Graph Protocol または ActivityPub に対応するあらゆるサービス向けの Instagram 埋め込みプロキシです。メディア、キャプション、統計情報を含むリッチプレビューを提供します。",
		SupportCta: "Ko-fi で支援する", DarkMode: "ダークモードを使用", LightMode: "ライトモードを使用", UsageH2: "使い方", NormalView: "通常表示", NormalDesc: "作成者のプロフィール、キャプション、統計、メディアを埋め込みます。",
		GalleryView: "ギャラリー表示", GalleryDesc: "作成者のプロフィールとメディアを埋め込みます。",
		DirectView: "ダイレクト表示", DirectDesc: "メディアの直接 URL のみを埋め込みます。", SupportedH2: "対応 URL", Posts: "投稿", UserProfile: "ユーザープロフィール", Reels: "リール",
		SupportNote: "非公開投稿、年齢制限付きの投稿、および米国で利用できない投稿は対応していません。",
		StatusH2:    "ステータス", StatusSub: "過去24時間", Requests: "リクエスト", ResponseTime: "レイテンシ",
		Disclaimer: "Instagram は Meta Platforms, Inc. の商標です。本サービスは Instagram および Meta と提携・承認・後援関係にありません。",
		JS:         homeJS{"成功", "失敗", "平均", "ms", "データなし", "まだデータがありません。", "統計を利用できません。", "コンテンツへ移動", "時刻 (UTC)", "言語"},
	},
	localeKO: {
		Lang: "ko", Tagline: "Discord, Telegram 및 Open Graph Protocol이나 ActivityPub를 지원하는 모든 서비스에서 사용할 수 있는 Instagram 임베드 프록시입니다. 미디어, 캡션, 통계가 포함된 리치 미리보기를 제공합니다.",
		SupportCta: "Ko-fi에서 후원하기", DarkMode: "다크 모드 사용", LightMode: "라이트 모드 사용", UsageH2: "사용법", NormalView: "일반 보기", NormalDesc: "작성자 프로필, 캡션, 통계, 미디어를 모두 임베드합니다.",
		GalleryView: "갤러리 보기", GalleryDesc: "작성자 프로필과 미디어를 임베드합니다.",
		DirectView: "다이렉트 보기", DirectDesc: "미디어 직접 URL만 임베드합니다.", SupportedH2: "지원 URL", Posts: "게시물", UserProfile: "사용자 프로필", Reels: "릴스",
		SupportNote: "비공개 게시물, 연령 제한 게시물, 미국에서 이용할 수 없는 게시물은 지원되지 않습니다.",
		StatusH2:    "상태", StatusSub: "최근 24시간", Requests: "요청", ResponseTime: "지연 시간",
		Disclaimer: "Instagram은 Meta Platforms, Inc.의 상표입니다. 본 서비스는 Instagram 또는 Meta와 제휴, 보증, 후원 관계가 없습니다.",
		JS:         homeJS{"성공", "실패", "평균", "ms", "데이터 없음", "아직 데이터가 없습니다.", "통계를 사용할 수 없습니다.", "본문으로 건너뛰기", "시간 (UTC)", "언어"},
	},
}

func (a *App) buildHomeHTML(host, acceptLanguage string, forced *HomeLocale) string {
	if host == "" {
		host = "this domain"
	}
	locale := resolveHomeLocale(acceptLanguage)
	if forced != nil {
		locale = *forced
	}
	t := homeStringsByLocale[locale]
	appJSON, _ := json.Marshal(homeAppData{
		Brand: a.cfg.BrandName, Version: a.cfg.Version, Host: host, Lang: t.Lang,
		Tagline: t.Tagline, SupportURL: a.cfg.SupportURL, SupportCta: t.SupportCta, GitHubURL: a.cfg.GitHubURL,
		DarkMode: t.DarkMode, LightMode: t.LightMode, UsageH2: t.UsageH2,
		NormalView: t.NormalView, NormalDesc: t.NormalDesc, GalleryView: t.GalleryView,
		GalleryDesc: t.GalleryDesc, DirectView: t.DirectView, DirectDesc: t.DirectDesc,
		SupportedH2: t.SupportedH2, SupportNote: t.SupportNote, Posts: t.Posts,
		UserProfile: t.UserProfile, Reels: t.Reels, StatusH2: t.StatusH2,
		StatusSub: t.StatusSub, Requests: t.Requests, ResponseTime: t.ResponseTime,
		Disclaimer: t.Disclaimer, JS: t.JS,
	})

	base := a.publicBaseURLFromHost(host)
	canonicalPath := "/"
	if forced != nil {
		canonicalPath = "/" + string(*forced)
	}
	headLinks := strings.Join([]string{
		`<link rel="canonical" href="` + attr(base+canonicalPath) + `">`,
		`<meta name="theme-color" content="` + attr(a.cfg.BrandColor) + `">`,
		`<meta property="og:url" content="` + attr(base+canonicalPath) + `">`,
		`<meta property="og:image" content="` + attr(base+a.assets.faviconPath("192")) + `">`,
		`<meta property="twitter:image" content="` + attr(base+a.assets.faviconPath("192")) + `">`,
		`<link rel="alternate" hreflang="en" href="` + attr(base+"/en") + `">`,
		`<link rel="alternate" hreflang="ja" href="` + attr(base+"/ja") + `">`,
		`<link rel="alternate" hreflang="ko" href="` + attr(base+"/ko") + `">`,
		`<link rel="alternate" hreflang="x-default" href="` + attr(base+"/") + `">`,
	}, "")

	repl := strings.NewReplacer(
		"{{APP_JSON}}", string(appJSON),
		"{{HEAD_LINKS}}", headLinks,
		"{{BRAND}}", htmlEscape(a.cfg.BrandName),
		"{{LANG}}", t.Lang,
		"{{T_TAGLINE}}", htmlEscape(t.Tagline),
		"{{MAIN_JS}}", a.assets.mainJS,
		"{{MAIN_CSS}}", a.assets.mainCSS,
		"{{FAVICON_192}}", a.assets.faviconPath("192"),
		"{{FAVICON_32}}", a.assets.faviconPath("32"),
	)
	return repl.Replace(a.assets.homeTemplate)
}
