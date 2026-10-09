package paprika

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	maxPhotoBytes = 8 << 20
	// Paprika keeps photos in this S3 bucket and hands out path-style links to it.
	photoStoreHost   = "s3.amazonaws.com"
	photoStorePrefix = "/uploads.paprikaapp.com/"
)

// PhotoLink asks Paprika for a download link to one gallery photo. The link is
// signed and expires within minutes, so it is requested when the image is needed.
func (c *Client) PhotoLink(ctx context.Context, photoUID string) (string, error) {
	path := syncPath + "photo/" + url.PathEscape(photoUID) + "/"
	res, err := c.call(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	var out struct {
		PhotoURL string `json:"photo_url"`
	}
	if err := json.Unmarshal(res, &out); err != nil || out.PhotoURL == "" {
		return "", &Error{Kind: KindProtocol, Op: "GET " + opPath(path), Status: http.StatusOK, Message: "photo has no download link"}
	}
	return out.PhotoURL, nil
}

// Download fetches the image behind a link Paprika issued. It contacts only
// Paprika's API host or its photo bucket, sends no account token, follows no
// redirect, and returns nothing that is not an image. Errors name the host,
// never the link, because the link is a credential while it lasts.
func (c *Client) Download(ctx context.Context, link string) (data []byte, mime string, err error) {
	const op = "GET photo"
	u, perr := url.Parse(link)
	if perr != nil {
		return nil, "", &Error{Kind: KindProtocol, Op: op, Message: "photo link is not a URL"}
	}
	base, _ := url.Parse(c.base)
	switch {
	case u.Host == photoStoreHost && strings.HasPrefix(u.Path, photoStorePrefix):
		u.Scheme = "https" // Paprika issues http links; the signature holds over https
	case u.Host == base.Host && u.Scheme == base.Scheme:
	default:
		return nil, "", &Error{Kind: KindProtocol, Op: op, Message: "photo link points at an unexpected host: " + u.Host}
	}
	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if rerr != nil {
		return nil, "", &Error{Kind: KindProtocol, Op: op, Message: "photo link is not a URL"}
	}
	req.Header.Set("User-Agent", c.userAgent)
	client := *c.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, derr := client.Do(req)
	if derr != nil {
		// The transport error quotes the signed link, so it is not kept.
		return nil, "", &Error{Kind: KindUnavailable, Op: op, Message: transportMessage(derr)}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return nil, "", &Error{Kind: KindUnavailable, Op: op, Status: resp.StatusCode, Message: "photo store answered " + http.StatusText(resp.StatusCode)}
	case resp.StatusCode != http.StatusOK:
		return nil, "", &Error{Kind: KindRejected, Op: op, Status: resp.StatusCode, Message: fmt.Sprintf("photo store refused the download link (HTTP %d)", resp.StatusCode)}
	}
	data, rerr = io.ReadAll(io.LimitReader(resp.Body, maxPhotoBytes+1))
	if rerr != nil {
		return nil, "", &Error{Kind: KindUnavailable, Op: op, Status: resp.StatusCode, Message: "photo could not be read completely"}
	}
	if len(data) > maxPhotoBytes {
		return nil, "", &Error{Kind: KindRejected, Op: op, Status: resp.StatusCode, Message: fmt.Sprintf("photo is larger than %d MiB", maxPhotoBytes>>20)}
	}
	mime = http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		return nil, "", &Error{Kind: KindProtocol, Op: op, Status: resp.StatusCode, Message: "download is not an image"}
	}
	c.log.Debug("photo downloaded", "host", u.Host, "bytes", len(data), "type", mime)
	return data, mime, nil
}
