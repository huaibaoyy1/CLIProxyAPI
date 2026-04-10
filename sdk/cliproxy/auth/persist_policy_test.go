package auth

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type countingStore struct {
	saveCount atomic.Int32
}

func (s *countingStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *countingStore) Save(context.Context, *Auth) (string, error) {
	s.saveCount.Add(1)
	return "", nil
}

func (s *countingStore) Delete(context.Context, string) error { return nil }

func TestWithSkipPersist_DisablesUpdatePersistence(t *testing.T) {
	store := &countingStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "auth-1",
		Provider: "antigravity",
		Metadata: map[string]any{"type": "antigravity"},
	}

	if _, err := mgr.Update(context.Background(), auth); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if got := store.saveCount.Load(); got != 1 {
		t.Fatalf("expected 1 Save call, got %d", got)
	}

	ctxSkip := WithSkipPersist(context.Background())
	if _, err := mgr.Update(ctxSkip, auth); err != nil {
		t.Fatalf("Update(skipPersist) returned error: %v", err)
	}
	if got := store.saveCount.Load(); got != 1 {
		t.Fatalf("expected Save call count to remain 1, got %d", got)
	}
}

func TestWithSkipPersist_DisablesRegisterPersistence(t *testing.T) {
	store := &countingStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "auth-1",
		Provider: "antigravity",
		Metadata: map[string]any{"type": "antigravity"},
	}

	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register(skipPersist) returned error: %v", err)
	}
	if got := store.saveCount.Load(); got != 0 {
		t.Fatalf("expected 0 Save calls, got %d", got)
	}
}

type persistPolicyTestExecutor struct {
	id        string
	shouldErr bool
}

func (e *persistPolicyTestExecutor) Identifier() string {
	return e.id
}

func (e *persistPolicyTestExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e.shouldErr {
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusInternalServerError, Message: "boom"}
	}
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func (e *persistPolicyTestExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "not implemented"}
}

func (e *persistPolicyTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *persistPolicyTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusNotImplemented, Message: "not implemented"}
}

func (e *persistPolicyTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestExecute_DoesNotPersistRequestPath_Success(t *testing.T) {
	store := &countingStore{}
	mgr := NewManager(store, nil, nil)

	executor := &persistPolicyTestExecutor{id: "claude", shouldErr: false}
	mgr.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "claude",
		Metadata: map[string]any{"type": "oauth"},
	}
	if _, err := mgr.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	model := "test-model"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	defer reg.UnregisterClient(auth.ID)

	savesAfterRegister := store.saveCount.Load()

	if _, err := mgr.Execute(context.Background(), []string{auth.Provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if got := store.saveCount.Load(); got != savesAfterRegister {
		t.Fatalf("expected request path to skip persistence, save count %d -> %d", savesAfterRegister, got)
	}
}

func TestExecute_DoesNotPersistRequestPath_Failure(t *testing.T) {
	store := &countingStore{}
	mgr := NewManager(store, nil, nil)

	executor := &persistPolicyTestExecutor{id: "claude", shouldErr: true}
	mgr.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "claude",
		Metadata: map[string]any{"type": "oauth"},
	}
	if _, err := mgr.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	model := "test-model"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	defer reg.UnregisterClient(auth.ID)

	savesAfterRegister := store.saveCount.Load()

	if _, err := mgr.Execute(context.Background(), []string{auth.Provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); err == nil {
		t.Fatalf("expected Execute to return error")
	}

	if got := store.saveCount.Load(); got != savesAfterRegister {
		t.Fatalf("expected failed request path to skip persistence, save count %d -> %d", savesAfterRegister, got)
	}
}