package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// ImageFingerprintCacheTTL bounds how long a fingerprint remains hot.
	ImageFingerprintCacheTTL = 24 * time.Hour

	// ImageFingerprintCacheMaxEntries bounds process memory used by fingerprints.
	ImageFingerprintCacheMaxEntries = 4096
)

// ImageFingerprint identifies image content without retaining the image bytes.
type ImageFingerprint struct {
	Key      string
	MIMEType string
	Size     int
}

type imageFingerprintEntry struct {
	LastSeen time.Time
}

// ImageFingerprintCache tracks recently observed image content.
type ImageFingerprintCache struct {
	mu         sync.Mutex
	entries    map[string]imageFingerprintEntry
	maxEntries int
	ttl        time.Duration
	now        func() time.Time
}

// NewImageFingerprintCache creates a bounded in-memory image fingerprint cache.
func NewImageFingerprintCache(maxEntries int, ttl time.Duration) *ImageFingerprintCache {
	if maxEntries <= 0 {
		maxEntries = ImageFingerprintCacheMaxEntries
	}
	if ttl <= 0 {
		ttl = ImageFingerprintCacheTTL
	}
	return &ImageFingerprintCache{
		entries:    make(map[string]imageFingerprintEntry),
		maxEntries: maxEntries,
		ttl:        ttl,
		now:        time.Now,
	}
}

// Observe records fingerprints and returns hit and miss counts for this batch.
func (c *ImageFingerprintCache) Observe(fingerprints []ImageFingerprint) (hits, misses int) {
	if c == nil || len(fingerprints) == 0 {
		return 0, 0
	}
	now := time.Now()
	if c.now != nil {
		now = c.now()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, fingerprint := range fingerprints {
		key := strings.TrimSpace(fingerprint.Key)
		if key == "" {
			continue
		}
		entry, ok := c.entries[key]
		if ok && now.Sub(entry.LastSeen) <= c.ttl {
			c.entries[key] = imageFingerprintEntry{LastSeen: now}
			hits++
			continue
		}
		if ok {
			delete(c.entries, key)
		}
		c.evictIfNeededLocked()
		c.entries[key] = imageFingerprintEntry{LastSeen: now}
		misses++
	}
	return hits, misses
}

// Clear removes all fingerprints from the cache.
func (c *ImageFingerprintCache) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.entries = make(map[string]imageFingerprintEntry)
	c.mu.Unlock()
}

