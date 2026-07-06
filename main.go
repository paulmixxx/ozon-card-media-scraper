package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
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
	BrowserMode  bool
	BrowserPath  string
	Headless     bool
	BrowserWait  time.Duration
}

type BrowserSnapshot struct {
	CardImages   []string        `json:"cardImages"`
	CardVideos   []string        `json:"cardVideos"`
	ReviewImages []string        `json:"reviewImages"`
	ReviewVideos []string        `json:"reviewVideos"`
	Reviews      []BrowserReview `json:"reviews"`
}

type BrowserReview struct {
	ID      string   `json:"id"`
	Author  string   `json:"author,omitempty"`
	Comment string   `json:"comment,omitempty"`
	Photos  []string `json:"photos,omitempty"`
	Videos  []string `json:"videos,omitempty"`
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
	os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr))
}

func execute(args []string, stdout, stderr io.Writer) int {
	err := run(args)
	if err == nil {
		return 0
	}
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stdout, usageText())
		return 0
	}
	fmt.Fprintln(stderr, "error:", err)
	return 1
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

	var cardMedia []MediaItem
	var reviews []ReviewRecord
	var warnings []string

	if opts.BrowserMode {
		snapshot, html, browserWarnings, err := scrapeWithBrowser(sourceURL, ref, opts)
		if err != nil {
			writeErrorJSON(rootDir, sourceURL, []byte(err.Error()))
			return err
		}
		warnings = append(warnings, browserWarnings...)
		if opts.WriteHTML && html != "" {
			_ = os.WriteFile(filepath.Join(rootDir, "product.html"), []byte(html), 0o644)
		}
		htmlCardMedia, htmlWarnings := extractCardMedia(sourceURL, ref, html)
		warnings = append(warnings, htmlWarnings...)
		cardMedia = preferBrowserHTMLMedia(htmlCardMedia, snapshot)
		reviews = normalizeBrowserReviews(snapshot.Reviews)
		for _, raw := range snapshot.ReviewImages {
			reviews = appendReviewLooseMedia(reviews, raw, "image")
		}
		for _, raw := range snapshot.ReviewVideos {
			reviews = appendReviewLooseMedia(reviews, raw, "video")
		}
	} else {
		pageResult, err := doRequest(client, http.MethodGet, sourceURL, nil, buildHeaders(opts, sourceURL, true))
		if err != nil {
			return err
		}
		if isAntiBotRedirect(pageResult) || isAntiBotBlocked(pageResult.StatusCode, pageResult.Body) {
			writeErrorJSON(rootDir, sourceURL, pageResult.Body)
			return &antiBotError{message: "Ozon anti-bot blocked the product page request. Try browser mode on the same desktop: --browser-mode --no-headless."}
		}

		html := string(pageResult.Body)
		if opts.WriteHTML {
			_ = os.WriteFile(filepath.Join(rootDir, "product.html"), pageResult.Body, 0o644)
		}

		cardMedia, warnings = extractCardMedia(sourceURL, ref, html)
		cardMedia = dedupeMedia(cardMedia)
		reviews, _ = fetchReviews(client, opts, sourceURL, ref)
	}

	if len(cardMedia) == 0 {
		warnings = append(warnings, "Не удалось извлечь медиа карточки товара.")
	}
	for i, media := range cardMedia {
		name := fmt.Sprintf("%03d%s", i+1, detectExt(media.URL, media.Kind))
		if err := downloadToFile(client, media.URL, filepath.Join(cardDir, name), buildHeaders(opts, sourceURL, false)); err != nil {
			warnings = append(warnings, fmt.Sprintf("card download failed for %s: %v", media.URL, err))
		}
	}

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
	case "--write-html", "--browser-mode", "--no-headless":
		return false
	default:
		return true
	}
}

func usageText() string {
	return `Usage:
  ozon-card-media-scraper [flags] <ozon-product-url>

Flags:
  --output <dir>         Base output directory (default: .)
  --reviews-pages <n>    Maximum number of review pages to request
  --cookie <value>       Raw Cookie header value
  --cookie-file <path>   Path to a file containing Cookie header value
  --proxy <url>          HTTP(S) proxy URL
  --timeout <sec>        Request timeout in seconds
  --write-html           Save fetched product HTML for debugging
  --user-agent <ua>      Custom User-Agent
  --date <yyyymmdd>      Override output suffix date for testing/debug
  --browser-mode         Use real Chromium browser mode instead of raw HTTP mode
  --browser-path <path>  Chromium/Chrome executable path (default: chromium-browser lookup)
  --browser-wait <sec>   Seconds to wait for page hydration in browser mode (default: 12)
  --no-headless          Run Chromium with UI (recommended on local desktop)
  --help                 Show this help
`
}

