// cpactl is a lightweight CLI for managing a running CPA (CLI Proxy API) instance
// via its Management API. Designed for use by personal Agents through shell tools.
//
// Usage:
//
//	cpactl [--api-base URL] [--key KEY] <command> [args...]
//
// Commands:
//
//	status                       Show server status and credential summary
//	providers list               List all openai-compatibility providers
//	providers probe <name>       Batch protocol probe for a provider
//	auth list                    List all auth credentials
//	auth health <name> <model>   Run health check on a credential
//	models                       List registered models
//	logs [--limit N] [--failed]  Show recent request journal
//	config get                   Print current config.yaml
//	config set <key> <value>     Update a config scalar value
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

var (
	apiBase = envOrDefault("CPA_API_BASE", "http://127.0.0.1:8888")
	apiKey  = envOrDefault("CPA_KEY", "admin")
)

func main() {
	args := os.Args[1:]

	// Parse global flags
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		switch {
		case args[0] == "--api-base" && len(args) > 1:
			apiBase = args[1]
			args = args[2:]
		case args[0] == "--key" && len(args) > 1:
			apiKey = args[1]
			args = args[2:]
		case strings.HasPrefix(args[0], "--api-base="):
			apiBase = strings.TrimPrefix(args[0], "--api-base=")
			args = args[1:]
		case strings.HasPrefix(args[0], "--key="):
			apiKey = strings.TrimPrefix(args[0], "--key=")
			args = args[1:]
		default:
			fatalf("unknown flag: %s", args[0])
		}
	}

	if len(args) == 0 {
		printUsage()
		os.Exit(0)
	}

	cmd := args[0]
	sub := args[1:]

	switch cmd {
	case "status":
		cmdStatus()
	case "providers":
		cmdProviders(sub)
	case "auth":
		cmdAuth(sub)
	case "models":
		cmdModels()
	case "logs":
		cmdLogs(sub)
	case "config":
		cmdConfig(sub)
	case "help", "-h", "--help":
		printUsage()
	default:
		fatalf("unknown command: %s\nRun 'cpactl help' for usage.", cmd)
	}
}

// --- Commands ---

func cmdStatus() {
	data := apiGet("/availability")
	var result map[string]any
	_ = json.Unmarshal(data, &result)
	printJSON(result)
}

func cmdProviders(args []string) {
	if len(args) == 0 {
		fatalf("usage: cpactl providers <list|probe> [args...]")
	}
	switch args[0] {
	case "list", "ls":
		data := apiGet("/openai-compatibility")
		printRawJSON(data)
	case "probe":
		if len(args) < 2 {
			fatalf("usage: cpactl providers probe <provider_name> [--base-url URL]")
		}
		name := args[1]
		baseURL := ""
		for i := 2; i < len(args); i++ {
			if args[i] == "--base-url" && i+1 < len(args) {
				baseURL = args[i+1]
				i++
			}
		}
		payload := map[string]any{
			"provider_name": name,
			"auto_apply":    true,
		}
		if baseURL != "" {
			payload["base_url"] = baseURL
		}
		data := apiPost("/protocol-probe-batch", payload)
		printRawJSON(data)
	default:
		fatalf("unknown providers subcommand: %s", args[0])
	}
}

func cmdAuth(args []string) {
	if len(args) == 0 {
		fatalf("usage: cpactl auth <list|health> [args...]")
	}
	switch args[0] {
	case "list", "ls":
		data := apiGet("/auth-files")
		printRawJSON(data)
	case "health":
		if len(args) < 3 {
			fatalf("usage: cpactl auth health <name> <model>")
		}
		name := args[1]
		model := args[2]
		payload := map[string]any{
			"name":  name,
			"model": model,
			"protocols": []string{"responses", "chat", "messages"},
		}
		data := apiPost("/auth-files/health-check", payload)
		printRawJSON(data)
	default:
		fatalf("unknown auth subcommand: %s", args[0])
	}
}

