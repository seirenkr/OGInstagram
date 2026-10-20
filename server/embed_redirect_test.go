package main

import (
	"context"
	"net/http"
	"testing"
)

// pageApp sends every request through one DC session answering with rt.
func pageApp(rt roundTripFunc) *App {
	return &App{pool: &SessionPool{sessions: []*Session{{name: "us-1", client: &http.Client{Transport: rt}}}}}
}

func TestEmbedRejectsLoginRedirect(t *testing.T) {
	for _, location := range []string{
		"/accounts/login", "/accounts/login/?next=%2Fp%2FABC123%2F", "/accounts/login/two_factor/",
		"/challenge", "/challenge/123/", "/checkpoint", "/checkpoint/123/",
		"https://www.instagram.com/accounts/login/",
	} {
		t.Run(location, func(t *testing.T) {
			calls := 0
			a := pageApp(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return mediaTestResponse(r, http.StatusFound, http.Header{"Location": {location}}, nil), nil
				}
				return mediaTestResponse(r, http.StatusOK, nil, []byte("login page")), nil
			})
			_, err := a.fetchPostEmbed(context.Background(), "ABC123")
			if err == nil || err.Code != errorCodeLoginRequired || calls != 1 {
				t.Fatalf("login redirect: err=%v calls=%d; want login_required without fetching destination", err, calls)
			}
		})
	}
}

func TestEmbedFollowsBenignRedirects(t *testing.T) {
	for _, location := range []string{
		"/p/ABC123/embed/captioned/?canonical=1",
		"/accounts/login-help/", "/challenge-info/", "/checkpoint-info/",
		"https://example.com/accounts/login/",
	} {
		t.Run(location, func(t *testing.T) {
			calls := 0
			a := pageApp(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return mediaTestResponse(r, http.StatusMovedPermanently, http.Header{"Location": {location}}, nil), nil
				}
				return mediaTestResponse(r, http.StatusOK, nil, []byte(simpleEmbedImagePage)), nil
			})
			if post, err := a.fetchPostEmbed(context.Background(), "ABC123"); err != nil || post.Username == "" || calls != 2 {
				t.Fatalf("benign redirect: err=%v calls=%d post=%+v", err, calls, post)
			}
		})
	}
}

func TestEmbedStopsRedirectLoops(t *testing.T) {
	calls := 0
	a := pageApp(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls > 10 {
			return mediaTestResponse(r, http.StatusOK, nil, []byte(simpleEmbedImagePage)), nil
		}
		return mediaTestResponse(r, http.StatusFound, http.Header{"Location": {"/p/ABC123/embed/"}}, nil), nil
	})
	if _, err := a.fetchPostEmbed(context.Background(), "ABC123"); err == nil || err.Code == errorCodeLoginRequired || calls != 10 {
		t.Fatalf("redirect loop: err=%v requests=%d; want rejection after 10 requests", err, calls)
	}
}