func parseFlags(args []string) (CLIOptions, string, error) {
	fs := flag.NewFlagSet("ozon-card-media-scraper", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	opts := CLIOptions{Headless: true}
	fs.StringVar(&opts.OutputDir, "output", ".", "base output directory")
	fs.StringVar(&opts.Cookie, "cookie", "", "raw Cookie header value")
	fs.StringVar(&opts.CookieFile, "cookie-file", "", "path to a file containing Cookie header value")
	fs.StringVar(&opts.Proxy, "proxy", "", "HTTP(S) proxy URL")
	fs.StringVar(&opts.UserAgent, "user-agent", defaultUserAgent, "HTTP user-agent")
	fs.IntVar(&opts.ReviewsPages, "reviews-pages", 20, "maximum number of review pages to request")
	fs.StringVar(&opts.DateOverride, "date", "", "date suffix override in yyyymmdd format (testing/debug)")
	fs.BoolVar(&opts.WriteHTML, "write-html", false, "save fetched product HTML for debugging")
	fs.BoolVar(&opts.BrowserMode, "browser-mode", false, "use real Chromium browser mode")
	fs.StringVar(&opts.BrowserPath, "browser-path", "", "Chromium/Chrome executable path")
	var noHeadless bool
	fs.BoolVar(&noHeadless, "no-headless", false, "run Chromium with visible UI")
	var timeoutSec int
	var browserWaitSec int
	fs.IntVar(&timeoutSec, "timeout", 30, "request timeout in seconds")
	fs.IntVar(&browserWaitSec, "browser-wait", 12, "seconds to wait for page hydration in browser mode")

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
	if browserWaitSec <= 0 {
		browserWaitSec = 12
	}
	opts.Timeout = time.Duration(timeoutSec) * time.Second
	opts.BrowserWait = time.Duration(browserWaitSec) * time.Second
	if opts.ReviewsPages <= 0 {
		opts.ReviewsPages = 1
	}
	if noHeadless {
		opts.Headless = false
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
	cookie, err := resolveCookieValue(opts)
	if err == nil && cookie != "" {
		h.Set("Cookie", cookie)
	}
	return h
}

func resolveCookieValue(opts CLIOptions) (string, error) {
	cookie := strings.TrimSpace(opts.Cookie)
	if cookie == "" && opts.CookieFile != "" {
		b, err := os.ReadFile(opts.CookieFile)
		if err != nil {
			return "", err
		}
		return normalizeCookieHeader(string(b)), nil
	}
	if cookie == "" {
		return "", nil
	}
	if strings.Contains(cookie, ";") || strings.Contains(cookie, "=") {
		return normalizeCookieHeader(cookie), nil
	}
	if strings.ContainsAny(cookie, `/\\`) || strings.HasSuffix(strings.ToLower(cookie), ".txt") {
		if b, err := os.ReadFile(cookie); err == nil {
			return normalizeCookieHeader(string(b)), nil
		}
	}
	return normalizeCookieHeader(cookie), nil
}

func normalizeCookieHeader(raw string) string {
	raw = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "Cookie:"))
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "[") {
		if parsed := cookiesFromJSON(raw); parsed != "" {
			return parsed
		}
	}
	if strings.Contains(raw, "	") {
		if parsed := cookiesFromNetscape(raw); parsed != "" {
			return parsed
		}
	}
	if strings.ContainsAny(raw, "\r\n") {
		parts := splitCookieLines(raw)
		if len(parts) > 0 {
			return strings.Join(parts, "; ")
		}
	}
	return strings.Join(splitCookiePairs(raw), "; ")
}

func splitCookieLines(raw string) []string {
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "Cookie:")
		if strings.Count(line, "	") >= 6 {
			fields := strings.Split(line, "	")
			if len(fields) >= 7 {
				name := strings.TrimSpace(fields[5])
				value := strings.TrimSpace(fields[6])
				if name != "" {
					out = append(out, name+"="+value)
				}
				continue
			}
		}
		for _, pair := range splitCookiePairs(line) {
			out = append(out, pair)
		}
	}
	return dedupeStrings(out)
}

