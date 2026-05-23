package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"flatnasgo-backend/config"

	"github.com/gin-gonic/gin"
)

// AliIcons Cache
type cachedAliIcons struct {
	Data      interface{}
	Timestamp time.Time
}

var (
	aliIconsCache cachedAliIcons
	aliIconsMutex sync.RWMutex
	// Cache duration: 24 hours
	aliIconsCacheDuration = 24 * time.Hour
)

const (
	// Use the URL that we verified works
	aliIconsURL         = "https://icon-manager.1851365c.er.aliyun-esa.net/icons.json"
	maxIconCacheSize    = 5 * 1024 * 1024
	defaultIconFileMode = 0644
)

type iconCachePayload struct {
	URL     string `json:"url"`
	DataURL string `json:"dataUrl"`
}

type iconError struct {
	Status  int
	Code    string
	Message string
	Err     error
}

func (e *iconError) Error() string {
	return e.Message
}

func boolEnv(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return v
}

func intEnv(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return v
}

var (
	forceWebPEnabled = boolEnv("ICON_CACHE_FORCE_WEBP", true)
	webPQuality      = intEnv("ICON_CACHE_WEBP_QUALITY", 82)
)

func respondIconError(c *gin.Context, iconErr *iconError) {
	payload := gin.H{
		"success": false,
		"error": gin.H{
			"code":    iconErr.Code,
			"message": iconErr.Message,
		},
	}
	if iconErr.Err != nil {
		payload["error"].(gin.H)["details"] = iconErr.Err.Error()
	}
	c.JSON(iconErr.Status, payload)
}

// CacheIcon caches a remote icon URL or dataURL to local disk.
func CacheIcon(c *gin.Context) {
	start := time.Now()
	var payload iconCachePayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		respondIconError(c, &iconError{
			Status:  http.StatusBadRequest,
			Code:    "invalid_json",
			Message: "Invalid JSON",
			Err:     err,
		})
		return
	}

	urlInput := strings.TrimSpace(payload.URL)
	dataURLInput := strings.TrimSpace(payload.DataURL)
	if (urlInput == "" && dataURLInput == "") || (urlInput != "" && dataURLInput != "") {
		respondIconError(c, &iconError{
			Status:  http.StatusBadRequest,
			Code:    "invalid_payload",
			Message: "Exactly one of url or dataUrl is required",
		})
		return
	}

	var (
		content     []byte
		contentType string
		err         *iconError
		sourceType  = "dataUrl"
	)
	if urlInput != "" {
		sourceType = "url"
		content, contentType, err = fetchIconFromURL(urlInput)
	} else {
		content, contentType, err = decodeIconDataURL(dataURLInput)
	}
	if err != nil {
		log.Printf("[icon-cache] source=%s cache_hit=false duration_ms=%d status=failed code=%s", sourceType, time.Since(start).Milliseconds(), err.Code)
		respondIconError(c, err)
		return
	}

	if len(content) == 0 {
		respondIconError(c, &iconError{
			Status:  http.StatusBadRequest,
			Code:    "empty_icon_content",
			Message: "Empty icon content",
		})
		return
	}
	if len(content) > maxIconCacheSize {
		respondIconError(c, &iconError{
			Status:  http.StatusRequestEntityTooLarge,
			Code:    "icon_too_large",
			Message: "Icon exceeds 5MB limit",
		})
		return
	}

	ext := resolveImageExtension(contentType, content)
	if ext == "" {
		respondIconError(c, &iconError{
			Status:  http.StatusUnsupportedMediaType,
			Code:    "unsupported_icon_type",
			Message: "Unsupported icon type",
		})
		return
	}

	if ext == ".svg" {
		if err := validateSafeSVG(content); err != nil {
			respondIconError(c, &iconError{
				Status:  http.StatusUnsupportedMediaType,
				Code:    "unsafe_svg",
				Message: "SVG contains unsupported or unsafe elements",
				Err:     err,
			})
			return
		}
	}

	if forceWebPEnabled {
		normalizedContent, normalizedType, normalizedExt, converted, convErr := normalizeRasterToWebP(content, contentType, ext)
		if convErr != nil {
			log.Printf("[icon-cache] webp_normalize_failed source=%s err=%v", sourceType, convErr)
		} else if converted {
			content = normalizedContent
			contentType = normalizedType
			ext = normalizedExt
		}
	}

	sum := sha256.Sum256(content)
	filename := fmt.Sprintf("%x%s", sum, ext)
	target := filepath.Join(config.IconCacheDir, filename)
	cacheHit := false
	if _, statErr := os.Stat(target); statErr == nil {
		cacheHit = true
	}

	if err := os.WriteFile(target, content, defaultIconFileMode); err != nil {
		respondIconError(c, &iconError{
			Status:  http.StatusInternalServerError,
			Code:    "icon_cache_write_failed",
			Message: "Failed to write icon cache",
			Err:     err,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success":    true,
		"path":       "/icon-cache/" + filename,
		"sourceType": sourceType,
		"cacheHit":   cacheHit,
		"mimeType":   contentType,
		"sizeBytes":  len(content),
	})
	log.Printf("[icon-cache] source=%s cache_hit=%t duration_ms=%d status=ok ext=%s size=%d", sourceType, cacheHit, time.Since(start).Milliseconds(), ext, len(content))
}

