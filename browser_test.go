package main

import "testing"

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