func splitCookiePairs(raw string) []string {
	segments := strings.Split(raw, ";")
	out := make([]string, 0, len(segments))
	for _, segment := range segments {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		if !strings.Contains(segment, "=") {
			continue
		}
		name, value, _ := strings.Cut(segment, "=")
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(value, "\r", ""), "\n", ""))
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if lower == "path" || lower == "domain" || lower == "expires" || lower == "max-age" || lower == "secure" || lower == "httponly" || lower == "samesite" {
			continue
		}
		out = append(out, name+"="+value)
	}
	return dedupeStrings(out)
}

func cookiesFromNetscape(raw string) string {
	pairs := splitCookieLines(raw)
	return strings.Join(pairs, "; ")
}

func cookiesFromJSON(raw string) string {
	var arrayPayload []map[string]any
	if err := json.Unmarshal([]byte(raw), &arrayPayload); err == nil {
		pairs := make([]string, 0, len(arrayPayload))
		for _, item := range arrayPayload {
			name := strings.TrimSpace(asString(item["name"]))
			value := strings.TrimSpace(asString(item["value"]))
			if name != "" {
				pairs = append(pairs, name+"="+value)
			}
		}
		return strings.Join(dedupeStrings(pairs), "; ")
	}
	var objectPayload map[string]any
	if err := json.Unmarshal([]byte(raw), &objectPayload); err == nil {
		if cookies, ok := objectPayload["cookies"].([]any); ok {
			pairs := []string{}
			for _, entry := range cookies {
				item, ok := entry.(map[string]any)
				if !ok {
					continue
				}
				name := strings.TrimSpace(asString(item["name"]))
				value := strings.TrimSpace(asString(item["value"]))
				if name != "" {
					pairs = append(pairs, name+"="+value)
				}
			}
			return strings.Join(dedupeStrings(pairs), "; ")
		}
	}
	return ""
}

