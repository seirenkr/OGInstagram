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
		return max(0, q.Index)
	}
	if q.OrderSet {
		return max(0, q.Order)
	}
	return 0
}

func querySpecified(q RequestQuery) bool {
	return q.ImgIndexSet || q.IndexSet || q.OrderSet
}
