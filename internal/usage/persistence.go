package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	usagePersistDebounce = 2 * time.Second
	usagePersistTimeout  = 10 * time.Second
)

type usageSnapshotStore interface {
	LoadUsageSnapshot(ctx context.Context) ([]byte, error)
	PersistUsageSnapshot(ctx context.Context, content []byte) error
}

type snapshotPersistenceController struct {
	store usageSnapshotStore

	mu     sync.Mutex
	timer  *time.Timer
	closed bool
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

	controller := &snapshotPersistenceController{store: store}
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

func (c *snapshotPersistenceController) SchedulePersist() {
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return
	}

	if c.timer != nil {
		c.timer.Stop()
	}

	c.timer = time.AfterFunc(usagePersistDebounce, func() {
		c.persistNow()
	})
}

func (c *snapshotPersistenceController) persistNow() {
	if c == nil || c.store == nil {
		return
	}

	if err := persistSnapshot(c.store, GetRequestStatistics().Snapshot()); err != nil {
		log.WithError(err).Warn("failed to persist usage statistics snapshot")
	}
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
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.mu.Unlock()

	c.persistNow()
}