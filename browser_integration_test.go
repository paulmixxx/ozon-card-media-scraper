package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestScrapeWithBrowserExtractsCardAndReviewMedia(t *testing.T) {
	browserPath, err := resolveBrowserPath("")
	if err != nil {
		t.Skipf("browser not available: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/product/demo-product-123/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><body>
			<div class="gallery">
				<img src="/media/card-1.jpg">
				<video src="/media/card-2.mp4"></video>
			</div>
		</body></html>`))
	})
	mux.HandleFunc("/product/demo-product-123/reviews", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><body>
			<article data-review-uuid="review-1">
				<span itemprop="author">Pavel</span>
				<p itemprop="reviewBody">Отлично</p>
				<img src="/media/review-1.jpg">
				<video src="/media/review-1.mp4"></video>
			</article>
		</body></html>`))
	})
	mux.HandleFunc("/media/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".mp4") {
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("fake-video"))
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("fake-image"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	ref, err := parseProductURL(server.URL + "/product/demo-product-123/")
	if err != nil {
		t.Fatalf("parseProductURL: %v", err)
	}

	snapshot, html, warnings, err := scrapeWithBrowser(server.URL+ref.Path, ref, CLIOptions{
		UserAgent:   defaultUserAgent,
		Timeout:     20 * time.Second,
		BrowserWait: 1 * time.Second,
		Headless:    true,
		BrowserPath: browserPath,
	})
	if err != nil {
		t.Fatalf("scrapeWithBrowser returned error: %v", err)
	}
	if len(warnings) > 0 {
		t.Fatalf("expected no warnings, got: %v", warnings)
	}
	if !strings.Contains(html, "gallery") {
		t.Fatalf("expected product html to be returned")
	}
	if len(snapshot.CardImages) == 0 || len(snapshot.CardVideos) == 0 {
		t.Fatalf("expected card image and video, got %+v", snapshot)
	}
	if len(snapshot.Reviews) == 0 {
		t.Fatalf("expected review records, got %+v", snapshot)
	}
	if got := snapshot.Reviews[0].Author; got != "Pavel" {
		t.Fatalf("expected author Pavel, got %q", got)
	}
}
