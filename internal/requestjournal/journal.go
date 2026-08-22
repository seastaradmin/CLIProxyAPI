// Package requestjournal provides durable, hard-deletable request call logs
// for the management availability UI. Records are stored as JSON Lines under
// the application log directory and are independent of the short-lived memory queue.
package requestjournal

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	subdirName       = "request-journal"
	defaultMaxDays   = 30
	defaultMaxFiles  = 90
	maxListLimit     = 500
	defaultListLimit = 100
)

// Record is one durable request log entry shown in the management UI.
type Record struct {
	ID           string         `json:"id"`
	Timestamp    time.Time      `json:"timestamp"`
	Provider     string         `json:"provider"`
	Model        string         `json:"model"`
	Alias        string         `json:"alias,omitempty"`
	Endpoint     string         `json:"endpoint,omitempty"`
	BaseURL      string         `json:"base_url,omitempty"`
	Protocol     string         `json:"protocol,omitempty"`
	Supplier     string         `json:"supplier,omitempty"`
	AuthType     string         `json:"auth_type,omitempty"`
	AuthIndex    string         `json:"auth_index,omitempty"`
	APIKey       string         `json:"api_key,omitempty"`
	RequestID    string         `json:"request_id,omitempty"`
	LatencyMs    int64          `json:"latency_ms"`
	TTFTMs       int64          `json:"ttft_ms"`
	Failed       bool           `json:"failed"`
	StatusCode   int            `json:"status_code"`
	FailureKind  string         `json:"failure_kind,omitempty"`
	ErrorBody    string         `json:"error_body,omitempty"`
	Source       string         `json:"source,omitempty"`
	Tokens       map[string]any `json:"tokens,omitempty"`
	ExecutorType string         `json:"executor_type,omitempty"`
}

var (
	mu      sync.Mutex
	rootDir string
	maxDays = defaultMaxDays
	enabled bool
	onceReg sync.Once
)

// Configure sets the journal root (typically the log directory) and retention.
func Configure(logDir string, keepDays int) {
	mu.Lock()
	defer mu.Unlock()
	rootDir = strings.TrimSpace(logDir)
	if keepDays > 0 {
		maxDays = keepDays
	} else {
		maxDays = defaultMaxDays
	}
	enabled = rootDir != ""
}

// Enable registers the usage plugin once and enables persistence.
func Enable(logDir string, keepDays int) {
	Configure(logDir, keepDays)
	onceReg.Do(func() {
		coreusage.RegisterNamedPlugin("request-journal", &plugin{})
	})
}

// Enabled reports whether the journal is active.
func Enabled() bool {
	mu.Lock()
	defer mu.Unlock()
	return enabled && rootDir != ""
}

type plugin struct{}

func (p *plugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if !Enabled() {
		return
	}
	rec := fromUsage(ctx, record)
	if err := appendRecord(rec); err != nil {
		log.WithError(err).Debug("request-journal: append failed")
	}
}

