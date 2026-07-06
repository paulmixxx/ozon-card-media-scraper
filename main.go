package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const defaultUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36"

type ProductRef struct {
	Slug      string
	ProductID string
	Path      string
}

type MediaItem struct {
	URL  string `json:"url"`
	Kind string `json:"kind"`
}

type ReviewRecord struct {
	ID        string      `json:"id"`
	Author    string      `json:"author,omitempty"`
	CreatedAt int64       `json:"createdAt,omitempty"`
	Score     int         `json:"score,omitempty"`
	Comment   string      `json:"comment,omitempty"`
	Media     []MediaItem `json:"media,omitempty"`
}

type ProductMetadata struct {
	SourceURL   string      `json:"sourceUrl"`
	Slug        string      `json:"slug"`
	ProductID   string      `json:"productId"`
	FetchedAt   string      `json:"fetchedAt"`
	CardMedia   []MediaItem `json:"cardMedia"`
	Warnings    []string    `json:"warnings,omitempty"`
	OutputDir   string      `json:"outputDir"`
	ProductPath string      `json:"productPath"`
}

type ReviewsMetadata struct {
	SourceURL string         `json:"sourceUrl"`
	FetchedAt string         `json:"fetchedAt"`
	Reviews   []ReviewRecord `json:"reviews"`
	Warnings  []string       `json:"warnings,omitempty"`
}

type CLIOptions struct {
	OutputDir    string
	Cookie       string
	CookieFile   string
	Proxy        string
	UserAgent    string
	Timeout      time.Duration
	ReviewsPages int
	DateOverride string
	WriteHTML    bool
}

type HTTPResult struct {
	StatusCode int
	Body       []byte
	Header     http.Header
}

type antiBotError struct {
	message string
}