func dedupeStrings(items []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
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

func buildBrowserCandidates(ref ProductRef) []string {
	clean := strings.TrimSuffix(ref.Path, "/")
	return []string{
		clean + "/",
		clean + "/reviews",
	}
}

func preferBrowserHTMLMedia(htmlMedia []MediaItem, snapshot BrowserSnapshot) []MediaItem {
	if len(htmlMedia) > 0 {
		result := append([]MediaItem(nil), htmlMedia...)
		hasImage := false
		hasVideo := false
		for _, item := range result {
			if item.Kind == "video" {
				hasVideo = true
			} else {
				hasImage = true
			}
		}
		if !hasImage {
			for _, raw := range snapshot.CardImages {
				result = append(result, MediaItem{URL: raw, Kind: classifyMediaKind(raw)})
			}
		}
		if !hasVideo {
			for _, raw := range snapshot.CardVideos {
				result = append(result, MediaItem{URL: raw, Kind: classifyMediaKind(raw)})
			}
		}
		return dedupeMedia(result)
	}
	result := make([]MediaItem, 0, len(snapshot.CardImages)+len(snapshot.CardVideos))
	for _, raw := range snapshot.CardImages {
		result = append(result, MediaItem{URL: raw, Kind: classifyMediaKind(raw)})
	}
	for _, raw := range snapshot.CardVideos {
		result = append(result, MediaItem{URL: raw, Kind: classifyMediaKind(raw)})
	}
	return dedupeMedia(result)
}

func normalizeBrowserReviews(items []BrowserReview) []ReviewRecord {
	out := make([]ReviewRecord, 0, len(items))
	for idx, item := range items {
		review := ReviewRecord{
			ID:      firstNonEmpty(item.ID, fmt.Sprintf("review-%03d", idx+1)),
			Author:  item.Author,
			Comment: item.Comment,
		}
		seen := map[string]bool{}
		for _, raw := range item.Photos {
			raw = strings.TrimSpace(raw)
			if raw == "" || seen[raw] {
				continue
			}
			seen[raw] = true
			review.Media = append(review.Media, MediaItem{URL: raw, Kind: "image"})
		}
		for _, raw := range item.Videos {
			raw = strings.TrimSpace(raw)
			if raw == "" || seen[raw] {
				continue
			}
			seen[raw] = true
			review.Media = append(review.Media, MediaItem{URL: raw, Kind: "video"})
		}
		out = append(out, review)
	}
	return out
}

func appendReviewLooseMedia(reviews []ReviewRecord, raw, kind string) []ReviewRecord {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return reviews
	}
	for i := range reviews {
		for _, media := range reviews[i].Media {
			if media.URL == raw {
				return reviews
			}
		}
	}
	for i := range reviews {
		if reviews[i].ID == "review-media-unassigned" {
			reviews[i].Media = append(reviews[i].Media, MediaItem{URL: raw, Kind: kind})
			return reviews
		}
	}
	return append(reviews, ReviewRecord{
		ID:    "review-media-unassigned",
		Media: []MediaItem{{URL: raw, Kind: kind}},
	})
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
					out = append(out, MediaItem{URL: absolutizeURL(sourceURL, value), Kind: classifyMediaKind(key + ":" + value)})
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

func scrapeWithBrowser(sourceURL string, ref ProductRef, opts CLIOptions) (BrowserSnapshot, string, []string, error) {
	browserPath, err := resolveBrowserPath(opts.BrowserPath)
	if err != nil {
		return BrowserSnapshot{}, "", nil, err
	}
	cookieHeader, err := resolveCookieValue(opts)
	if err != nil {
		return BrowserSnapshot{}, "", nil, err
	}

	runtimeDir, err := os.MkdirTemp("", "ozon-chromium-runtime-*")
	if err != nil {
		return BrowserSnapshot{}, "", nil, err
	}
	defer os.RemoveAll(runtimeDir)
	userDataDir, err := os.MkdirTemp("", "ozon-chromium-profile-*")
	if err != nil {
		return BrowserSnapshot{}, "", nil, err
	}
	defer os.RemoveAll(userDataDir)

	allocatorOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(browserPath),
		chromedp.NoSandbox,
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("user-data-dir", userDataDir),
		chromedp.Flag("headless", opts.Headless),
		chromedp.UserAgent(opts.UserAgent),
		chromedp.Env("XDG_RUNTIME_DIR="+runtimeDir),
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), allocatorOpts...)
	defer cancelAlloc()

	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	logWriter := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(logWriter)

	timeout := opts.Timeout + opts.BrowserWait + 20*time.Second
	ctx, cancelTimeout := context.WithTimeout(ctx, timeout)
	defer cancelTimeout()

	headers := network.Headers(map[string]any{
		"Accept-Language":           "ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7",
		"Cache-Control":             "max-age=0",
		"Upgrade-Insecure-Requests": "1",
		"Sec-Fetch-Site":            "none",
		"Sec-Fetch-Mode":            "navigate",
		"Sec-Fetch-User":            "?1",
		"Sec-Fetch-Dest":            "document",
		"sec-ch-ua":                 `"Not.A/Brand";v="8", "Chromium";v="137", "Google Chrome";v="137"`,
		"sec-ch-ua-mobile":          "?0",
		"sec-ch-ua-platform":        `"Linux"`,
	})
	if cookieHeader != "" {
		headers["Cookie"] = cookieHeader
	}

	var snapshot BrowserSnapshot
	var html string
	var location string
	warnings := []string{}

	if err := chromedp.Run(ctx,
		network.Enable(),
		network.SetExtraHTTPHeaders(headers),
		chromedp.Navigate(sourceURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Sleep(opts.BrowserWait),
		chromedp.Location(&location),
		chromedp.OuterHTML("html", &html, chromedp.ByQuery),
		chromedp.Evaluate(browserExtractJS, &snapshot),
	); err != nil {
		return BrowserSnapshot{}, "", warnings, fmt.Errorf("browser mode failed: %w", err)
	}
	if !browserPageLooksUsable(location, html, snapshot) {
		return BrowserSnapshot{}, html, warnings, &antiBotError{message: "browser mode still hit Ozon anti-bot. Run with visible browser on your desktop: --browser-mode --no-headless --browser-wait 20"}
	}

	for _, candidate := range buildBrowserCandidates(ref)[1:] {
		candidateURL := absolutizeURL(sourceURL, candidate)
		var extra BrowserSnapshot
		var pageHTML string
		if err := chromedp.Run(ctx,
			chromedp.Navigate(candidateURL),
			chromedp.WaitReady("body", chromedp.ByQuery),
			chromedp.Sleep(opts.BrowserWait),
			chromedp.OuterHTML("html", &pageHTML, chromedp.ByQuery),
			chromedp.Evaluate(browserExtractJS, &extra),
		); err != nil {
			warnings = append(warnings, fmt.Sprintf("browser candidate failed for %s: %v", candidateURL, err))
			continue
		}
		if html == "" {
			html = pageHTML
		}
		snapshot.CardImages = append(snapshot.CardImages, extra.CardImages...)
		snapshot.CardVideos = append(snapshot.CardVideos, extra.CardVideos...)
		snapshot.ReviewImages = append(snapshot.ReviewImages, extra.ReviewImages...)
		snapshot.ReviewVideos = append(snapshot.ReviewVideos, extra.ReviewVideos...)
		snapshot.Reviews = append(snapshot.Reviews, extra.Reviews...)
	}

	snapshot.CardImages = filterBrowserURLs(snapshot.CardImages)
	snapshot.CardVideos = filterBrowserURLs(snapshot.CardVideos)
	snapshot.ReviewImages = filterBrowserURLs(snapshot.ReviewImages)
	snapshot.ReviewVideos = filterBrowserURLs(snapshot.ReviewVideos)
	return snapshot, html, warnings, nil
}