func (c *ImageFingerprintCache) evictIfNeededLocked() {
	if len(c.entries) < c.maxEntries {
		return
	}
	oldestKey := ""
	var oldest time.Time
	for key, entry := range c.entries {
		if oldestKey == "" || entry.LastSeen.Before(oldest) {
			oldestKey = key
			oldest = entry.LastSeen
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

var defaultImageFingerprintCache = NewImageFingerprintCache(ImageFingerprintCacheMaxEntries, ImageFingerprintCacheTTL)

// ObserveImageFingerprints records fingerprints in the process-wide cache.
func ObserveImageFingerprints(fingerprints []ImageFingerprint) (hits, misses int) {
	return defaultImageFingerprintCache.Observe(fingerprints)
}

// ClearImageFingerprintCache clears the process-wide fingerprint cache.
func ClearImageFingerprintCache() {
	defaultImageFingerprintCache.Clear()
}

// ExtractImageFingerprints finds common OpenAI, Anthropic, and Gemini image inputs.
// Remote URLs are keyed by URL because this MVP does not fetch external content.
func ExtractImageFingerprints(body []byte) []ImageFingerprint {
	if len(body) == 0 || !mayContainImageInput(body) {
		return nil
	}
	var root any
	if errUnmarshal := json.Unmarshal(body, &root); errUnmarshal != nil {
		return nil
	}

	seen := make(map[string]struct{})
	result := make([]ImageFingerprint, 0, 2)
	var walk func(any)
	walk = func(value any) {
		switch typed := value.(type) {
		case []any:
			for _, item := range typed {
				walk(item)
			}
		case map[string]any:
			collectImageMap(typed, &result, seen)
			for _, item := range typed {
				walk(item)
			}
		}
	}
	walk(root)
	return result
}

func mayContainImageInput(body []byte) bool {
	for _, marker := range [][]byte{
		[]byte(`"image_url"`),
		[]byte(`"imageUrl"`),
		[]byte(`"input_image"`),
		[]byte(`"inline_data"`),
		[]byte(`"inlineData"`),
		[]byte(`"media_type"`),
	} {
		if bytes.Contains(body, marker) {
			return true
		}
	}
	return false
}

func collectImageMap(value map[string]any, result *[]ImageFingerprint, seen map[string]struct{}) {
	typeName, _ := value["type"].(string)
	typeName = strings.ToLower(strings.TrimSpace(typeName))
	if typeName == "image" || typeName == "input_image" || typeName == "image_url" {
		if reference, ok := value["image_url"]; ok {
			appendImageReference(result, seen, reference, "")
		}
	}

	if reference, ok := value["image_url"]; ok {
		appendImageReference(result, seen, reference, "")
	}
	if reference, ok := value["imageUrl"]; ok {
		appendImageReference(result, seen, reference, "")
	}

	for _, key := range []string{"inlineData", "inline_data"} {
		if inline, ok := value[key].(map[string]any); ok {
			data, _ := inline["data"].(string)
			mimeType := firstString(inline, "mimeType", "mime_type")
			appendBase64Image(result, seen, mimeType, data)
		}
	}

	if source, ok := value["source"].(map[string]any); ok {
		data, _ := source["data"].(string)
		mimeType := firstString(source, "media_type", "mime_type", "mimeType")
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(mimeType)), "image/") {
			appendBase64Image(result, seen, mimeType, data)
		}
	}
}

func appendImageReference(result *[]ImageFingerprint, seen map[string]struct{}, reference any, mimeHint string) {
	switch typed := reference.(type) {
	case string:
		appendImageString(result, seen, typed, mimeHint)
	case map[string]any:
		urlValue, _ := typed["url"].(string)
		if urlValue == "" {
			urlValue, _ = typed["uri"].(string)
		}
		if urlValue != "" {
			appendImageString(result, seen, urlValue, firstString(typed, "mimeType", "mime_type"))
		}
	}
}

func appendImageString(result *[]ImageFingerprint, seen map[string]struct{}, value, mimeHint string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	if strings.HasPrefix(strings.ToLower(value), "data:") {
		mimeType, encoded, ok := parseDataURL(value)
		if !ok {
			return
		}
		if mimeType == "" {
			mimeType = mimeHint
		}
		appendBase64Image(result, seen, mimeType, encoded)
		return
	}
	parsed, errParse := url.Parse(value)
	if errParse != nil || parsed.Scheme == "" || parsed.Host == "" {
		return
	}
	parsed.Fragment = ""
	appendFingerprint(result, seen, ImageFingerprint{
		Key:      hashImageReference(parsed.String()),
		MIMEType: mimeHint,
	})
}

func appendBase64Image(result *[]ImageFingerprint, seen map[string]struct{}, mimeType, encoded string) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return
	}
	decoded, errDecode := base64.StdEncoding.DecodeString(encoded)
	if errDecode != nil {
		decoded, errDecode = base64.RawStdEncoding.DecodeString(encoded)
	}
	if errDecode != nil || len(decoded) == 0 {
		return
	}
	appendFingerprint(result, seen, ImageFingerprint{
		Key:      hashImageBytes(decoded),
		MIMEType: strings.TrimSpace(mimeType),
		Size:     len(decoded),
	})
}

func appendFingerprint(result *[]ImageFingerprint, seen map[string]struct{}, fingerprint ImageFingerprint) {
	if fingerprint.Key == "" {
		return
	}
	if _, exists := seen[fingerprint.Key]; exists {
		return
	}
	seen[fingerprint.Key] = struct{}{}
	*result = append(*result, fingerprint)
}

func parseDataURL(value string) (mimeType, encoded string, ok bool) {
	comma := strings.IndexByte(value, ',')
	if comma < 0 || !strings.HasPrefix(strings.ToLower(value), "data:") {
		return "", "", false
	}
	header := value[len("data:"):comma]
	if !strings.Contains(strings.ToLower(header), ";base64") {
		decoded, errDecode := url.QueryUnescape(value[comma+1:])
		if errDecode != nil {
			return "", "", false
		}
		return header, base64.StdEncoding.EncodeToString([]byte(decoded)), true
	}
	parts := strings.Split(header, ";")
	if len(parts) == 0 {
		return "", "", false
	}
	return parts[0], value[comma+1:], true
}

func hashImageBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return "image-sha256:" + hex.EncodeToString(digest[:])
}

func hashImageReference(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "image-url-sha256:" + hex.EncodeToString(digest[:])
}

func firstString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if current, ok := value[key].(string); ok && strings.TrimSpace(current) != "" {
			return strings.TrimSpace(current)
		}
	}
	return ""
}
