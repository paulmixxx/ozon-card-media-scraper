package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeReviewMediaMergesImagesAndVideos(t *testing.T) {
	reviews := []BrowserReview{{
		ID:     "review-1",
		Photos: []string{"https://cdn.example/review-1.jpg", "https://cdn.example/review-1.jpg"},
		Videos: []string{"https://cdn.example/review-1.mp4"},
	}}

	normalized := normalizeBrowserReviews(reviews)
	if len(normalized) != 1 {
		t.Fatalf("expected 1 review, got %d", len(normalized))
	}
	if len(normalized[0].Media) != 2 {
		t.Fatalf("expected 2 unique media items, got %d", len(normalized[0].Media))
	}
	if normalized[0].Media[0].Kind != "image" {
		t.Fatalf("expected first media to be image, got %q", normalized[0].Media[0].Kind)
	}
	if normalized[0].Media[1].Kind != "video" {
		t.Fatalf("expected second media to be video, got %q", normalized[0].Media[1].Kind)
	}
}

func TestBuildBrowserCandidatesAddsReviewsSuffix(t *testing.T) {
	candidates := buildBrowserCandidates(ProductRef{Path: "/product/demo-product-123/"})
	if len(candidates) < 2 {
		t.Fatalf("expected at least 2 browser candidates, got %d", len(candidates))
	}
	if candidates[0] != "/product/demo-product-123/" {
		t.Fatalf("unexpected first candidate: %q", candidates[0])
	}
	if candidates[1] != "/product/demo-product-123/reviews" {
		t.Fatalf("unexpected second candidate: %q", candidates[1])
	}
}

func TestBrowserPageLooksUsableAllowsRRIfContentVisible(t *testing.T) {
	snapshot := BrowserSnapshot{CardImages: []string{"https://cdn.ozon.ru/t/test.jpg"}}
	html := `<html><body><h1>Шарф снуд Essentials</h1><div>В корзину</div></body></html>`
	if !browserPageLooksUsable("https://www.ozon.ru/product/demo/?__rr=1&abt_att=1", html, snapshot) {
		t.Fatal("expected visible product page with __rr to be treated as usable")
	}
}

func TestResolveCookieValueReadsFilePathPassedToCookieFlag(t *testing.T) {
	tmpDir := t.TempDir()
	cookieFile := filepath.Join(tmpDir, "cookies.txt")
	if err := os.WriteFile(cookieFile, []byte("session=abc; region=ru"), 0o644); err != nil {
		t.Fatalf("write cookie file: %v", err)
	}
	got, err := resolveCookieValue(CLIOptions{Cookie: cookieFile})
	if err != nil {
		t.Fatalf("resolveCookieValue returned error: %v", err)
	}
	if got != "session=abc; region=ru" {
		t.Fatalf("unexpected cookie value: %q", got)
	}
}
