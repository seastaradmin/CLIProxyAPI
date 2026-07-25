package util

import "testing"

func TestNormalizeUpstreamProtocols(t *testing.T) {
	got := NormalizeUpstreamProtocols(nil)
	if len(got) != 1 || got[0] != "chat" {
		t.Fatalf("default = %v", got)
	}
	got = NormalizeUpstreamProtocols([]string{"Responses", "messages", "chat", "responses", "bogus"})
	if stringsJoin(got) != "responses,messages,chat" {
		t.Fatalf("got %v", got)
	}
}

func TestHasProtocol(t *testing.T) {
	if !HasProtocol([]string{"chat", "responses"}, "openai-response") {
		t.Fatal("expected responses")
	}
	if HasProtocol([]string{"chat"}, "messages") {
		t.Fatal("did not expect messages")
	}
}

func stringsJoin(v []string) string {
	out := ""
	for i, s := range v {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}