func (e *antiBotError) Error() string {
	return e.message
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	opts, sourceURL, err := parseFlags(args)
	if err != nil {
		return err
	}

	ref, err := parseProductURL(sourceURL)
	if err != nil {
		return err
	}

	stamp := opts.DateOverride
	if stamp == "" {
		stamp = time.Now().Format("20060102")
	}

	rootDir, err := prepareOutputDir(opts.OutputDir, ref.Slug, stamp)
	if err != nil {
		return err
	}
	cardDir := filepath.Join(rootDir, "card")
	reviewDir := filepath.Join(rootDir, "review")
	for _, dir := range []string{cardDir, reviewDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	client, err := newHTTPClient(opts)
	if err != nil {
		return err
	}

	pageResult, err := doRequest(client, http.MethodGet, sourceURL, nil, buildHeaders(opts, sourceURL, true))
	if err != nil {
		return err
	}
	if isAntiBotRedirect(pageResult) || isAntiBotBlocked(pageResult.StatusCode, pageResult.Body) {
		writeErrorJSON(rootDir, sourceURL, pageResult.Body)
		return &antiBotError{message: "Ozon anti-bot blocked the product page request. Try running from a residential/mobile IP, with valid browser cookies, or through a trusted proxy."}
	}

	html := string(pageResult.Body)
	if opts.WriteHTML {
		_ = os.WriteFile(filepath.Join(rootDir, "product.html"), pageResult.Body, 0o644)
	}

	cardMedia, warnings := extractCardMedia(sourceURL, ref, html)
	cardMedia = dedupeMedia(cardMedia)
	if len(cardMedia) == 0 {
		warnings = append(warnings, "Не удалось извлечь медиа карточки из HTML/JSON страницы.")
	}

	for i, media := range cardMedia {
		name := fmt.Sprintf("%03d%s", i+1, detectExt(media.URL, media.Kind))
		if err := downloadToFile(client, media.URL, filepath.Join(cardDir, name), buildHeaders(opts, sourceURL, false)); err != nil {
			warnings = append(warnings, fmt.Sprintf("card download failed for %s: %v", media.URL, err))
		}
	}

	reviews, reviewWarnings := fetchReviews(client, opts, sourceURL, ref)
	warnings = append(warnings, reviewWarnings...)

	for _, review := range reviews {
		reviewMediaDir := filepath.Join(reviewDir, sanitizeName(review.ID))
		if err := os.MkdirAll(reviewMediaDir, 0o755); err != nil {
			warnings = append(warnings, fmt.Sprintf("review dir create failed for %s: %v", review.ID, err))
			continue
		}
		for i, media := range review.Media {
			name := fmt.Sprintf("%03d%s", i+1, detectExt(media.URL, media.Kind))
			if err := downloadToFile(client, media.URL, filepath.Join(reviewMediaDir, name), buildHeaders(opts, sourceURL, false)); err != nil {
				warnings = append(warnings, fmt.Sprintf("review download failed for %s: %v", media.URL, err))
			}
		}
	}

	productMeta := ProductMetadata{
		SourceURL:   sourceURL,
		Slug:        ref.Slug,
		ProductID:   ref.ProductID,
		FetchedAt:   time.Now().Format(time.RFC3339),
		CardMedia:   cardMedia,
		Warnings:    warnings,
		OutputDir:   rootDir,
		ProductPath: ref.Path,
	}
	if err := writeJSON(filepath.Join(rootDir, "product.json"), productMeta); err != nil {
		return err
	}

	reviewsMeta := ReviewsMetadata{
		SourceURL: sourceURL,
		FetchedAt: time.Now().Format(time.RFC3339),
		Reviews:   reviews,
		Warnings:  warnings,
	}
	if err := writeJSON(filepath.Join(rootDir, "reviews.json"), reviewsMeta); err != nil {
		return err
	}

	fmt.Fprintf(os.Stdout, "Saved %d card media and %d reviews into %s\n", len(cardMedia), len(reviews), rootDir)
	if len(warnings) > 0 {
		fmt.Fprintf(os.Stdout, "Warnings:\n- %s\n", strings.Join(warnings, "\n- "))
	}
	return nil
}

func flagExpectsValue(name string) bool {
	switch name {
	case "--write-html":
		return false
	default:
		return true
	}
}

func parseFlags(args []string) (CLIOptions, string, error) {
	fs := flag.NewFlagSet("ozon-card-media-scraper", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	opts := CLIOptions{}
	fs.StringVar(&opts.OutputDir, "output", ".", "base output directory")
	fs.StringVar(&opts.Cookie, "cookie", "", "raw Cookie header value")
	fs.StringVar(&opts.CookieFile, "cookie-file", "", "path to a file containing Cookie header value")
	fs.StringVar(&opts.Proxy, "proxy", "", "HTTP(S) proxy URL")
	fs.StringVar(&opts.UserAgent, "user-agent", defaultUserAgent, "HTTP user-agent")
	fs.IntVar(&opts.ReviewsPages, "reviews-pages", 20, "maximum number of review pages to request")
	fs.StringVar(&opts.DateOverride, "date", "", "date suffix override in yyyymmdd format (testing/debug)")
	fs.BoolVar(&opts.WriteHTML, "write-html", false, "save fetched product HTML for debugging")
	var timeoutSec int
	fs.IntVar(&timeoutSec, "timeout", 30, "request timeout in seconds")

	var sourceURL string
	flagArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			flagArgs = append(flagArgs, arg)
			if !strings.Contains(arg, "=") && flagExpectsValue(arg) && i+1 < len(args) {
				i++
				flagArgs = append(flagArgs, args[i])
			}
			continue
		}
		if sourceURL == "" {
			sourceURL = arg
			continue
		}
		flagArgs = append(flagArgs, arg)
	}

	if err := fs.Parse(flagArgs); err != nil {
		return opts, "", err
	}
	if sourceURL == "" {
		if fs.NArg() == 1 {
			sourceURL = fs.Arg(0)
		} else {
			return opts, "", errors.New("usage: ozon-card-media-scraper [flags] <ozon-product-url>")
		}
	}
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	opts.Timeout = time.Duration(timeoutSec) * time.Second
	if opts.ReviewsPages <= 0 {
		opts.ReviewsPages = 1
	}
	return opts, sourceURL, nil
}

