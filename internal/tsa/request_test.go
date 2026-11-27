package tsa_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/tsa"
	"github.com/devrdias/ambermoth/internal/tsa/tsatest"
)

var errBoom = errors.New("boom")

// roundTripFunc is an http.RoundTripper that never touches the network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// failingBody returns some bytes and then an error.
type failingBody struct{ read bool }

func (b *failingBody) Read(p []byte) (int, error) {
	if b.read {
		return 0, errBoom
	}
	b.read = true
	return copy(p, "partial"), nil
}

func (b *failingBody) Close() error { return nil }

func serve(t *testing.T, status int, contentType string, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRequestRejectsReply(t *testing.T) {
	f := newFixture(t)
	other := sha256.Sum256([]byte("another head"))
	otherReq, _, err := tsa.BuildRequest(other[:])
	if err != nil {
		t.Fatal(err)
	}
	otherDigest := f.auth.Reply(t, otherReq)
	reqDER, _, err := tsa.BuildRequest(f.digest)
	if err != nil {
		t.Fatal(err)
	}
	noNonceReq := tsatest.ParseDER(t, reqDER)
	noNonceReq.Kids = append(noNonceReq.Kids[:2], noNonceReq.Kids[3:]...)
	noNonce := f.auth.Reply(t, noNonceReq.DER())

	cases := []struct {
		name   string
		status int
		body   []byte
		want   string
		prefix bool
	}{
		{name: "HTTP 500", status: 500, body: f.resp, want: "returned HTTP 500", prefix: true},
		{name: "HTTP 404", status: 404, body: nil, want: "returned HTTP 404", prefix: true},
		{name: "HTTP 204", status: 204, body: nil, want: "returned HTTP 204", prefix: true},
		{name: "empty body", status: 200, body: nil, want: "not a TimeStampResp: ", prefix: true},
		{name: "not a token", status: 200, body: []byte("<html>busy</html>"), want: "not a TimeStampResp: ", prefix: true},
		{name: "body over the size limit", status: 200, body: append(append([]byte(nil), f.resp...), make([]byte, 2<<20)...), want: "not a TimeStampResp: trailing data"},
		{name: "reply for another digest", status: 200, body: otherDigest, want: "token's message imprint does not match the anchored hash"},
		{name: "reply for another request", status: 200, body: f.resp, want: "TSA reply nonce does not match request"},
		{name: "reply without a nonce", status: 200, body: noNonce, want: "TSA reply nonce does not match request"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := serve(t, c.status, "application/timestamp-reply", c.body)
			body, info, err := tsa.Request(context.Background(), srv.URL, f.digest, srv.Client())
			if err == nil {
				t.Fatalf("accepted: %d bytes, %+v", len(body), info)
			}
			ok := err.Error() == c.want
			if c.prefix {
				ok = strings.Contains(err.Error(), c.want)
			}
			if !ok || body != nil || info != nil {
				t.Fatalf("err = %q, body %d bytes, info %v; want %q", err, len(body), info, c.want)
			}
		})
	}
}

func TestRequestFailsBeforeTheReply(t *testing.T) {
	digest := bytes.Repeat([]byte{1}, 32)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	body := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: &failingBody{}}, nil
	})
	down := roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errBoom })
	honoursContext := roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })
	cases := []struct {
		name   string
		ctx    context.Context
		url    string
		digest []byte
		client *http.Client
		is     error
		want   string
	}{
		{name: "digest of the wrong size", ctx: context.Background(), url: "http://tsa.invalid", digest: digest[:31], want: "digest must be 32 bytes (SHA-256)"},
		{name: "context already cancelled", ctx: cancelled, url: "http://tsa.invalid", digest: digest, client: &http.Client{Transport: honoursContext}, is: context.Canceled},
		{name: "transport error", ctx: context.Background(), url: "http://tsa.invalid", digest: digest, client: &http.Client{Transport: down}, is: errBoom},
		{name: "body read error", ctx: context.Background(), url: "http://tsa.invalid", digest: digest, client: &http.Client{Transport: body}, is: errBoom},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := tsa.Request(c.ctx, c.url, c.digest, c.client)
			if err == nil || (c.is != nil && !errors.Is(err, c.is)) || (c.want != "" && err.Error() != c.want) {
				t.Fatalf("err = %v; want %v %q", err, c.is, c.want)
			}
		})
	}
}

func TestRequestRejectsBadURL(t *testing.T) {
	t.Parallel()
	_, _, err := tsa.Request(context.Background(), "http://[::1", bytes.Repeat([]byte{1}, 32), nil)
	var uerr *url.Error
	if !errors.As(err, &uerr) {
		t.Fatalf("err = %v; want a *url.Error", err)
	}
}

func TestRequestCancelledWhileWaiting(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // the server notices a closed connection only once the body is read
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := tsa.Request(ctx, srv.URL, bytes.Repeat([]byte{1}, 32), srv.Client())
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want context.Canceled", err)
	}
}

// sent is what the TSA endpoint saw.
type sent struct {
	method, contentType string
	body                []byte
}

func TestRequestSendsATimeStampQuery(t *testing.T) {
	f := newFixture(t)
	seen := make(chan sent, 1)
	inner := f.auth.Handler(t, "application/timestamp-reply")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen <- sent{r.Method, r.Header.Get("Content-Type"), b}
		r.Body = io.NopCloser(bytes.NewReader(b))
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	body, info, err := tsa.Request(context.Background(), srv.URL, f.digest, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := <-seen
	if got.method != http.MethodPost || got.contentType != "application/timestamp-query" || len(got.body) == 0 {
		t.Fatalf("request was %s %q with %d bytes", got.method, got.contentType, len(got.body))
	}
	if _, err := tsa.Verify(body, f.digest, f.auth.Roots); err != nil {
		t.Fatalf("returned token does not verify: %v", err)
	}
	if info.ChainVerified {
		t.Fatal("Request does not check the chain; callers verify against roots themselves")
	}
}

func TestRequestAcceptsAnyReplyContentType(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(f.auth.Handler(t, "text/html"))
	t.Cleanup(srv.Close)
	if _, _, err := tsa.Request(context.Background(), srv.URL, f.digest, srv.Client()); err != nil {
		t.Fatalf("rejected a reply labelled text/html: %v", err)
	}
}
