package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func concatUrl(base *url.URL, path string) *url.URL {
	if strings.Contains(path, "http") {
		uri, _ := url.Parse(path)
		return uri
	}
	trimmed := base.Path[:strings.LastIndex(base.Path, "/")]
	// url with scheme and domain
	return &url.URL{
		Scheme: base.Scheme,
		Host:   base.Host,
		Path:   trimmed + "/" + path,
	}
}

func Get(ctx context.Context, uri *url.URL) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "hls_downloader")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return data, nil
}