func parseProductURL(raw string) (ProductRef, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return ProductRef{}, fmt.Errorf("parse url: %w", err)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "product" {
		return ProductRef{}, fmt.Errorf("unsupported Ozon product URL: %s", raw)
	}
	last := strings.TrimSuffix(parts[1], "/")
	re := regexp.MustCompile(`^(.*)-(\d+)$`)
	match := re.FindStringSubmatch(last)
	if len(match) != 3 {
		return ProductRef{}, fmt.Errorf("cannot extract slug/product id from %s", raw)
	}
	return ProductRef{
		Slug:      sanitizeName(match[1]),
		ProductID: match[2],
		Path:      "/product/" + match[1] + "-" + match[2] + "/",
	}, nil
}

func prepareOutputDir(baseDir, slug, date string) (string, error) {
	first := filepath.Join(baseDir, slug)
	if _, err := os.Stat(first); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(first, 0o755); err != nil {
			return "", err
		}
		return first, nil
	}
	second := filepath.Join(baseDir, slug+"_"+date)
	if err := os.MkdirAll(second, 0o755); err != nil {
		return "", err
	}
	return second, nil
}

func newHTTPClient(opts CLIOptions) (*http.Client, error) {
	transport := &http.Transport{}
	if opts.Proxy != "" {
		proxyURL, err := url.Parse(opts.Proxy)
		if err != nil {
			return nil, fmt.Errorf("parse proxy: %w", err)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{
		Timeout:   opts.Timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if strings.Contains(req.URL.RawQuery, "__rr=1") || strings.Contains(req.URL.Path, "block.html") {
				return http.ErrUseLastResponse
			}
			if len(via) >= 6 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}, nil
}

func buildHeaders(opts CLIOptions, referer string, html bool) http.Header {
	h := http.Header{}
	h.Set("User-Agent", opts.UserAgent)
	h.Set("Accept-Language", "ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7")
	if html {
		h.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	} else {
		h.Set("Accept", "*/*")
	}
	if referer != "" {
		h.Set("Referer", referer)
	}
	cookie := strings.TrimSpace(opts.Cookie)
	if cookie == "" && opts.CookieFile != "" {
		if b, err := os.ReadFile(opts.CookieFile); err == nil {
			cookie = strings.TrimSpace(string(b))
		}
	}
	if cookie != "" {
		h.Set("Cookie", cookie)
	}
	return h
}

func doRequest(client *http.Client, method, rawURL string, body []byte, headers http.Header) (HTTPResult, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, reader)
	if err != nil {
		return HTTPResult{}, err
	}
	for k, values := range headers {
		for _, v := range values {
			req.Header.Add(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return HTTPResult{}, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return HTTPResult{}, err
	}
	return HTTPResult{StatusCode: resp.StatusCode, Body: payload, Header: resp.Header.Clone()}, nil
}

func isAntiBotRedirect(res HTTPResult) bool {
	location := strings.ToLower(res.Header.Get("Location"))
	return strings.Contains(location, "__rr=1") || strings.Contains(location, "block.html")
}

func isAntiBotBlocked(statusCode int, body []byte) bool {
	text := strings.ToLower(string(body))
	if statusCode == http.StatusForbidden || statusCode == http.StatusTooManyRequests {
		return strings.Contains(text, "incident") || strings.Contains(text, "инцидент") || strings.Contains(text, "supporturl") || strings.Contains(text, "anti-bot") || strings.Contains(text, "vpn") || strings.Contains(text, "похоже, нет соединения")
	}
	return false
}

func extractCardMedia(sourceURL string, ref ProductRef, html string) ([]MediaItem, []string) {
	var media []MediaItem
	var warnings []string

	for _, img := range extractLDJSONImages(html) {
		media = append(media, MediaItem{URL: absolutizeURL(sourceURL, img), Kind: classifyMediaKind(img)})
	}

	for _, blob := range collectJSONBlobs(html) {
		parsed := map[string]any{}
		if err := json.Unmarshal([]byte(blob), &parsed); err != nil {
			continue
		}
		media = append(media, collectMediaFromAny(parsed, sourceURL)...)
	}

	media = append(media, collectMediaFromRegex(html, sourceURL)...)
	media = dedupeMedia(media)

	filtered := media[:0]
	for _, item := range media {
		if strings.Contains(item.URL, ref.ProductID) || isKnownMediaHost(item.URL) {
			filtered = append(filtered, item)
		}
	}
	if len(filtered) == 0 {
		filtered = media
	}
	if len(filtered) == 0 {
		warnings = append(warnings, "card media not found in page HTML or embedded JSON")
	}
	return filtered, warnings
}

func fetchReviews(client *http.Client, opts CLIOptions, sourceURL string, ref ProductRef) ([]ReviewRecord, []string) {
	base, _ := url.Parse(sourceURL)
	base.RawQuery = ""
	base.Fragment = ""
	endpoint := base.Scheme + "://" + base.Host + "/api/composer-api.bx/widget/json/v2"
	candidates := buildReviewURLCandidates(ref.Path)
	warnings := []string{}
	seen := map[string]bool{}
	var reviews []ReviewRecord

	for page := 1; page <= opts.ReviewsPages; page++ {
		var pageFetched bool
		var pageHadReviews bool
		for _, candidate := range candidates {
			widgetURL := fmt.Sprintf(candidate, page)
			payload := map[string]any{
				"asyncData": []string{"tileGridDesktop", "webListReviews", "webReviewGallery"},
				"url":       widgetURL,
			}
			body, _ := json.Marshal(payload)
			headers := buildHeaders(opts, sourceURL, false)
			headers.Set("Content-Type", "application/json")
			res, err := doRequest(client, http.MethodPost, endpoint, body, headers)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("review request failed for %s: %v", widgetURL, err))
				continue
			}
			if isAntiBotBlocked(res.StatusCode, res.Body) {
				warnings = append(warnings, "Ozon anti-bot blocked review API requests.")
				return reviews, warnings
			}
			if res.StatusCode >= 400 {
				warnings = append(warnings, fmt.Sprintf("review API returned status %d for %s", res.StatusCode, widgetURL))
				continue
			}
			parsed, ok := parseReviewResponse(res.Body, sourceURL)
			if !ok {
				continue
			}
			pageFetched = true
			if len(parsed) == 0 {
				continue
			}
			pageHadReviews = true
			for _, review := range parsed {
				if review.ID == "" || seen[review.ID] {
					continue
				}
				seen[review.ID] = true
				reviews = append(reviews, review)
			}
			break
		}
		if !pageFetched || !pageHadReviews {
			break
		}
	}

	sort.Slice(reviews, func(i, j int) bool { return reviews[i].ID < reviews[j].ID })
	return reviews, warnings
}

func buildReviewURLCandidates(productPath string) []string {
	clean := strings.TrimSuffix(productPath, "/")
	return []string{
		clean + "/?page=%d",
		clean + "/reviews/?page=%d",
		strings.Replace(clean, "/product/", "/reviews/", 1) + "/?page=%d",
	}
}

func parseReviewResponse(body []byte, sourceURL string) ([]ReviewRecord, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false
	}
	state, ok := payload["state"].(map[string]any)
	if !ok {
		return nil, false
	}
	reviewsRaw, exists := state["reviews"]
	if !exists || reviewsRaw == nil {
		return nil, true
	}
	list, ok := reviewsRaw.([]any)
	if !ok {
		return nil, false
	}
	result := make([]ReviewRecord, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		review := ReviewRecord{
			ID:        asString(m["id"]),
			CreatedAt: asInt64(m["createdAt"]),
		}
		if author, ok := m["author"].(map[string]any); ok {
			review.Author = asString(author["firstName"])
		}
		if content, ok := m["content"].(map[string]any); ok {
			review.Score = asInt(content["score"])
			review.Comment = firstNonEmpty(asString(content["comment"]), asString(content["text"]))
			review.Media = collectReviewMedia(content, sourceURL)
		}
		result = append(result, review)
	}
	return result, true
}