// GetAliIcons proxies the request to Alibaba Icon Manager to avoid CORS issues
func GetAliIcons(c *gin.Context) {
	aliIconsMutex.RLock()
	if aliIconsCache.Data != nil && time.Since(aliIconsCache.Timestamp) < aliIconsCacheDuration {
		data := aliIconsCache.Data
		aliIconsMutex.RUnlock()
		c.JSON(http.StatusOK, data)
		return
	}
	aliIconsMutex.RUnlock()

	// Fetch from upstream
	client, err := getSharedProxyClient()
	if err != nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Get(aliIconsURL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch icons from upstream", "details": err.Error()})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Upstream returned non-200 status", "status": resp.StatusCode})
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read response body", "details": err.Error()})
		return
	}

	var data interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to parse JSON", "details": err.Error()})
		return
	}

	// Update cache
	aliIconsMutex.Lock()
	aliIconsCache.Data = data
	aliIconsCache.Timestamp = time.Now()
	aliIconsMutex.Unlock()

	c.JSON(http.StatusOK, data)
}

// GetIconBase64 fetches a URL and returns it as base64
func GetIconBase64(c *gin.Context) {
	urlStr := c.Query("url")
	if urlStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing url parameter"})
		return
	}

	parsed, err := url.Parse(urlStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid URL"})
		return
	}

	if IsBlockedHost(parsed.Hostname()) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Target host is not allowed"})
		return
	}

	client, err := getSharedProxyClient()
	if err != nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Get(urlStr)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch icon", "details": err.Error()})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Upstream returned non-200 status", "status": resp.StatusCode})
		return
	}

	// Limit size to avoid memory issues (e.g., 5MB)
	const maxLimit = 5 * 1024 * 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLimit))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read body", "details": err.Error()})
		return
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream" // fallback
	}

	base64Str := base64.StdEncoding.EncodeToString(body)
	dataURI := fmt.Sprintf("data:%s;base64,%s", contentType, base64Str)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"icon":    dataURI,
	})
}

func fetchIconFromURL(urlStr string) ([]byte, string, *iconError) {
	parsed, err := url.Parse(urlStr)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, "", &iconError{
			Status:  http.StatusBadRequest,
			Code:    "invalid_url",
			Message: "Invalid URL",
			Err:     err,
		}
	}
	if IsBlockedHost(parsed.Hostname()) {
		return nil, "", &iconError{
			Status:  http.StatusForbidden,
			Code:    "blocked_host",
			Message: "Target host is not allowed",
		}
	}

	client, err := getSharedProxyClient()
	if err != nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Get(urlStr)
	if err != nil {
		return nil, "", &iconError{
			Status:  http.StatusBadGateway,
			Code:    "fetch_failed",
			Message: "Failed to fetch icon",
			Err:     err,
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", &iconError{
			Status:  http.StatusBadGateway,
			Code:    "upstream_status_not_ok",
			Message: fmt.Sprintf("Upstream returned non-200 status: %d", resp.StatusCode),
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIconCacheSize+1))
	if err != nil {
		return nil, "", &iconError{
			Status:  http.StatusBadGateway,
			Code:    "fetch_read_failed",
			Message: "Failed to read icon body",
			Err:     err,
		}
	}
	if len(body) > maxIconCacheSize {
		return nil, "", &iconError{
			Status:  http.StatusRequestEntityTooLarge,
			Code:    "icon_too_large",
			Message: "Icon exceeds 5MB limit",
		}
	}
	return body, resp.Header.Get("Content-Type"), nil
}

