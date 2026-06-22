package main

import (
	"net/url"
	"strconv"
)

func queryInt(values url.Values, key string) (int, bool) {
	if _, has := values[key]; !has {
		return 0, false
	}
	raw := values.Get(key)
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, true
	}
	return n, true
}

func parseRequestQuery(values url.Values) RequestQuery {
	img, imgSet := queryInt(values, "img_index")
	idx, idxSet := queryInt(values, "index")
	ord, ordSet := queryInt(values, "order")
	return RequestQuery{
		ImgIndex:    img,
		ImgIndexSet: imgSet,
		Index:       idx,
		IndexSet:    idxSet,
		Order:       ord,
		OrderSet:    ordSet,
		PostType:    values.Get("post_type"),
		Shortcode:   values.Get("shortcode"),
		Text:        values.Get("text"),
		Status:      values.Get("status"),
		Provider:    values.Get("provider"),
		Gallery:     values.Get("__gallery") == "1",
	}
}

func mediaIndexFromQuery(q RequestQuery, pathIndex int) int {
	if pathIndex >= 0 {
		v := pathIndex - 1
		if v < 0 {
			return 0
		}
		return v
	}
	if q.ImgIndexSet {
		v := q.ImgIndex - 1
		if v < 0 {
			return 0
		}
		return v
	}
	if q.IndexSet {
		return maxInt(0, q.Index)
	}
	if q.OrderSet {
		return maxInt(0, q.Order)
	}
	return 0
}

func querySpecified(q RequestQuery) bool {
	return q.ImgIndexSet || q.IndexSet || q.OrderSet
}

func fillQueryFromURL(q *RequestQuery, rawURL string) {
	if rawURL == "" || q.Shortcode != "" || q.Status != "" {
		return
	}
	postType, shortcode, mediaIndex, hasIndex, ok := parseEmbedURL(rawURL)
	if !ok {
		return
	}
	q.Shortcode = shortcode
	q.Status = shortcode
	q.PostType = postType
	if hasIndex {
		q.ImgIndex = mediaIndex + 1
		q.ImgIndexSet = true
	}
}

func parseEmbedURL(rawURL string) (string, string, int, bool, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Path == "" {
		if u2, err2 := url.Parse(instagramOrigin + "/" + rawURL); err2 == nil {
			u = u2
		} else {
			return "", "", 0, false, false
		}
	}
	if u.Host == "" && u.Path != "" {

		if u2, err2 := url.Parse(instagramOrigin + u.Path); err2 == nil {
			u2.RawQuery = u.RawQuery
			u = u2
		}
	}
	route := parseEmbedSegments(splitPath(u.Path))
	if route == nil {
		return "", "", 0, false, false
	}
	q := parseRequestQuery(u.Query())
	pathIndex := -1
	if route.HasIndex {
		pathIndex = route.PathIndex
	}
	mediaIndex := mediaIndexFromQuery(q, pathIndex)
	hasIndex := querySpecified(q) || route.HasIndex
	return route.PostType, route.Shortcode, mediaIndex, hasIndex, true
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