func collectReviewMedia(content map[string]any, sourceURL string) []MediaItem {
	var media []MediaItem
	for _, key := range []string{"photos", "images"} {
		if list, ok := content[key].([]any); ok {
			for _, item := range list {
				if m, ok := item.(map[string]any); ok {
					if raw := firstNonEmpty(asString(m["url"]), asString(m["src"])); raw != "" {
						media = append(media, MediaItem{URL: absolutizeURL(sourceURL, raw), Kind: "image"})
					}
				}
			}
		}
	}
	for _, key := range []string{"videos", "video"} {
		switch typed := content[key].(type) {
		case []any:
			for _, item := range typed {
				if m, ok := item.(map[string]any); ok {
					if raw := firstNonEmpty(asString(m["url"]), asString(m["src"]), asString(m["videoUrl"])); raw != "" {
						media = append(media, MediaItem{URL: absolutizeURL(sourceURL, raw), Kind: "video"})
					}
				}
			}
		case map[string]any:
			if raw := firstNonEmpty(asString(typed["url"]), asString(typed["src"]), asString(typed["videoUrl"])); raw != "" {
				media = append(media, MediaItem{URL: absolutizeURL(sourceURL, raw), Kind: "video"})
			}
		}
	}
	return dedupeMedia(media)
}