func decodeIconDataURL(raw string) ([]byte, string, *iconError) {
	if !strings.HasPrefix(raw, "data:") {
		return nil, "", &iconError{
			Status:  http.StatusBadRequest,
			Code:    "invalid_data_url",
			Message: "Invalid dataUrl",
		}
	}
	comma := strings.Index(raw, ",")
	if comma <= 5 {
		return nil, "", &iconError{
			Status:  http.StatusBadRequest,
			Code:    "invalid_data_url",
			Message: "Invalid dataUrl",
		}
	}

	meta := raw[5:comma]
	dataPart := raw[comma+1:]
	if !strings.Contains(strings.ToLower(meta), ";base64") {
		return nil, "", &iconError{
			Status:  http.StatusBadRequest,
			Code:    "data_url_not_base64",
			Message: "dataUrl must be base64 encoded",
		}
	}
	baseType := strings.TrimSpace(strings.Split(meta, ";")[0])
	decoded, err := base64.StdEncoding.DecodeString(dataPart)
	if err != nil {
		return nil, "", &iconError{
			Status:  http.StatusBadRequest,
			Code:    "invalid_base64_data_url",
			Message: "Invalid base64 dataUrl",
			Err:     err,
		}
	}
	if len(decoded) > maxIconCacheSize {
		return nil, "", &iconError{
			Status:  http.StatusRequestEntityTooLarge,
			Code:    "icon_too_large",
			Message: "Icon exceeds 5MB limit",
		}
	}
	return decoded, baseType, nil
}

func validateSafeSVG(content []byte) error {
	lower := strings.ToLower(string(content))
	unsafeTokens := []string{
		"<script",
		"javascript:",
		"onload=",
		"onerror=",
		"onclick=",
		"<foreignobject",
		"<iframe",
		"<object",
		"<embed",
	}
	for _, token := range unsafeTokens {
		if strings.Contains(lower, token) {
			return fmt.Errorf("contains unsafe token: %s", token)
		}
	}
	return nil
}

func resolveImageExtension(contentType string, content []byte) string {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if semi := strings.Index(ct, ";"); semi > 0 {
		ct = ct[:semi]
	}

	if ext := imageExtFromMime(ct); ext != "" {
		return ext
	}
	detected := strings.ToLower(http.DetectContentType(content))
	if semi := strings.Index(detected, ";"); semi > 0 {
		detected = detected[:semi]
	}
	if ext := imageExtFromMime(detected); ext != "" {
		return ext
	}
	if looksLikeSVG(content) {
		return ".svg"
	}
	if looksLikeICO(content) {
		return ".ico"
	}
	return ""
}

func imageExtFromMime(m string) string {
	switch m {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/svg+xml":
		return ".svg"
	case "image/x-icon", "image/vnd.microsoft.icon":
		return ".ico"
	}
	if m != "" {
		if exts, _ := mime.ExtensionsByType(m); len(exts) > 0 {
			for _, ext := range exts {
				switch ext {
				case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".svg", ".ico":
					if ext == ".jpeg" {
						return ".jpg"
					}
					return ext
				}
			}
		}
	}
	return ""
}

func looksLikeSVG(content []byte) bool {
	trimmed := strings.TrimSpace(string(content))
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	return strings.HasPrefix(lower, "<?xml") || strings.Contains(lower, "<svg")
}

func looksLikeICO(content []byte) bool {
	if len(content) < 4 {
		return false
	}
	return bytes.Equal(content[:4], []byte{0x00, 0x00, 0x01, 0x00})
}

// FetchFavicon extracts the best favicon URL from a target website.
// It parses HTML <link> tags and falls back to /favicon.ico.
// Unlike other icon handlers, this intentionally allows private/LAN hosts
// since users need to discover favicons from their internal services.
func FetchFavicon(c *gin.Context) {
	targetURL := c.Query("url")
	if targetURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "Missing url parameter"})
		return
	}

	parsed, err := url.Parse(targetURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "Invalid URL, must be http or https"})
		return
	}

	// Build a client that allows access to private hosts (LAN services).
	// Use a shorter timeout to avoid long hangs on unreachable hosts.
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}

	// Also try to use proxy client for external sites
	proxyClient, proxyErr := getSharedProxyClient()
	if proxyErr == nil && proxyClient != nil {
		// For external hosts, prefer proxy client; for internal, use direct client
		if !isPrivateHost(parsed.Hostname()) {
			client = proxyClient
			// Override timeout
			client.Timeout = 10 * time.Second
		}
	}

	// Step 1: Fetch the HTML page
	req, err := http.NewRequest("GET", parsed.String(), nil)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "Failed to create request"})
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "error": "Failed to fetch target page", "details": err.Error()})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.JSON(http.StatusOK, gin.H{"success": false, "error": fmt.Sprintf("Target returned status %d", resp.StatusCode)})
		return
	}

	// Use the final URL after redirects as the base for resolving relative URLs
	finalURL := resp.Request.URL.String()

	// Read the HTML body (limit to 2MB to avoid memory issues)
	const maxHTMLSize = 2 * 1024 * 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTMLSize))
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "error": "Failed to read page body"})
		return
	}
	html := string(body)

	// Step 2: Parse <link> tags to find favicon
	bestIconURL := extractFaviconFromHTML(html, finalURL)

	// Step 3: Verify the extracted icon URL
	if bestIconURL != "" && verifyIconURL(client, bestIconURL) {
		c.JSON(http.StatusOK, gin.H{"success": true, "icon": bestIconURL})
		return
	}

	// Step 4: Fallback to /favicon.ico
	fallbackParsed, err := url.Parse(finalURL)
	if err == nil {
		fallbackURL := fmt.Sprintf("%s://%s/favicon.ico", fallbackParsed.Scheme, fallbackParsed.Host)
		if verifyIconURL(client, fallbackURL) {
			c.JSON(http.StatusOK, gin.H{"success": true, "icon": fallbackURL})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"success": false, "error": "No favicon found"})
}

