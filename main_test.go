package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseProductURL(t *testing.T) {
	ref, err := parseProductURL("https://www.ozon.ru/product/sharf-snud-essentials-696919507/?at=abc")
	if err != nil {
		t.Fatalf("parseProductURL returned error: %v", err)
	}

	if ref.Slug != "sharf-snud-essentials" {
		t.Fatalf("unexpected slug: %q", ref.Slug)
	}
	if ref.ProductID != "696919507" {
		t.Fatalf("unexpected product id: %q", ref.ProductID)
	}
	if ref.Path != "/product/sharf-snud-essentials-696919507/" {
		t.Fatalf("unexpected path: %q", ref.Path)
	}
}

func TestPrepareOutputDirAddsSuffixWhenDirectoryExists(t *testing.T) {
	base := t.TempDir()
	first, err := prepareOutputDir(base, "sample-slug", "20260706")
	if err != nil {
		t.Fatalf("prepareOutputDir first call error: %v", err)
	}
	if filepath.Base(first) != "sample-slug" {
		t.Fatalf("unexpected first dir: %s", first)
	}

	second, err := prepareOutputDir(base, "sample-slug", "20260706")
	if err != nil {
		t.Fatalf("prepareOutputDir second call error: %v", err)
	}
	if filepath.Base(second) != "sample-slug_20260706" {
		t.Fatalf("unexpected second dir: %s", second)
	}
}

func TestRunDownloadsCardAndReviewMedia(t *testing.T) {
	mux := http.NewServeMux()
	var baseURL string

	mux.HandleFunc("/product/test-product-123/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		html := `<!doctype html>
<html><head>
<script type="application/ld+json">{"image":["__SERVER_URL__/media/card-1.jpg","__SERVER_URL__/media/card-2.png"]}</script>
<script>window.__INITIAL_STATE__={"gallery":{"images":[{"url":"__SERVER_URL__/media/card-3.webp"}],"videos":[{"url":"__SERVER_URL__/media/card-video.mp4"}]}};</script>
</head><body>ok</body></html>`
		_, _ = w.Write([]byte(strings.ReplaceAll(html, serverURLPlaceholder, baseURL)))
	})

	mux.HandleFunc("/api/composer-api.bx/widget/json/v2", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode widget payload: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(payload.URL, "page=1") {
			response := `{
				"state": {
					"itemId": 123,
					"products": {"123": {"name": "Test Product"}},
					"reviews": [
						{
							"id": "review-1",
							"createdAt": 1710000000,
							"author": {"firstName": "Alice"},
							"content": {
								"score": 5,
								"comment": "Great",
								"photos": [{"url": "__SERVER_URL__/media/review-1-photo.jpg"}],
								"videos": [{"url": "__SERVER_URL__/media/review-1-video.mp4"}]
							}
						}
					]
				}
			}`
			_, _ = w.Write([]byte(strings.ReplaceAll(response, serverURLPlaceholder, baseURL)))
			return
		}
		_, _ = w.Write([]byte(`{"state":{"itemId":123,"products":{"123":{"name":"Test Product"}},"reviews":null}}`))
	})

	mux.HandleFunc("/media/", func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Ext(r.URL.Path) {
		case ".mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("video-bytes"))
		default:
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("image-bytes"))
		}
	})

	server := httptest.NewServer(mux)
	defer server.Close()
	baseURL = server.URL

	workdir := t.TempDir()
	if err := run([]string{
		server.URL + "/product/test-product-123/",
		"--output", workdir,
		"--reviews-pages", "2",
		"--date", "20260706",
	}); err != nil {
		t.Fatalf("run returned error: %v", err)
	}

	root := filepath.Join(workdir, "test-product")
	assertFileExists(t, filepath.Join(root, "card", "001.jpg"))
	assertFileExists(t, filepath.Join(root, "card", "002.png"))
	assertFileExists(t, filepath.Join(root, "card", "003.webp"))
	assertFileExists(t, filepath.Join(root, "card", "004.mp4"))
	assertFileExists(t, filepath.Join(root, "review", "review-1", "001.jpg"))
	assertFileExists(t, filepath.Join(root, "review", "review-1", "002.mp4"))
	assertFileExists(t, filepath.Join(root, "product.json"))
	assertFileExists(t, filepath.Join(root, "reviews.json"))
}

func TestRunReportsAntiBotBlock(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/product/test-product-123/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body>Инцидент: fab_12345</body></html>`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	err := run([]string{server.URL + "/product/test-product-123/", "--output", t.TempDir(), "--date", "20260706"})
	if err == nil {
		t.Fatal("expected anti-bot error, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "anti-bot") {
		t.Fatalf("expected anti-bot error, got: %v", err)
	}
}

func assertFileExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file %s to exist: %v", path, err)
	}
}

const serverURLPlaceholder = "__SERVER_URL__"
