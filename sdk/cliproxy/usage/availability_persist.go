package usage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// availabilitySaveInterval is how often dirty in-memory samples are flushed to disk.
const availabilitySaveInterval = time.Minute

// persistedAvailability is the on-disk snapshot of the aggregator state.
type persistedAvailability struct {
	SavedAt time.Time                       `json:"saved_at"`
	Keys    map[string]AvailabilityKey      `json:"keys"`
	Samples map[string][]AvailabilitySample `json:"samples"`
}

// LoadFromFile restores previously persisted samples so availability metrics
// survive process restarts. Samples older than the retention window are dropped.
func (a *AvailabilityAggregator) LoadFromFile(path string) error {
	if a == nil || strings.TrimSpace(path) == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var state persistedAvailability
	if errUnmarshal := json.Unmarshal(data, &state); errUnmarshal != nil {
		return errUnmarshal
	}
	cutoff := time.Now().Add(-a.retention)
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, key := range state.Keys {
		samples := state.Samples[id]
		kept := make([]AvailabilitySample, 0, len(samples))
		for _, s := range samples {
			if s.At.Before(cutoff) {
				continue
			}
			kept = append(kept, s)
		}
		if len(kept) == 0 {
			continue
		}
		if len(kept) > maxSamplesPerKey {
			kept = kept[len(kept)-maxSamplesPerKey:]
		}
		a.keys[id] = key
		a.samples[id] = append(kept, a.samples[id]...)
	}
	return nil
}

// SaveToFile writes the current aggregator state atomically (tmp file + rename).
func (a *AvailabilityAggregator) SaveToFile(path string) error {
	if a == nil || strings.TrimSpace(path) == "" {
		return nil
	}
	a.mu.RLock()
	state := persistedAvailability{
		SavedAt: time.Now(),
		Keys:    make(map[string]AvailabilityKey, len(a.keys)),
		Samples: make(map[string][]AvailabilitySample, len(a.samples)),
	}
	for id, key := range a.keys {
		state.Keys[id] = key
		state.Samples[id] = append([]AvailabilitySample(nil), a.samples[id]...)
	}
	a.mu.RUnlock()
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o755); errMkdir != nil {
		return errMkdir
	}
	tmp := path + ".tmp"
	if errWrite := os.WriteFile(tmp, data, 0o600); errWrite != nil {
		return errWrite
	}
	return os.Rename(tmp, path)
}

// consumeDirty reports whether new samples arrived since the last flush and
// resets the flag.
func (a *AvailabilityAggregator) consumeDirty() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	dirty := a.dirty
	a.dirty = false
	return dirty
}

var (
	availPersistMu     sync.Mutex
	availPersistPath   string
	availPersistCancel context.CancelFunc
	availPersistDone   chan struct{}
)

// EnableDefaultAvailabilityPersistence loads the persisted snapshot and starts
// a background loop flushing the default aggregator to path. Repeated calls are no-ops.
func EnableDefaultAvailabilityPersistence(ctx context.Context, path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	availPersistMu.Lock()
	defer availPersistMu.Unlock()
	if availPersistPath != "" {
		return
	}
	if err := defaultAvailability.LoadFromFile(path); err != nil {
		log.Warnf("usage: failed to load availability state from %s: %v", path, err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	availPersistPath = path
	availPersistCancel = cancel
	availPersistDone = done
	go func() {
		defer close(done)
		ticker := time.NewTicker(availabilitySaveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
				if defaultAvailability.consumeDirty() {
					if err := defaultAvailability.SaveToFile(path); err != nil {
						log.Warnf("usage: failed to persist availability state: %v", err)
					}
				}
			}
		}
	}()
}

// ShutdownDefaultAvailabilityPersistence stops the flush loop and performs a final save.
func ShutdownDefaultAvailabilityPersistence() {
	availPersistMu.Lock()
	defer availPersistMu.Unlock()
	if availPersistPath == "" {
		return
	}
	if availPersistCancel != nil {
		availPersistCancel()
		availPersistCancel = nil
	}
	if availPersistDone != nil {
		<-availPersistDone
		availPersistDone = nil
	}
	if defaultAvailability.consumeDirty() {
		if err := defaultAvailability.SaveToFile(availPersistPath); err != nil {
			log.Warnf("usage: failed to persist availability state on shutdown: %v", err)
		}
	}
	availPersistPath = ""
}
