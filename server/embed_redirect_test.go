package main

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestDirectEmbedRejectsLoginRedirect(t *testing.T) {
	for _, location := range []string{
		"/accounts/login", "/accounts/login/?next=%2Fp%2FABC123%2F", "/accounts/login/two_factor/",
		"/challenge", "/challenge/123/", "/checkpoint", "/checkpoint/123/",
		"https://www.instagram.com/accounts/login/",
	} {
		t.Run(location, func(t *testing.T) {
			calls := 0
			stubDefaultTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return mediaTestResponse(r, http.StatusFound, http.Header{"Location": {location}}, nil), nil
				}
				return mediaTestResponse(r, http.StatusOK, nil, []byte("login page")), nil
			}))
			body, err := (&App{}).directGet(context.Background(), "post", instagramOrigin+"/p/ABC123/embed/captioned/")
			if err == nil || err.Code != errorCodeLoginRequired || body != "" || calls != 1 {
				t.Fatalf("login redirect: body=%q err=%v calls=%d; want login_required without fetching destination", body, err, calls)
			}
		})
	}
}

func TestDirectEmbedPreservesBenignRedirects(t *testing.T) {
	for _, location := range []string{
		"/p/ABC123/embed/captioned/?canonical=1",
		"/accounts/login-help/", "/challenge-info/", "/checkpoint-info/",
		"https://example.com/accounts/login/",
	} {
		t.Run(location, func(t *testing.T) {
			calls := 0
			stubDefaultTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return mediaTestResponse(r, http.StatusMovedPermanently, http.Header{"Location": {location}}, nil), nil
				}
				return mediaTestResponse(r, http.StatusOK, nil, []byte(simpleEmbedImagePage)), nil
			}))
			body, err := (&App{}).directGet(context.Background(), "post", instagramOrigin+"/p/ABC123/embed/captioned/")
			if err != nil || body != simpleEmbedImagePage || calls != 2 {
				t.Fatalf("benign redirect: err=%v calls=%d body matches=%t", err, calls, body == simpleEmbedImagePage)
			}
		})
	}
}

func TestDirectEmbedPreservesRedirectPolicy(t *testing.T) {
	previous := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = previous })
	policyError := errors.New("redirect rejected by existing policy")
	calls, policyCalls := 0, 0
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { policyCalls++; return policyError },
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return mediaTestResponse(r, http.StatusFound, http.Header{"Location": {"/p/ABC123/embed/"}}, nil), nil
		}),
	}
	http.DefaultClient = client
	_, err := (&App{}).directGet(context.Background(), "post", instagramOrigin+"/p/ABC123/embed/captioned/")
	if err == nil || !errors.Is(err.Cause, policyError) || calls != 1 || policyCalls != 1 {
		t.Fatalf("custom redirect policy: err=%v requests=%d policy calls=%d", err, calls, policyCalls)
	}
	if got := client.CheckRedirect(nil, nil); got != policyError || policyCalls != 2 {
		t.Fatal("shared client's redirect policy was changed")
	}
}

func TestDirectEmbedPreservesRedirectLimit(t *testing.T) {
	calls := 0
	stubDefaultTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls > 10 {
			return mediaTestResponse(r, http.StatusOK, nil, []byte(simpleEmbedImagePage)), nil
		}
		return mediaTestResponse(r, http.StatusFound, http.Header{"Location": {"/p/ABC123/embed/"}}, nil), nil
	}))
	_, err := (&App{}).directGet(context.Background(), "post", instagramOrigin+"/p/ABC123/embed/captioned/")
	if err == nil || err.Code == errorCodeLoginRequired || calls != 10 {
		t.Fatalf("redirect loop: err=%v requests=%d; want rejection after 10 requests", err, calls)
	}
}

