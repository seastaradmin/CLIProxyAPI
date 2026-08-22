package handlers

import (
	"encoding/base64"
	"fmt"
	"testing"

	internalcache "github.com/router-for-me/CLIProxyAPI/v7/internal/cache"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestSetImageFingerprintMetadataTracksRepeatedImages(t *testing.T) {
	internalcache.ClearImageFingerprintCache()
	t.Cleanup(internalcache.ClearImageFingerprintCache)

	encoded := base64.StdEncoding.EncodeToString([]byte("codex-image"))
	body := []byte(fmt.Sprintf(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,%s"}]}]}`, encoded))
	meta := make(map[string]any)
	setImageFingerprintMetadata(meta, body)
	if got := meta[coreexecutor.ImageFingerprintCacheHitsMetadataKey]; got != 0 {
		t.Fatalf("first image cache hits = %v, want 0", got)
	}
	if got := meta[coreexecutor.ImageFingerprintCacheMissesMetadataKey]; got != 1 {
		t.Fatalf("first image cache misses = %v, want 1", got)
	}

	secondMeta := make(map[string]any)
	setImageFingerprintMetadata(secondMeta, body)
	if got := secondMeta[coreexecutor.ImageFingerprintCacheHitsMetadataKey]; got != 1 {
		t.Fatalf("second image cache hits = %v, want 1", got)
	}
	if got := secondMeta[coreexecutor.ImageFingerprintCacheMissesMetadataKey]; got != 0 {
		t.Fatalf("second image cache misses = %v, want 0", got)
	}
}

func TestSetImageFingerprintMetadataBypassesTextOnlyRequests(t *testing.T) {
	meta := make(map[string]any)
	setImageFingerprintMetadata(meta, []byte(`{"model":"deepseek-v4-flash","input":"hello"}`))
	if len(meta) != 0 {
		t.Fatalf("text-only metadata = %#v, want empty", meta)
	}
}
