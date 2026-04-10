package usage

import (
	"context"
	"sync"
	"testing"
)

type testUsageSnapshotStore struct {
	mu         sync.Mutex
	saveCount  int
	loadRaw    []byte
	persistErr error
}

func (s *testUsageSnapshotStore) LoadUsageSnapshot(context.Context) ([]byte, error) {
	return s.loadRaw, nil
}

func (s *testUsageSnapshotStore) PersistUsageSnapshot(context.Context, []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.persistErr != nil {
		return s.persistErr
	}
	s.saveCount++
	return nil
}

func (s *testUsageSnapshotStore) Saves() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveCount
}

func resetDefaultStatsForPersistenceTest(t *testing.T) {
	t.Helper()
	previous := defaultRequestStatistics
	defaultRequestStatistics = NewRequestStatistics()
	t.Cleanup(func() {
		defaultRequestStatistics = previous
	})
}

func TestSnapshotPersistenceController_PersistNowSkipsWhenNotDirty(t *testing.T) {
	resetDefaultStatsForPersistenceTest(t)

	store := &testUsageSnapshotStore{}
	controller := &snapshotPersistenceController{store: store}

	controller.persistNow()

	if got := store.Saves(); got != 0 {
		t.Fatalf("expected 0 persists when controller is not dirty, got %d", got)
	}
}

func TestSnapshotPersistenceController_PersistNowClearsDirtyOnSuccess(t *testing.T) {
	resetDefaultStatsForPersistenceTest(t)

	store := &testUsageSnapshotStore{}
	controller := &snapshotPersistenceController{store: store}

	controller.SchedulePersist()
	controller.persistNow()

	if got := store.Saves(); got != 1 {
		t.Fatalf("expected 1 persist after dirty snapshot, got %d", got)
	}

	controller.persistNow()
	if got := store.Saves(); got != 1 {
		t.Fatalf("expected no additional persist after dirty flag cleared, got %d", got)
	}
}

func TestSnapshotPersistenceController_PersistNowKeepsDirtyOnFailure(t *testing.T) {
	resetDefaultStatsForPersistenceTest(t)

	store := &testUsageSnapshotStore{persistErr: context.DeadlineExceeded}
	controller := &snapshotPersistenceController{store: store}

	controller.SchedulePersist()
	controller.persistNow()

	if got := store.Saves(); got != 0 {
		t.Fatalf("expected failed persist not to increment save count, got %d", got)
	}

	store.persistErr = nil
	controller.persistNow()

	if got := store.Saves(); got != 1 {
		t.Fatalf("expected retry to persist once after failure clears, got %d", got)
	}
}