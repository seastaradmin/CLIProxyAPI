package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	modelProbeResultsFileName  = ".model-probe-results"
	modelProbeResultsVersion   = 1
	modelProbeResultsRetention = 30 * 24 * time.Hour
)

type modelProbeResult struct {
	AuthIndex  string    `json:"auth_index"`
	AuthID     string    `json:"auth_id,omitempty"`
	Provider   string    `json:"provider,omitempty"`
	Model      string    `json:"model"`
	Protocol   string    `json:"protocol"`
	Status     string    `json:"status"`
	StatusCode int       `json:"status_code,omitempty"`
	CheckedAt  time.Time `json:"checked_at"`
	LatencyMS  int64     `json:"latency_ms"`
	Error      string    `json:"error,omitempty"`
	ErrorType  string    `json:"error_type,omitempty"`
}

type modelProbeResultsFile struct {
	Version int                `json:"version"`
	Results []modelProbeResult `json:"results"`
}

// ListModelProbeResults returns recent active model probe evidence without
// changing credential health or routing state.
func (h *Handler) ListModelProbeResults(c *gin.Context) {
	results, errLoad := h.loadModelProbeResults(c.Request.Context())
	if errLoad != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to load model probe results: %v", errLoad)})
		return
	}

	authIndex := strings.TrimSpace(c.Query("auth_index"))
	model := strings.TrimSpace(c.Query("model"))
	if authIndex != "" || model != "" {
		filtered := make([]modelProbeResult, 0, len(results))
		for _, result := range results {
			if authIndex != "" && result.AuthIndex != authIndex {
				continue
			}
			if model != "" && !strings.EqualFold(result.Model, model) {
				continue
			}
			filtered = append(filtered, result)
		}
		results = filtered
	}

	c.JSON(http.StatusOK, gin.H{
		"results":        results,
		"retention_days": int(modelProbeResultsRetention / (24 * time.Hour)),
	})
}

func (h *Handler) persistModelProbeResults(ctx context.Context, auth *coreauth.Auth, model string, checkedAt time.Time, results []authFileHealthProtocolResult) error {
	if h == nil || auth == nil || len(results) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errContext := ctx.Err(); errContext != nil {
		return errContext
	}

	auth.EnsureIndex()
	authIndex := strings.TrimSpace(auth.Index)
	model = strings.TrimSpace(model)
	if authIndex == "" || model == "" {
		return errors.New("auth_index and model are required")
	}
	if checkedAt.IsZero() {
		checkedAt = time.Now().UTC()
	}

	h.modelProbeResultsMu.Lock()
	defer h.modelProbeResultsMu.Unlock()

	existing, errLoad := h.loadModelProbeResultsLocked(ctx, checkedAt)
	if errLoad != nil {
		return errLoad
	}
	byKey := make(map[string]modelProbeResult, len(existing)+len(results))
	for _, result := range existing {
		byKey[modelProbeResultKey(result.AuthIndex, result.Model, result.Protocol)] = result
	}
	for _, protocolResult := range results {
		protocol := strings.ToLower(strings.TrimSpace(protocolResult.Protocol))
		if protocol == "" {
			continue
		}
		result := modelProbeResult{
			AuthIndex:  authIndex,
			AuthID:     strings.TrimSpace(auth.ID),
			Provider:   strings.TrimSpace(auth.Provider),
			Model:      model,
			Protocol:   protocol,
			Status:     strings.TrimSpace(protocolResult.Status),
			StatusCode: protocolResult.StatusCode,
			CheckedAt:  checkedAt.UTC(),
			LatencyMS:  protocolResult.LatencyMS,
			Error:      limitHealthText(protocolResult.Error),
			ErrorType:  strings.TrimSpace(protocolResult.ErrorType),
		}
		byKey[modelProbeResultKey(authIndex, model, protocol)] = result
	}

	next := make([]modelProbeResult, 0, len(byKey))
	for _, result := range byKey {
		next = append(next, result)
	}
	sortModelProbeResults(next)
	return h.writeModelProbeResultsLocked(ctx, next)
}

