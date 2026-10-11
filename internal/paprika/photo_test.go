package paprika_test

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/pavel-alay/paprika-adapter/internal/paprika"
	"github.com/pavel-alay/paprika-adapter/internal/paprikatest"
)

// store answers like the photo bucket and records what the client asked it.
type store struct {
	requests []*http.Request
	reply    func(w http.ResponseWriter, r *http.Request)
}

func (s *store) client(t *testing.T) *paprika.Client {
	t.Helper()
	fake := newFake(t)
	return newClient(t, fake, func(o *paprika.Options) {
		o.HTTPClient = paprikatest.HandlerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.requests = append(s.requests, r)
			s.reply(w, r)
		}))
	})
}

func TestDownloadUsesHTTPSAndSendsNoToken(t *testing.T) {
	s := &store{reply: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(paprikatest.Thumbnail) }}
	c := s.client(t)
	data, mime, err := c.Download(context.Background(), "http://s3.amazonaws.com/uploads.paprikaapp.com/1/a.jpg?Expires=1&Signature=sig")
	if err != nil || mime != "image/jpeg" || !bytes.Equal(data, paprikatest.Thumbnail) {
		t.Fatalf("download: %d bytes, %q, %v", len(data), mime, err)
	}
	r := s.requests[0]
	if r.URL.Scheme != "https" || r.URL.Host != "s3.amazonaws.com" || r.URL.Query().Get("Signature") != "sig" {
		t.Fatalf("requested %s://%s, want the same link over https", r.URL.Scheme, r.URL.Host)
	}
	if r.Header.Get("Authorization") != "" {
		t.Fatal("the account token was sent to the photo store")
	}
}

func TestDownloadOnlyContactsPaprikaHosts(t *testing.T) {
	s := &store{reply: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(paprikatest.Thumbnail) }}
	c := s.client(t)
	for _, link := range []string{
		"https://example.com/a.jpg",
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:8080/mcp",
		"https://s3.amazonaws.com/some-other-bucket/a.jpg",
		"https://s3.amazonaws.com.evil.example/uploads.paprikaapp.com/a.jpg",
		"http://paprika.test/photos/a.jpg", // the API host, but not over its scheme
		"file:///etc/passwd",
		"://",
	} {
		_, _, err := c.Download(context.Background(), link)
		wantKind(t, err, paprika.KindProtocol)
	}
	if len(s.requests) != 0 {
		t.Fatalf("%d request(s) left the adapter for a refused link", len(s.requests))
	}
}

func TestDownloadRefusesWhatIsNotAPhoto(t *testing.T) {
	link := "https://s3.amazonaws.com/uploads.paprikaapp.com/1/a.jpg?Signature=secret-signature"
	for name, tc := range map[string]struct {
		reply func(w http.ResponseWriter, r *http.Request)
		kind  paprika.Kind
	}{
		"redirect": {func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://example.com/elsewhere.jpg", http.StatusFound)
		}, paprika.KindRejected},
		"expired link": {func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "Request has expired", http.StatusForbidden)
		}, paprika.KindRejected},
		"store down": {func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "SlowDown", http.StatusServiceUnavailable) }, paprika.KindUnavailable},
		"not an image": {func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html><body>sign in</body></html>"))
		}, paprika.KindProtocol},
		"too large": {func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(paprikatest.Thumbnail)
			_, _ = w.Write(make([]byte, 9<<20))
		}, paprika.KindRejected},
	} {
		t.Run(name, func(t *testing.T) {
			s := &store{reply: tc.reply}
			data, _, err := s.client(t).Download(context.Background(), link)
			wantKind(t, err, tc.kind)
			if data != nil || len(s.requests) != 1 {
				t.Fatalf("data returned: %v, requests: %d (a redirect must not be followed)", data != nil, len(s.requests))
			}
			if strings.Contains(err.Error(), "secret-signature") {
				t.Fatalf("error quotes the signed link: %v", err)
			}
		})
	}
}

func TestPhotoLinkComesFromThePhotoEndpoint(t *testing.T) {
	fake := newFake(t)
	c := newClient(t, fake)
	link, err := c.PhotoLink(context.Background(), "7A1C9E30-4B2D-4F58-9A6C-0D1E2F3A4B5C")
	if err != nil || !strings.Contains(link, "/photos/7A1C9E30-4B2D-4F58-9A6C-0D1E2F3A4B5C.jpg?") {
		t.Fatalf("link error: %v", err)
	}
	data, _, err := c.Download(context.Background(), link)
	if err != nil || !bytes.Equal(data, paprikatest.FullPhoto) {
		t.Fatalf("download through the issued link: %d bytes, %v", len(data), err)
	}
	_, err = c.PhotoLink(context.Background(), "NOPE")
	wantKind(t, err, paprika.KindNotFound)
}
