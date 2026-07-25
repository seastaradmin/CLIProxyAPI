package config

import (
	"testing"
	"time"
)

func TestResolveResponseHeaderTimeout(t *testing.T) {
	if got := ResolveResponseHeaderTimeout(0); got != 20*time.Second {
		t.Fatalf("default = %v, want 20s", got)
	}
	if got := ResolveResponseHeaderTimeout(-1); got != 0 {
		t.Fatalf("disabled = %v, want 0", got)
	}
	if got := ResolveResponseHeaderTimeout(30); got != 30*time.Second {
		t.Fatalf("custom = %v, want 30s", got)
	}
}

func TestResolveFirstChunkTimeout(t *testing.T) {
	if got := ResolveFirstChunkTimeout(0); got != 30*time.Second {
		t.Fatalf("default = %v, want 30s", got)
	}
	if got := ResolveFirstChunkTimeout(-1); got != 0 {
		t.Fatalf("disabled = %v, want 0", got)
	}
	if got := ResolveFirstChunkTimeout(45); got != 45*time.Second {
		t.Fatalf("custom = %v, want 45s", got)
	}
}