func TestEmbedPauseLastsThreeHoursAndRequiresValidProbe(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls, responseStatus, responseBody := 0, status, ""
				stubDefaultTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					return mediaTestResponse(r, responseStatus, http.Header{"Location": {"/accounts/login/"}}, []byte(responseBody)), nil
				}))
				a := &App{}
				ctx := context.Background()
				if _, err := a.fetchPostEmbed(ctx, "DaD8phTyclR"); err == nil {
					t.Fatal("upstream rejection was accepted")
				}
				if got := time.Until(time.Unix(0, a.embedPausedUntil.Load())); got != 3*time.Hour {
					t.Fatalf("pause = %s, want exactly 3 hours", got)
				}
				time.Sleep(3*time.Hour - time.Nanosecond)
				if _, err := a.fetchPostEmbed(ctx, "DaD8phTyclR"); err == nil || calls != 1 {
					t.Fatalf("embed retried before 3 hours: err=%v requests=%d", err, calls)
				}
				time.Sleep(time.Nanosecond)
				responseStatus, responseBody = http.StatusOK, "<html>not an embed</html>"
				if _, err := a.fetchPostEmbed(ctx, "DaD8phTyclR"); err == nil || calls != 2 {
					t.Fatalf("invalid probe: err=%v requests=%d", err, calls)
				}
				if got := time.Until(time.Unix(0, a.embedPausedUntil.Load())); got != 3*time.Hour {
					t.Fatalf("invalid HTTP 200 probe cleared the pause: remaining=%s", got)
				}
				if _, err := a.fetchPostEmbed(ctx, "DaD8phTyclR"); err == nil || calls != 2 {
					t.Fatal("invalid probe let the next request reach Instagram")
				}
				time.Sleep(3 * time.Hour)
				responseBody = simpleEmbedImagePage
				if _, err := a.fetchPostEmbed(ctx, "DaD8phTyclR"); err != nil || calls != 3 {
					t.Fatalf("valid probe: err=%v requests=%d", err, calls)
				}
				if a.embedPausedUntil.Load() != 0 {
					t.Fatal("valid probe did not clear the pause")
				}
			})
		})
	}
}

func TestLateEmbedSuccessDoesNotClearNewPause(t *testing.T) {
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan *AppError, 1)
	stubDefaultTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/p/SLOW/embed/captioned/" {
			close(started)
			<-release
			return mediaTestResponse(r, http.StatusOK, nil, []byte(simpleEmbedImagePage)), nil
		}
		return mediaTestResponse(r, http.StatusTooManyRequests, nil, nil), nil
	}))
	a := &App{}
	go func() {
		_, err := a.fetchPostEmbed(context.Background(), "SLOW")
		done <- err
	}()
	<-started
	_, rejected := a.fetchPostEmbed(context.Background(), "LIMITED")
	pausedUntil := a.embedPausedUntil.Load()
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("earlier request failed: %v", err)
	}
	if rejected == nil || pausedUntil == 0 || a.embedPausedUntil.Load() != pausedUntil {
		t.Fatal("earlier successful request cleared a newer rejection's pause")
	}
}

func TestPausedEmbedStartsGraphQLImmediately(t *testing.T) {
	previous := externalHelperPostImpl
	externalHelperPostImpl = nil
	t.Cleanup(func() { externalHelperPostImpl = previous })
	var directCalls, graphqlCalls atomic.Int32
	stubDefaultTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		directCalls.Add(1)
		return mediaTestResponse(r, http.StatusInternalServerError, nil, nil), nil
	}))
	synctest.Test(t, func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			graphqlCalls.Add(1)
			return mediaTestResponse(r, http.StatusOK, nil, []byte(`{"data":{"xdt_api__v1__media__shortcode__web_info":{"items":[{"code":"ABC123","user":{"username":"instagram"},"display_url":"https://scontent.cdninstagram.com/image.jpg","media_type":1}]}}}`)), nil
		})}
		pool := &SessionPool{sessions: []*Session{{client: client}}, budgetLeaseExpires: time.Now().Add(time.Hour), budgetLeaseRemaining: 1 << 20}
		a := newApp(Config{}, pool, offloadSigner{})
		a.embedPausedUntil.Store(time.Now().Add(3 * time.Hour).UnixNano())
		started := time.Now()
		post, err := a.getPost(context.Background(), "ABC123", nil)
		if err != nil || post.Username != "instagram" || time.Since(started) != 0 || directCalls.Load() != 0 || graphqlCalls.Load() != 1 {
			t.Fatalf("paused fetch: err=%v duration=%s direct=%d GraphQL=%d", err, time.Since(started), directCalls.Load(), graphqlCalls.Load())
		}
	})
}