func fromUsage(ctx context.Context, record coreusage.Record) Record {
	ts := record.RequestedAt
	if ts.IsZero() {
		ts = time.Now()
	}
	reqID := strings.TrimSpace(internallogging.GetRequestID(ctx))
	endpoint := strings.TrimSpace(internallogging.GetEndpoint(ctx))
	id := reqID
	if id == "" {
		id = ts.UTC().Format("20060102T150405.000000000") + "-" + strings.TrimSpace(record.Provider) + "-" + strings.TrimSpace(record.AuthIndex) + "-" + strings.TrimSpace(record.Model)
	} else {
		// Ensure uniqueness if the same request id is retried.
		id = id + "-" + ts.UTC().Format("150405.000000")
	}

	status := 200
	errBody := ""
	if record.Failed {
		status = record.Fail.StatusCode
		if status <= 0 {
			status = 500
		}
		errBody = truncateJournalBody(strings.TrimSpace(record.Fail.Body))
	}
	failureKind := ""
	if record.Failed {
		if record.Fail.NetworkError {
			// Transport-level failure (DNS/dial/TLS/timeout): attribute it to
			// the network environment, not the credential.
			failureKind = "network"
		} else {
			failureKind = classifyFailure(status, errBody)
		}
	}

	tokens := map[string]any{}
	if record.Detail.InputTokens != 0 {
		tokens["input_tokens"] = record.Detail.InputTokens
	}
	if record.Detail.OutputTokens != 0 {
		tokens["output_tokens"] = record.Detail.OutputTokens
	}
	if record.Detail.ReasoningTokens != 0 {
		tokens["reasoning_tokens"] = record.Detail.ReasoningTokens
	}
	if record.Detail.CachedTokens != 0 {
		tokens["cached_tokens"] = record.Detail.CachedTokens
	}
	if record.Detail.TotalTokens != 0 {
		tokens["total_tokens"] = record.Detail.TotalTokens
	}
	if len(tokens) == 0 {
		tokens = nil
	}

	apiKey := maskSecret(record.APIKey)

	return Record{
		ID:           id,
		Timestamp:    ts,
		Provider:     strings.TrimSpace(record.Provider),
		Model:        strings.TrimSpace(record.Model),
		Alias:        strings.TrimSpace(record.Alias),
		Endpoint:     endpoint,
		BaseURL:      strings.TrimSpace(record.BaseURL),
		Protocol:     strings.TrimSpace(record.Protocol),
		Supplier:     strings.TrimSpace(record.Supplier),
		AuthType:     strings.TrimSpace(record.AuthType),
		AuthIndex:    strings.TrimSpace(record.AuthIndex),
		APIKey:       apiKey,
		RequestID:    strings.TrimSpace(reqID),
		LatencyMs:    record.Latency.Milliseconds(),
		TTFTMs:       record.TTFT.Milliseconds(),
		Failed:       record.Failed,
		StatusCode:   status,
		FailureKind:  failureKind,
		ErrorBody:    errBody,
		Source:       sanitizeSource(record.Source, record.AuthType),
		Tokens:       tokens,
		ExecutorType: strings.TrimSpace(record.ExecutorType),
	}
}

func journalDir() string {
	mu.Lock()
	defer mu.Unlock()
	if rootDir == "" {
		return ""
	}
	return filepath.Join(rootDir, subdirName)
}

func dayFile(t time.Time) string {
	return filepath.Join(journalDir(), t.UTC().Format("2006-01-02")+".jsonl")
}

func appendRecord(rec Record) error {
	dir := journalDir()
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := dayFile(rec.Timestamp)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	_, err = f.Write(raw)
	return err
}

// ListOptions controls listing.
type ListOptions struct {
	Limit      int
	Provider   string
	Supplier   string
	FailedOnly bool
	Before     time.Time // exclusive upper bound
}

// List returns newest-first records.
func List(opts ListOptions) ([]Record, error) {
	if !Enabled() {
		return nil, nil
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	dir := journalDir()
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	sort.Slice(files, func(i, j int) bool { return files[i] > files[j] }) // newest day first

	out := make([]Record, 0, limit)
	providerFilter := strings.TrimSpace(opts.Provider)
	supplierFilter := strings.TrimSpace(opts.Supplier)
	for _, path := range files {
		recs, errRead := readFile(path)
		if errRead != nil {
			continue
		}
		// newest last in file → reverse
		for i := len(recs) - 1; i >= 0; i-- {
			rec := recs[i]
			if !opts.Before.IsZero() && !rec.Timestamp.Before(opts.Before) {
				continue
			}
			if providerFilter != "" && !strings.EqualFold(rec.Provider, providerFilter) {
				continue
			}
			if supplierFilter != "" && !strings.EqualFold(rec.Supplier, supplierFilter) {
				continue
			}
			if opts.FailedOnly && !rec.Failed {
				continue
			}
			out = append(out, rec)
			if len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

func readFile(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec Record
		if errJSON := json.Unmarshal([]byte(line), &rec); errJSON != nil {
			continue
		}
		if strings.TrimSpace(rec.ID) == "" {
			continue
		}
		sanitizeRecord(&rec)
		out = append(out, rec)
	}
	return out, sc.Err()
}

func sanitizeRecord(rec *Record) {
	if rec == nil {
		return
	}
	rec.APIKey = maskSecret(rec.APIKey)
	rec.Source = sanitizeSource(rec.Source, rec.AuthType)
	if rec.Failed {
		if rec.StatusCode <= 0 {
			rec.StatusCode = 500
		}
		if strings.TrimSpace(rec.FailureKind) == "" {
			rec.FailureKind = classifyFailure(rec.StatusCode, rec.ErrorBody)
		}
	}
}

func classifyFailure(statusCode int, body string) string {
	if kind := coreauth.ClassifyUsageError(statusCode, "", body); kind != "" {
		return string(kind)
	}
	switch statusCode {
	case 499:
		return "client_cancelled"
	case 401, 403:
		return "authentication"
	case 400, 422:
		return "invalid_request"
	case 404:
		return "model_unavailable"
	case 408, 500, 502, 503, 504:
		return "upstream_error"
	case 0:
		return "unknown"
	default:
		return "request_error"
	}
}

func maskSecret(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 10 || strings.Contains(value, "****") {
		return value
	}
	return value[:4] + "****" + value[len(value)-4:]
}

func sanitizeSource(source, authType string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return source
	}
	lowerType := strings.ToLower(strings.TrimSpace(authType))
	if strings.Contains(lowerType, "api") || strings.HasPrefix(source, "sk-") || strings.HasPrefix(source, "sk_") || strings.HasPrefix(source, "gsk_") || strings.HasPrefix(source, "xai-") || strings.HasPrefix(source, "AIza") {
		return maskSecret(source)
	}
	// Emails and other non-key identifiers pass through unmasked.
	return source
}

// DeleteID hard-deletes one record by id across journal files.
func DeleteID(id string) (bool, error) {
	id = strings.TrimSpace(id)
	if id == "" || !Enabled() {
		return false, nil
	}
	dir := journalDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		deleted, errDel := rewriteWithoutID(path, id)
		if errDel != nil {
			return false, errDel
		}
		if deleted {
			return true, nil
		}
	}
	return false, nil
}

// DeleteAll hard-deletes every journal file.
func DeleteAll() (int, error) {
	if !Enabled() {
		return 0, nil
	}
	dir := journalDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if errRemove := os.Remove(path); errRemove == nil {
			n++
		}
	}
	return n, nil
}