func resolveBrowserPath(explicit string) (string, error) {
	for _, candidate := range []string{strings.TrimSpace(explicit), "chromium-browser", "google-chrome", "chromium", "chrome"} {
		if candidate == "" {
			continue
		}
		if strings.Contains(candidate, "/") {
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
			continue
		}
		if found, err := exec.LookPath(candidate); err == nil {
			return found, nil
		}
	}
	return "", errors.New("Chromium/Chrome executable not found; install chromium-browser or pass --browser-path")
}

func filterBrowserURLs(items []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, raw := range items {
		raw = strings.TrimSpace(raw)
		if raw == "" || seen[raw] {
			continue
		}
		if !isKnownMediaHost(raw) && !looksLikeMediaURL(raw) {
			continue
		}
		seen[raw] = true
		out = append(out, raw)
	}
	return out
}

func browserPageLooksUsable(location, html string, snapshot BrowserSnapshot) bool {
	lowerLocation := strings.ToLower(location)
	lowerHTML := strings.ToLower(html)
	if !strings.Contains(lowerLocation, "__rr=1") && !strings.Contains(lowerLocation, "block.html") && !isAntiBotBlocked(200, []byte(html)) {
		return true
	}
	if len(snapshot.CardImages) > 0 || len(snapshot.CardVideos) > 0 || len(snapshot.ReviewImages) > 0 || len(snapshot.ReviewVideos) > 0 || len(snapshot.Reviews) > 0 {
		return true
	}
	productMarkers := []string{"в корзину", "купить сейчас", "о товаре", "отзывы", "артикул:", "доставка", "ozon fresh"}
	for _, marker := range productMarkers {
		if strings.Contains(lowerHTML, marker) {
			return true
		}
	}
	return false
}

const browserExtractJS = `(() => {
  const abs = (value) => {
    if (!value) return "";
    try { return new URL(value, location.href).href; } catch { return ""; }
  };
  const uniq = (items) => [...new Set(items.map((v) => (v || "").trim()).filter(Boolean))];
  const addBackgrounds = (bucket, node) => {
    const style = node.getAttribute && node.getAttribute("style");
    if (!style) return;
    const matches = style.match(/url\(([^)]+)\)/gi) || [];
    for (const match of matches) {
      const raw = match.replace(/^url\((.*)\)$/i, "$1").replace(/^['\"]|['\"]$/g, "");
      const url = abs(raw);
      if (url) bucket.push(url);
    }
  };
  const attrNames = [
    "src", "href", "content", "poster", "data-src", "data-origin", "data-large-image",
    "data-preview", "data-video", "data-url", "data-image", "data-img", "srcset", "data-srcset"
  ];
  const collectFromRoot = (root) => {
    const images = [];
    const videos = [];
    const nodes = root.querySelectorAll("img,source,video,a,meta,[style],[data-src],[data-srcset],[data-origin],[data-large-image],[data-video]");
    nodes.forEach((node) => {
      if (node.currentSrc) {
        const url = abs(node.currentSrc);
        if (url) {
          if (/\.(mp4|mov|webm|m3u8)(\?|$)/i.test(url) || /video/i.test(url)) videos.push(url); else images.push(url);
        }
      }
      for (const attr of attrNames) {
        const value = node.getAttribute && node.getAttribute(attr);
        if (!value) continue;
        for (const part of String(value).split(",")) {
          const candidate = part.trim().split(/\s+/)[0];
          const url = abs(candidate);
          if (!url) continue;
          if (/\.(mp4|mov|webm|m3u8)(\?|$)/i.test(url) || /video/i.test(url)) {
            videos.push(url);
          } else {
            images.push(url);
          }
        }
      }
      addBackgrounds(images, node);
    });
    return { images: uniq(images), videos: uniq(videos) };
  };
  const card = collectFromRoot(document);
  const reviewRoots = Array.from(document.querySelectorAll('[data-review-uuid], [data-widget*="review"], article'));
  const reviews = [];
  const looseReviewImages = [];
  const looseReviewVideos = [];
  reviewRoots.forEach((root, index) => {
    const media = collectFromRoot(root);
    if (media.images.length === 0 && media.videos.length === 0) return;
    looseReviewImages.push(...media.images);
    looseReviewVideos.push(...media.videos);
    const textNode = root.querySelector('[data-widget*="text"], [itemprop="reviewBody"], p, span');
    const authorNode = root.querySelector('[itemprop="author"], [data-widget*="author"]');
    reviews.push({
      id: root.getAttribute('data-review-uuid') || root.id || ('review-' + String(index + 1).padStart(3, '0')),
      author: authorNode ? (authorNode.textContent || '').trim() : '',
      comment: textNode ? (textNode.textContent || '').trim() : '',
      photos: media.images,
      videos: media.videos,
    });
  });
  return {
    cardImages: card.images,
    cardVideos: card.videos,
    reviewImages: uniq(looseReviewImages),
    reviewVideos: uniq(looseReviewVideos),
    reviews,
  };
})()`