func collectJSONBlobs(html string) []string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?s)<script[^>]*id="__NEXT_DATA__"[^>]*>(.*?)</script>`),
		regexp.MustCompile(`(?s)<script[^>]*type="application/json"[^>]*>(.*?)</script>`),
		regexp.MustCompile(`(?s)window\.__INITIAL_STATE__\s*=\s*(\{.*?\})\s*;</script>`),
		regexp.MustCompile(`(?s)window\.__INITIAL_STATE__\s*=\s*(\{.*?\})\s*;`),
		regexp.MustCompile(`(?s)window\.__PRELOADED_STATE__\s*=\s*(\{.*?\})\s*;`),
	}
	var blobs []string
	for _, re := range patterns {
		for _, match := range re.FindAllStringSubmatch(html, -1) {
			if len(match) > 1 {
				blobs = append(blobs, htmlUnescape(match[1]))
			}
		}
	}
	return blobs
}

func extractLDJSONImages(html string) []string {
	re := regexp.MustCompile(`(?s)<script[^>]*type="application/ld\+json"[^>]*>(.*?)</script>`)
	var urls []string
	for _, match := range re.FindAllStringSubmatch(html, -1) {
		if len(match) < 2 {
			continue
		}
		var payload any
		if err := json.Unmarshal([]byte(htmlUnescape(match[1])), &payload); err != nil {
			continue
		}
		urls = append(urls, collectImageStrings(payload)...)
	}
	return urls
}

func collectImageStrings(node any) []string {
	switch typed := node.(type) {
	case map[string]any:
		var out []string
		for k, v := range typed {
			if strings.EqualFold(k, "image") {
				out = append(out, flattenStrings(v)...)
			}
			out = append(out, collectImageStrings(v)...)
		}
		return out
	case []any:
		var out []string
		for _, v := range typed {
			out = append(out, collectImageStrings(v)...)
		}
		return out
	default:
		return nil
	}
}

func flattenStrings(v any) []string {
	switch typed := v.(type) {
	case string:
		return []string{typed}
	case []any:
		var out []string
		for _, item := range typed {
			out = append(out, flattenStrings(item)...)
		}
		return out
	case map[string]any:
		var out []string
		for _, item := range typed {
			out = append(out, flattenStrings(item)...)
		}
		return out
	default:
		return nil
	}
}

func collectMediaFromAny(node any, sourceURL string) []MediaItem {
	var out []MediaItem
	switch typed := node.(type) {
	case map[string]any:
		for k, v := range typed {
			key := strings.ToLower(k)
			switch value := v.(type) {
			case string:
				if looksLikeMediaURL(value) {
					out = append(out, MediaItem{URL: absolutizeURL(sourceURL, value), Kind: classifyMediaKind(key+":"+value)})
				}
			case []any, map[string]any:
				out = append(out, collectMediaFromAny(value, sourceURL)...)
			}
		}
	case []any:
		for _, item := range typed {
			out = append(out, collectMediaFromAny(item, sourceURL)...)
		}
	}
	return out
}

func collectMediaFromRegex(html string, sourceURL string) []MediaItem {
	re := regexp.MustCompile(`https?:\\/\\/[^"'<>\\s]+|https?://[^"'<>\\s]+`)
	matches := re.FindAllString(html, -1)
	var out []MediaItem
	for _, raw := range matches {
		candidate := strings.ReplaceAll(raw, `\/`, `/`)
		candidate = strings.TrimRight(candidate, ",")
		if looksLikeMediaURL(candidate) {
			out = append(out, MediaItem{URL: candidate, Kind: classifyMediaKind(candidate)})
		}
	}
	return out
}

