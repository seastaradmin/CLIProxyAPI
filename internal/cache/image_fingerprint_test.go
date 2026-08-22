package cache

import (
	"encoding/base64"
	"fmt"
	"testing"
	"time"
)

func TestExtractImageFingerprintsDeduplicatesOpenAIDataURLs(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("same-image"))
	body := []byte(fmt.Sprintf(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}},{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]}]}`, encoded, encoded))

	fingerprints := ExtractImageFingerprints(body)
	if len(fingerprints) != 1 {
		t.Fatalf("ExtractImageFingerprints() count = %d, want 1", len(fingerprints))
	}
	if fingerprints[0].MIMEType != "image/png" {
		t.Fatalf("MIMEType = %q, want image/png", fingerprints[0].MIMEType)
	}
	if fingerprints[0].Size != len("same-image") {
		t.Fatalf("Size = %d, want %d", fingerprints[0].Size, len("same-image"))
	}
}

func TestExtractImageFingerprintsSupportsResponsesAndAnthropicImages(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("shared-image"))
	body := []byte(fmt.Sprintf(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/jpeg;base64,%s"}]}],"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"%s"}}]}]}`, encoded, encoded))

	fingerprints := ExtractImageFingerprints(body)
	if len(fingerprints) != 1 {
		t.Fatalf("ExtractImageFingerprints() count = %d, want 1", len(fingerprints))
	}
}

func TestExtractImageFingerprintsSkipsTextAndInvalidURLs(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"image_url is only text"},{"type":"input_image","image_url":"not-a-url"}]}`)
	if fingerprints := ExtractImageFingerprints(body); len(fingerprints) != 0 {
		t.Fatalf("ExtractImageFingerprints() = %+v, want no fingerprints", fingerprints)
	}
}

func TestImageFingerprintCacheObserveHitMissTTLAndEviction(t *testing.T) {
	now := time.Unix(100, 0)
	cache := NewImageFingerprintCache(2, time.Minute)
	cache.now = func() time.Time { return now }
	fingerprints := []ImageFingerprint{{Key: "a"}, {Key: "b"}}
	if hits, misses := cache.Observe(fingerprints); hits != 0 || misses != 2 {
		t.Fatalf("first Observe() = %d/%d, want 0/2", hits, misses)
	}
	if hits, misses := cache.Observe([]ImageFingerprint{{Key: "a"}}); hits != 1 || misses != 0 {
		t.Fatalf("second Observe() = %d/%d, want 1/0", hits, misses)
	}
	now = now.Add(2 * time.Minute)
	if hits, misses := cache.Observe([]ImageFingerprint{{Key: "a"}}); hits != 0 || misses != 1 {
		t.Fatalf("expired Observe() = %d/%d, want 0/1", hits, misses)
	}
	if hits, misses := cache.Observe([]ImageFingerprint{{Key: "c"}}); hits != 0 || misses != 1 {
		t.Fatalf("eviction Observe() = %d/%d, want 0/1", hits, misses)
	}
	if hits, misses := cache.Observe([]ImageFingerprint{{Key: "b"}}); hits != 0 || misses != 1 {
		t.Fatalf("oldest entry Observe() = %d/%d, want 0/1", hits, misses)
	}
}