func (h *Handler) loadModelProbeResults(ctx context.Context) ([]modelProbeResult, error) {
	if h == nil {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.modelProbeResultsMu.Lock()
	defer h.modelProbeResultsMu.Unlock()
	return h.loadModelProbeResultsLocked(ctx, time.Now().UTC())
}

func (h *Handler) loadModelProbeResultsLocked(ctx context.Context, now time.Time) ([]modelProbeResult, error) {
	if errContext := ctx.Err(); errContext != nil {
		return nil, errContext
	}
	path, errPath := h.modelProbeResultsPath()
	if errPath != nil {
		return nil, errPath
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		if errors.Is(errRead, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read model probe results: %w", errRead)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	var envelope modelProbeResultsFile
	if errUnmarshal := json.Unmarshal(data, &envelope); errUnmarshal != nil {
		return nil, fmt.Errorf("parse model probe results: %w", errUnmarshal)
	}

	cutoff := now.Add(-modelProbeResultsRetention)
	results := make([]modelProbeResult, 0, len(envelope.Results))
	for _, result := range envelope.Results {
		if result.CheckedAt.IsZero() || result.CheckedAt.Before(cutoff) {
			continue
		}
		if strings.TrimSpace(result.AuthIndex) == "" || strings.TrimSpace(result.Model) == "" || strings.TrimSpace(result.Protocol) == "" {
			continue
		}
		results = append(results, result)
	}
	sortModelProbeResults(results)
	return results, nil
}

func (h *Handler) writeModelProbeResultsLocked(ctx context.Context, results []modelProbeResult) error {
	if errContext := ctx.Err(); errContext != nil {
		return errContext
	}
	path, errPath := h.modelProbeResultsPath()
	if errPath != nil {
		return errPath
	}
	payload, errMarshal := json.MarshalIndent(modelProbeResultsFile{
		Version: modelProbeResultsVersion,
		Results: results,
	}, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("marshal model probe results: %w", errMarshal)
	}
	payload = append(payload, '\n')

	dir := filepath.Dir(path)
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return fmt.Errorf("create model probe results directory: %w", errMkdir)
	}
	tmpFile, errCreate := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if errCreate != nil {
		return fmt.Errorf("create model probe results temp file: %w", errCreate)
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if errChmod := tmpFile.Chmod(0o600); errChmod != nil {
		if errClose := tmpFile.Close(); errClose != nil {
			return fmt.Errorf("set model probe results permissions: %w; close temp file: %v", errChmod, errClose)
		}
		return fmt.Errorf("set model probe results permissions: %w", errChmod)
	}
	if _, errWrite := tmpFile.Write(payload); errWrite != nil {
		if errClose := tmpFile.Close(); errClose != nil {
			return fmt.Errorf("write model probe results: %w; close temp file: %v", errWrite, errClose)
		}
		return fmt.Errorf("write model probe results: %w", errWrite)
	}
	if errClose := tmpFile.Close(); errClose != nil {
		return fmt.Errorf("close model probe results temp file: %w", errClose)
	}
	if errRename := os.Rename(tmpPath, path); errRename != nil {
		return fmt.Errorf("replace model probe results file: %w", errRename)
	}
	return nil
}

func (h *Handler) modelProbeResultsPath() (string, error) {
	h.mu.Lock()
	authDir := ""
	if h.cfg != nil {
		authDir = strings.TrimSpace(h.cfg.AuthDir)
	}
	h.mu.Unlock()
	resolved, errResolve := util.ResolveAuthDir(authDir)
	if errResolve != nil {
		return "", errResolve
	}
	return filepath.Join(resolved, modelProbeResultsFileName), nil
}

func modelProbeResultKey(authIndex, model, protocol string) string {
	return strings.TrimSpace(authIndex) + "\x1f" + strings.ToLower(strings.TrimSpace(model)) + "\x1f" + strings.ToLower(strings.TrimSpace(protocol))
}

func sortModelProbeResults(results []modelProbeResult) {
	sort.Slice(results, func(i, j int) bool {
		if !results[i].CheckedAt.Equal(results[j].CheckedAt) {
			return results[i].CheckedAt.After(results[j].CheckedAt)
		}
		return modelProbeResultKey(results[i].AuthIndex, results[i].Model, results[i].Protocol) < modelProbeResultKey(results[j].AuthIndex, results[j].Model, results[j].Protocol)
	})
}