func looksLikeMediaURL(raw string) bool {
	lower := strings.ToLower(raw)
	for _, ext := range []string{".jpg", ".jpeg", ".png", ".webp", ".gif", ".mp4", ".mov", ".webm", ".m3u8"} {
		if strings.Contains(lower, ext) {
			return true
		}
	}
	return strings.Contains(lower, "/video/") || strings.Contains(lower, "/images/") || strings.Contains(lower, "cdn")
}

func classifyMediaKind(raw string) string {
	lower := strings.ToLower(raw)
	for _, ext := range []string{".mp4", ".mov", ".webm", ".m3u8"} {
		if strings.Contains(lower, ext) {
			return "video"
		}
	}
	if strings.Contains(lower, "video") {
		return "video"
	}
	return "image"
}

func dedupeMedia(items []MediaItem) []MediaItem {
	seen := map[string]bool{}
	out := make([]MediaItem, 0, len(items))
	for _, item := range items {
		if item.URL == "" || seen[item.URL] {
			continue
		}
		seen[item.URL] = true
		out = append(out, item)
	}
	return out
}

func downloadToFile(client *http.Client, rawURL, filename string, headers http.Header) error {
	res, err := doRequest(client, http.MethodGet, rawURL, nil, headers)
	if err != nil {
		return err
	}
	if res.StatusCode >= 400 {
		return fmt.Errorf("status %d", res.StatusCode)
	}
	return os.WriteFile(filename, res.Body, 0o644)
}

func detectExt(rawURL, kind string) string {
	u, err := url.Parse(rawURL)
	if err == nil {
		ext := strings.ToLower(path.Ext(u.Path))
		if ext != "" && len(ext) <= 8 {
			return ext
		}
	}
	if kind == "video" {
		return ".mp4"
	}
	return ".jpg"
}

func writeJSON(filename string, payload any) error {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(filename, data, 0o644)
}

func writeErrorJSON(rootDir, sourceURL string, body []byte) {
	_ = writeJSON(filepath.Join(rootDir, "error.json"), map[string]any{
		"sourceUrl": sourceURL,
		"error":     "anti-bot blocked request",
		"body":      string(body),
	})
}

func sanitizeName(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	replacer := strings.NewReplacer(" ", "-", "/", "-", "\\", "-", ":", "-", "?", "", "&", "-", "=", "-", "%", "", "*", "", "\"", "", "<", "", ">", "", "|", "")
	s = replacer.Replace(s)
	s = regexp.MustCompile(`-+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-._")
	if s == "" {
		return "unknown"
	}
	return s
}

func absolutizeURL(baseURL, raw string) string {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, `\/`, `/`))
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err == nil && u.IsAbs() {
		return u.String()
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return raw
	}
	return base.ResolveReference(u).String()
}

func htmlUnescape(s string) string {
	replacer := strings.NewReplacer("&quot;", `"`, "&#34;", `"`, "&amp;", `&`, "&lt;", `<`, "&gt;", `>`)
	return replacer.Replace(strings.TrimSpace(s))
}

func isKnownMediaHost(raw string) bool {
	lower := strings.ToLower(raw)
	return strings.Contains(lower, "cdn") || strings.Contains(lower, "ozon")
}

func asString(v any) string {
	switch typed := v.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}

func asInt(v any) int {
	switch typed := v.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case int64:
		return int(typed)
	case string:
		i, _ := strconv.Atoi(typed)
		return i
	default:
		return 0
	}
}

func asInt64(v any) int64 {
	switch typed := v.(type) {
	case float64:
		return int64(typed)
	case int:
		return int64(typed)
	case int64:
		return typed
	case string:
		i, _ := strconv.ParseInt(typed, 10, 64)
		return i
	default:
		return 0
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