func downloadToFile(client *http.Client, rawURL, filename string, headers http.Header) error {
	res, err := doRequest(client, http.MethodGet, rawURL, nil, headers)
	if err != nil {
		return err
	}
	if res.StatusCode >= 400 {
		return fmt.Errorf("status %d", res.StatusCode)
	}
	kind := classifyMediaKind(rawURL)
	if err := validateMediaResponse(rawURL, kind, res); err != nil {
		return err
	}
	return os.WriteFile(filename, res.Body, 0o644)
}

func validateMediaResponse(rawURL, kind string, res HTTPResult) error {
	contentType := strings.ToLower(strings.TrimSpace(res.Header.Get("Content-Type")))
	if idx := strings.Index(contentType, ";"); idx >= 0 {
		contentType = strings.TrimSpace(contentType[:idx])
	}
	if contentType == "text/html" || contentType == "application/json" || contentType == "text/plain" {
		return fmt.Errorf("unexpected content-type %s for %s", contentType, rawURL)
	}
	body := res.Body
	if len(body) == 0 {
		return fmt.Errorf("empty body for %s", rawURL)
	}
	if looksLikeHTML(body) {
		return fmt.Errorf("unexpected HTML body for %s", rawURL)
	}
	if kind == "video" {
		if isRecognizedVideoContent(contentType, body) {
			return nil
		}
		return fmt.Errorf("response body is not recognized as video for %s", rawURL)
	}
	if isRecognizedImageContent(contentType, body) {
		return nil
	}
	return fmt.Errorf("response body is not recognized as image for %s", rawURL)
}

func looksLikeHTML(body []byte) bool {
	sample := strings.ToLower(string(body[:min(len(body), 512)]))
	return strings.Contains(sample, "<html") || strings.Contains(sample, "<!doctype html") || strings.Contains(sample, "<body")
}

func isRecognizedImageContent(contentType string, body []byte) bool {
	if strings.HasPrefix(contentType, "image/") {
		return true
	}
	if len(body) >= 3 && body[0] == 0xff && body[1] == 0xd8 && body[2] == 0xff {
		return true
	}
	if len(body) >= 8 && bytes.Equal(body[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		return true
	}
	if len(body) >= 6 && (bytes.Equal(body[:6], []byte("GIF87a")) || bytes.Equal(body[:6], []byte("GIF89a"))) {
		return true
	}
	if len(body) >= 12 && bytes.Equal(body[:4], []byte("RIFF")) && bytes.Equal(body[8:12], []byte("WEBP")) {
		return true
	}
	return false
}

func isRecognizedVideoContent(contentType string, body []byte) bool {
	if strings.HasPrefix(contentType, "video/") || contentType == "application/vnd.apple.mpegurl" || contentType == "application/x-mpegurl" {
		return true
	}
	if len(body) >= 8 && bytes.Equal(body[4:8], []byte("ftyp")) {
		return true
	}
	if len(body) >= 4 && bytes.Equal(body[:4], []byte{0x1a, 0x45, 0xdf, 0xa3}) {
		return true
	}
	if strings.HasPrefix(string(body[:min(len(body), 16)]), "#EXTM3U") {
		return true
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
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