func rewriteWithoutID(path, id string) (bool, error) {
	recs, err := readFile(path)
	if err != nil {
		return false, err
	}
	kept := make([]Record, 0, len(recs))
	found := false
	for _, rec := range recs {
		if rec.ID == id || rec.RequestID == id {
			found = true
			continue
		}
		kept = append(kept, rec)
	}
	if !found {
		return false, nil
	}
	if len(kept) == 0 {
		_ = os.Remove(path)
		return true, nil
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	w := bufio.NewWriter(f)
	for _, rec := range kept {
		raw, errJSON := json.Marshal(rec)
		if errJSON != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return false, errJSON
		}
		if _, errWrite := w.Write(raw); errWrite != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return false, errWrite
		}
		if errWrite := w.WriteByte('\n'); errWrite != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return false, errWrite
		}
	}
	if errFlush := w.Flush(); errFlush != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return false, errFlush
	}
	if errClose := f.Close(); errClose != nil {
		_ = os.Remove(tmp)
		return false, errClose
	}
	if errRename := os.Rename(tmp, path); errRename != nil {
		_ = os.Remove(tmp)
		return false, errRename
	}
	return true, nil
}

// PruneOld removes day files older than maxDays.
func PruneOld() {
	if !Enabled() {
		return
	}
	dir := journalDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -maxDays)
	count := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		day := strings.TrimSuffix(e.Name(), ".jsonl")
		ts, errParse := time.Parse("2006-01-02", day)
		if errParse != nil {
			continue
		}
		if ts.Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
			count++
		}
	}
	// also cap total file count
	entries, err = os.ReadDir(dir)
	if err != nil {
		return
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	for len(files) > defaultMaxFiles {
		_ = os.Remove(filepath.Join(dir, files[0]))
		files = files[1:]
	}
	_ = count
}

func truncateJournalBody(body string) string {
	const max = 2000
	if len(body) <= max {
		return body
	}
	// Prefer a short human-readable prefix for HTML diagnostic pages.
	lower := strings.ToLower(body)
	if strings.Contains(lower, "<html") || strings.Contains(lower, "<!doctype") {
		// Keep title if present.
		if i := strings.Index(lower, "<title>"); i >= 0 {
			j := strings.Index(lower[i:], "</title>")
			if j > 0 {
				return strings.TrimSpace(body[i:i+j+8]) + " …[html truncated]"
			}
		}
		return body[:400] + " …[html truncated]"
	}
	return body[:max] + "…[truncated]"
}
