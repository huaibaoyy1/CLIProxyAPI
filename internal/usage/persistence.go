package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	usagePersistInterval = 30 * time.Second
	usagePersistTimeout  = 10 * time.Second
)

type usageSnapshotStore interface {
	LoadUsageSnapshot(ctx context.Context) ([]byte, error)
	PersistUsageSnapshot(ctx context.Context, content []byte) error
}

type snapshotPersistenceController struct {
	store usageSnapshotStore

	mu               sync.Mutex
	closed           bool
	stopCh           chan struct{}
	started          sync.Once
	dirty            bool
	lastPersistHash  [32]byte
	hasPersistedHash bool
}

type persistencePlugin struct {
	controller *snapshotPersistenceController
}

var (
	persistenceMu         sync.Mutex
	persistenceController *snapshotPersistenceController
	persistenceWarned     bool
)

// EnablePersistence enables PostgreSQL-backed persistence for usage statistics.
// Existing persisted statistics are loaded and merged into the in-memory snapshot
// before the background persistence plugin is registered.
func EnablePersistence(store usageSnapshotStore) error {
	if store == nil {
		return nil
	}

	persistenceMu.Lock()
	if persistenceController != nil {
		persistenceMu.Unlock()
		return nil
	}
	persistenceMu.Unlock()

	if err := loadPersistedSnapshot(store); err != nil {
		return err
	}

	controller := &snapshotPersistenceController{
		store:  store,
		stopCh: make(chan struct{}),
	}
	controller.Start()
	coreusage.RegisterPlugin(&persistencePlugin{controller: controller})

	persistenceMu.Lock()
	persistenceController = controller
	persistenceWarned = false
	persistenceMu.Unlock()

	log.Info("usage statistics persistence enabled with PostgreSQL backend")
	return nil
}

// DisablePersistence flushes any pending usage snapshot to the persistence backend
// and disables further persistence attempts.
func DisablePersistence() {
	persistenceMu.Lock()
	controller := persistenceController
	persistenceController = nil
	persistenceMu.Unlock()

	if controller != nil {
		controller.Close()
	}
}

// WarnPersistenceUnavailable logs a one-time warning when usage statistics are
// configured but PostgreSQL persistence is not enabled.
func WarnPersistenceUnavailable() {
	persistenceMu.Lock()
	if persistenceWarned {
		persistenceMu.Unlock()
		return
	}
	persistenceWarned = true
	persistenceMu.Unlock()

	log.Warn("usage statistics persistence unavailable: PostgreSQL not configured; statistics will remain in memory only")
}

func loadPersistedSnapshot(store usageSnapshotStore) error {
	ctx, cancel := context.WithTimeout(context.Background(), usagePersistTimeout)
	defer cancel()

	raw, err := store.LoadUsageSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("load persisted usage snapshot: %w", err)
	}

	result, err := mergePersistedSnapshotInto(GetRequestStatistics(), raw)
	if err != nil {
		return err
	}

	if result.Added > 0 || result.Skipped > 0 {
		log.Infof("loaded persisted usage statistics (added=%d skipped=%d)", result.Added, result.Skipped)
	}
	return nil
}

func mergePersistedSnapshotInto(stats *RequestStatistics, raw []byte) (MergeResult, error) {
	var zero MergeResult
	if stats == nil {
		return zero, nil
	}

	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return zero, nil
	}

	var snapshot StatisticsSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return zero, fmt.Errorf("decode persisted usage snapshot: %w", err)
	}

	return stats.MergeSnapshot(snapshot), nil
}

func persistSnapshot(store usageSnapshotStore, snapshot StatisticsSnapshot) error {
	if store == nil {
		return nil
	}

	raw, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("marshal usage snapshot: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), usagePersistTimeout)
	defer cancel()

	if err := store.PersistUsageSnapshot(ctx, raw); err != nil {
		return fmt.Errorf("persist usage snapshot: %w", err)
	}
	return nil
}

func (p *persistencePlugin) HandleUsage(_ context.Context, _ coreusage.Record) {
	if p == nil || p.controller == nil {
		return
	}
	p.controller.SchedulePersist()
}

func (c *snapshotPersistenceController) Start() {
	if c == nil {
		return
	}

	c.started.Do(func() {
		go c.runPeriodicPersist()
	})
}

func (c *snapshotPersistenceController) SchedulePersist() {
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return
	}

	c.dirty = true
}

func (c *snapshotPersistenceController) runPeriodicPersist() {
	if c == nil {
		return
	}

	ticker := time.NewTicker(usagePersistInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.persistNow()
		case <-c.stopCh:
			return
		}
	}
}

func (c *snapshotPersistenceController) persistNow() {
	if c == nil || c.store == nil {
		return
	}

	c.mu.Lock()
	dirty := c.dirty
	c.mu.Unlock()
	if !dirty {
		return
	}

	snapshot := GetRequestStatistics().Snapshot()
	raw, errMarshal := json.Marshal(snapshot)
	if errMarshal != nil {
		log.WithError(errMarshal).Warn("failed to marshal usage statistics snapshot")
		return
	}

	sum := sha256.Sum256(raw)

	c.mu.Lock()
	alreadyPersisted := c.hasPersistedHash && c.lastPersistHash == sum
	c.mu.Unlock()
	if alreadyPersisted {
		c.mu.Lock()
		c.dirty = false
		c.mu.Unlock()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), usagePersistTimeout)
	defer cancel()
	if errPersist := c.store.PersistUsageSnapshot(ctx, raw); errPersist != nil {
		log.WithError(errPersist).Warn("failed to persist usage statistics snapshot")
		return
	}

	c.mu.Lock()
	c.lastPersistHash = sum
	c.hasPersistedHash = true
	c.dirty = false
	c.mu.Unlock()
}

func (c *snapshotPersistenceController) Close() {
	if c == nil {
		return
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	stopCh := c.stopCh
	c.mu.Unlock()

	if stopCh != nil {
		close(stopCh)
	}

	c.persistNow()
}