// extractFaviconFromHTML parses HTML to find the best favicon URL from <link> tags.
func extractFaviconFromHTML(html, baseURL string) string {
	// Match all <link> tags
	linkTagRegex := regexp.MustCompile(`(?i)<link[^>]+>`)
	linkTags := linkTagRegex.FindAllString(html, -1)

	var bestIconURL string

	for _, tag := range linkTags {
		rel := extractAttr(tag, "rel")
		href := extractAttr(tag, "href")

		if rel == "" || href == "" {
			continue
		}

		relLower := strings.ToLower(rel)
		relTokens := strings.Fields(relLower)

		// Check for standard icon
		for _, token := range relTokens {
			if token == "icon" {
				resolved := resolveURL(href, baseURL)
				if resolved != "" {
					return resolved // Found standard icon, return immediately
				}
			}
		}

		// Check for apple-touch-icon as fallback
		if bestIconURL == "" {
			for _, token := range relTokens {
				if token == "apple-touch-icon" || token == "apple-touch-icon-precomposed" {
					resolved := resolveURL(href, baseURL)
					if resolved != "" {
						bestIconURL = resolved
					}
				}
			}
		}
	}

	return bestIconURL
}

// extractAttr extracts an attribute value from an HTML tag string.
func extractAttr(tag, attrName string) string {
	// Pattern: attrName="value" or attrName='value' or attrName=value
	pattern := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(attrName) + `=["']?([^"'>\s]+)["']?`)
	match := pattern.FindStringSubmatch(tag)
	if len(match) >= 2 {
		return match[1]
	}
	return ""
}

// resolveURL resolves a potentially relative URL against a base URL.
func resolveURL(href, baseURL string) string {
	if href == "" {
		return ""
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	ref, err := url.Parse(href)
	if err != nil {
		return ""
	}
	return base.ResolveReference(ref).String()
}

// verifyIconURL checks if a URL points to a valid image resource.
func verifyIconURL(client *http.Client, iconURL string) bool {
	// Try HEAD first
	req, err := http.NewRequest("HEAD", iconURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()

	// If HEAD fails with 405 or 403, try GET
	if resp.StatusCode == 405 || resp.StatusCode == 403 {
		getReq, err := http.NewRequest("GET", iconURL, nil)
		if err != nil {
			return false
		}
		getReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
		getResp, err := client.Do(getReq)
		if err != nil {
			return false
		}
		defer getResp.Body.Close()
		// Read a small portion to check content type
		io.ReadAll(io.LimitReader(getResp.Body, 1024))
		ct := strings.ToLower(getResp.Header.Get("Content-Type"))
		return getResp.StatusCode == 200 && strings.Contains(ct, "image")
	}

	if resp.StatusCode != 200 {
		return false
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	// Allow image/* or application/octet-stream (some servers don't set correct type for .ico)
	// Also allow missing content-type for favicon.ico
	return strings.Contains(ct, "image") || strings.Contains(ct, "octet-stream") || ct == ""
}

// isPrivateHost checks if a hostname resolves to a private/internal IP address.
func isPrivateHost(host string) bool {
	if host == "" {
		return false
	}
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "localhost" || host == "localhost." {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	// Try DNS resolution
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, item := range ips {
		if item.IP != nil && (item.IP.IsLoopback() || item.IP.IsPrivate() || item.IP.IsLinkLocalUnicast()) {
			return true
		}
	}
	return false
}