func cmdModels() {
	data := apiGet("/config")
	var cfg map[string]any
	_ = json.Unmarshal(data, &cfg)

	// Extract model-related info
	type modelEntry struct {
		Name     string `json:"name"`
		Provider string `json:"provider,omitempty"`
		Alias    string `json:"alias,omitempty"`
	}
	var models []modelEntry

	if compat, ok := cfg["openai-compatibility"].([]any); ok {
		for _, item := range compat {
			p, _ := item.(map[string]any)
			provName, _ := p["name"].(string)
			if ms, ok := p["models"].([]any); ok {
				for _, m := range ms {
					mm, _ := m.(map[string]any)
					name, _ := mm["name"].(string)
					alias, _ := mm["alias"].(string)
					if name != "" {
						models = append(models, modelEntry{Name: name, Provider: provName, Alias: alias})
					}
				}
			}
		}
	}

	printJSON(map[string]any{"models": models, "count": len(models)})
}

func cmdLogs(args []string) {
	limit := 20
	failed := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--limit", "-n":
			if i+1 < len(args) {
				limit, _ = strconv.Atoi(args[i+1])
				i++
			}
		case "--failed":
			failed = true
		}
	}
	path := fmt.Sprintf("/request-journal?limit=%d", limit)
	if failed {
		path += "&failed=true"
	}
	data := apiGet(path)
	printRawJSON(data)
}

func cmdConfig(args []string) {
	if len(args) == 0 {
		fatalf("usage: cpactl config <get|set> [args...]")
	}
	switch args[0] {
	case "get":
		data := apiGetRaw("/config.yaml")
		fmt.Print(string(data))
	case "set":
		if len(args) < 3 {
			fatalf("usage: cpactl config set <key> <value>\nExample: cpactl config set debug true")
		}
		// Read current config, modify, write back
		current := apiGetRaw("/config.yaml")
		key := args[1]
		value := args[2]
		// Simple line-based replacement for top-level keys
		lines := strings.Split(string(current), "\n")
		found := false
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, key+":") {
				lines[i] = key + ": " + value
				found = true
				break
			}
		}
		if !found {
			lines = append(lines, key+": "+value)
		}
		newConfig := strings.Join(lines, "\n")
		apiPutRaw("/config.yaml", []byte(newConfig))
		fmt.Fprintf(os.Stderr, "OK: %s = %s\n", key, value)
	default:
		fatalf("unknown config subcommand: %s", args[0])
	}
}

// --- HTTP Helpers ---

func apiGet(path string) []byte {
	url := strings.TrimRight(apiBase, "/") + "/v0/management" + path
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		fatalf("request error: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	return doRequest(req)
}

func apiGetRaw(path string) []byte {
	url := strings.TrimRight(apiBase, "/") + "/v0/management" + path
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		fatalf("request error: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	return doRequest(req)
}

func apiPost(path string, payload any) []byte {
	body, _ := json.Marshal(payload)
	url := strings.TrimRight(apiBase, "/") + "/v0/management" + path
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		fatalf("request error: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	return doRequest(req)
}

func apiPutRaw(path string, body []byte) {
	url := strings.TrimRight(apiBase, "/") + "/v0/management" + path
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		fatalf("request error: %v", err)
	}
	req.Header.Set("Content-Type", "application/yaml")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	doRequest(req)
}

func doRequest(req *http.Request) []byte {
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fatalf("HTTP error: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		fatalf("HTTP %d: %s", resp.StatusCode, truncate(string(data), 500))
	}
	return data
}

// --- Utilities ---

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func printRawJSON(data []byte) {
	var v any
	if json.Unmarshal(data, &v) == nil {
		printJSON(v)
	} else {
		fmt.Println(string(data))
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "cpactl: "+format+"\n", args...)
	os.Exit(1)
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func printUsage() {
	fmt.Print(`cpactl - CPA Management CLI

Usage: cpactl [--api-base URL] [--key KEY] <command> [args...]

Environment:
  CPA_API_BASE  API base URL (default: http://127.0.0.1:8888)
  CPA_KEY       Management key (default: admin)

Commands:
  status                         Show server status and credential summary
  providers list                 List all openai-compatibility providers
  providers probe <name>         Batch protocol probe for a provider
  auth list                      List all auth credentials
  auth health <name> <model>     Run health check on a credential
  models                         List registered models
  logs [--limit N] [--failed]    Show recent request journal
  config get                     Print current config.yaml
  config set <key> <value>       Update a top-level config value

Examples:
  cpactl status
  cpactl providers probe 火山
  cpactl auth health xai-seingelo.1800@hotmail.com.json grok-4.5
  cpactl logs --limit 5 --failed
  cpactl config set debug false
`)
}
