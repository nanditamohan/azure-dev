// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package environment

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/azure/azure-dev/cli/azd/pkg/config"
	"github.com/azure/azure-dev/cli/azd/pkg/environment/azdcontext"
	"github.com/azure/azure-dev/cli/azd/test/mocks"
	"github.com/joho/godotenv"
	"github.com/stretchr/testify/require"
)

func TestPersistenceSnapshotAndReplace(t *testing.T) {
	t.Setenv("PROCESS_ONLY", "process")
	values := map[string]string{"VALUE": "01", "LD_PRELOAD": "preserved"}
	env := NewWithValues("original", values)
	values["VALUE"] = "outside"
	require.Equal(t, "01", env.Getenv("VALUE"))
	env.DotenvDelete("deleted")
	require.NoError(t, env.Config().Set("nested.value", "original"))
	view := env.Config()

	snapshot, err := env.SnapshotState()
	require.NoError(t, err)
	require.Equal(t, "preserved", snapshot.Dotenv["LD_PRELOAD"])
	require.NotContains(t, snapshot.Dotenv, "PROCESS_ONLY")
	require.NotContains(t, env.Dotenv(), "LD_PRELOAD")
	require.NotContains(t, env.Environ(), "LD_PRELOAD=preserved")
	snapshot.Dotenv["VALUE"] = "loaded"
	require.NoError(t, snapshot.Config.Set("nested.value", "loaded"))
	require.Equal(t, "01", env.Getenv("VALUE"))
	value, found := view.GetString("nested.value")
	require.True(t, found)
	require.Equal(t, "original", value)

	require.NoError(t, env.ReplaceState(snapshot))
	snapshot.Dotenv["VALUE"] = "changed after replace"
	require.NoError(t, snapshot.Config.Set("nested.value", "changed after replace"))
	require.Equal(t, "loaded", env.Getenv("VALUE"))
	value, found = view.GetString("nested.value")
	require.True(t, found)
	require.Equal(t, "loaded", value)
	require.Equal(t, "original", env.PersistenceName())
	require.Empty(t, env.deletedKeys)

	require.NoError(t, env.ReplaceState(PersistedState{}))
	env.DotenvSet("NEW", "value")
	require.Equal(t, "value", env.Getenv("NEW"))
	require.True(t, view.IsEmpty())
}

func TestReplaceStateWithEnvironmentConfigView(t *testing.T) {
	env := NewWithValues("test", map[string]string{"VALUE": "before"})
	retainedView := env.Config()
	require.NoError(t, retainedView.SetSecret("password", "unsaved-secret"))
	dotenv := map[string]string{"VALUE": "loaded"}
	done := make(chan error, 1)

	go func() {
		done <- env.ReplaceState(PersistedState{
			Dotenv: dotenv,
			Config: env.Config(),
		})
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("ReplaceState deadlocked while cloning its own config view")
	}

	dotenv["VALUE"] = "changed"
	require.Equal(t, "loaded", env.Getenv("VALUE"))
	password, exists := retainedView.GetString("password")
	require.True(t, exists)
	require.Equal(t, "unsaved-secret", password)
	require.NoError(t, retainedView.Set("retained", "value"))
	value, exists := env.Config().GetString("retained")
	require.True(t, exists)
	require.Equal(t, "value", value)
}

func TestPersistenceNameDoesNotResolveFallback(t *testing.T) {
	env := NewWithValues("", map[string]string{EnvNameEnvVarName: "fallback"})
	require.Empty(t, env.PersistenceName())
	require.Equal(t, "fallback", env.Name())
	require.Equal(t, "fallback", env.PersistenceName())
}

func TestReplaceStateRejectsInvalidConfigWithoutMutation(t *testing.T) {
	env := NewWithValues("test", map[string]string{"value": "memory"})
	env.DotenvDelete("deleted")
	require.NoError(t, env.Config().Set("value", "memory"))
	err := env.ReplaceState(PersistedState{
		Dotenv: map[string]string{"value": "loaded"},
		Config: config.NewConfig(map[string]any{"unsupported": make(chan string)}),
	})
	require.ErrorContains(t, err, "loading environment config")
	require.Equal(t, "memory", env.Getenv("value"))
	value, found := env.Config().GetString("value")
	require.True(t, found)
	require.Equal(t, "memory", value)
	require.Contains(t, env.deletedKeys, "deleted")
}

func TestMergeAndSave(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "failure then retry"
		}
		t.Run(name, func(t *testing.T) {
			env := NewWithValues("test", map[string]string{"shared": "memory", "memory": "01"})
			env.DotenvDelete("deleted")
			disk := map[string]string{"shared": "disk", "disk": "value", "deleted": "old"}
			want := map[string]string{"shared": "memory", "memory": "01", "disk": "value"}
			writeErr := errors.New("write failed")

			err := env.MergeAndSave(t.Context(), disk, func(_ context.Context, state PersistedState) error {
				require.Equal(t, want, state.Dotenv)
				// A writer cannot mutate the live state, even on success.
				state.Dotenv["shared"] = "writer mutation"
				require.NoError(t, state.Config.Set("writer", "mutation"))
				if fail {
					return writeErr
				}
				return nil
			})
			if fail {
				require.ErrorIs(t, err, writeErr)
				require.NotContains(t, env.Dotenv(), "disk")
				require.Contains(t, env.deletedKeys, "deleted")
				require.NoError(t, env.MergeAndSave(t.Context(), disk, func(_ context.Context, state PersistedState) error {
					require.Equal(t, want, state.Dotenv)
					return nil
				}))
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, want, env.Dotenv())
			require.True(t, env.Config().IsEmpty())
			require.Empty(t, env.deletedKeys)
			require.Equal(t, "old", disk["deleted"])

			// A successful save acknowledges tombstones; a later external value can return.
			require.NoError(t, env.MergeAndSave(t.Context(), disk, func(_ context.Context, state PersistedState) error {
				require.Equal(t, "old", state.Dotenv["deleted"])
				return nil
			}))
		})
	}
}

func TestMergeAndSaveSerializesConcurrentChanges(t *testing.T) {
	env := NewWithValues("test", map[string]string{"value": "before", "deleted": "before"})
	require.NoError(t, env.Config().Set("value", "before"))
	started := make(chan struct{})
	finished := make(chan error, 1)

	require.NoError(t, env.MergeAndSave(t.Context(), nil, func(_ context.Context, state PersistedState) error {
		// Assert the whole write callback, not just snapshot creation, owns the lock.
		if env.mu.TryLock() {
			env.mu.Unlock()
			t.Fatal("save released the environment lock before writing")
		}
		go func() {
			close(started)
			env.DotenvSet("value", "after")
			env.DotenvDelete("deleted")
			finished <- env.Config().Set("value", "after")
		}()
		<-started
		require.Equal(t, "before", state.Dotenv["value"])
		value, found := state.Config.GetString("value")
		require.True(t, found)
		require.Equal(t, "before", value)
		return nil
	}))
	require.NoError(t, <-finished)
	require.Equal(t, "after", env.Getenv("value"))
	require.NotContains(t, env.Dotenv(), "deleted")
	require.Contains(t, env.deletedKeys, "deleted")
	value, found := env.Config().GetString("value")
	require.True(t, found)
	require.Equal(t, "after", value)
}

// persistenceOnlyView exposes no variable methods: stores must use raw state.
type persistenceOnlyView struct {
	Persistence
}

func TestLocalPersistenceThroughView(t *testing.T) {
	ctx := t.Context()
	azdCtx := azdcontext.NewAzdContextWithDirectory(t.TempDir())
	store := NewLocalFileDataStore(azdCtx, config.NewFileConfigManager(config.NewManager()))
	raw := NewWithValues("test", map[string]string{
		"EXISTING_SERVICE_BUS_NAME": "orders",
		"LD_PRELOAD":                "preserved",
		"LEADING_ZERO":              "01",
	})
	view := persistenceOnlyView{raw}
	require.NoError(t, store.Save(ctx, view, nil))
	values, err := godotenv.Read(store.EnvPath(view))
	require.NoError(t, err)
	require.Equal(t, "preserved", values["LD_PRELOAD"])
	require.Equal(t, "01", values["LEADING_ZERO"])
	require.Equal(t, "orders", values["EXISTING_SERVICE_BUS_NAME"])
	require.NotContains(t, values, "SERVICE_BUS_NAME")

	values["EXISTING_SERVICE_BUS_NAME"] = "updated"
	require.NoError(t, godotenv.Write(values, store.EnvPath(view)))
	require.NoError(t, store.Reload(ctx, view))
	require.Equal(t, "updated", raw.Getenv("EXISTING_SERVICE_BUS_NAME"))
	require.NotContains(t, raw.Dotenv(), "LD_PRELOAD")
}

func TestLocalReloadFailurePreservesState(t *testing.T) {
	azdCtx := azdcontext.NewAzdContextWithDirectory(t.TempDir())
	store := NewLocalFileDataStore(azdCtx, config.NewFileConfigManager(config.NewManager()))
	env := New("test")
	env.DotenvSet("value", "memory")
	env.DotenvDelete("deleted")
	require.NoError(t, env.Config().Set("value", "memory"))
	require.NoError(t, os.MkdirAll(filepath.Dir(store.EnvPath(env)), 0700))
	require.NoError(t, os.WriteFile(store.EnvPath(env), []byte("value=disk\n"), 0600))
	require.NoError(t, os.WriteFile(store.ConfigPath(env), []byte("{invalid"), 0600))

	require.ErrorContains(t, store.Reload(t.Context(), env), "loading config")
	require.Equal(t, "memory", env.Getenv("value"))
	value, found := env.Config().GetString("value")
	require.True(t, found)
	require.Equal(t, "memory", value)
	require.Contains(t, env.deletedKeys, "deleted")
}

type interceptConfigManager struct {
	config.FileConfigManager
	beforeSave func(config.Config) error
}

func (m *interceptConfigManager) Save(cfg config.Config, path string) error {
	if m.beforeSave != nil {
		if err := m.beforeSave(cfg); err != nil {
			return err
		}
	}
	return m.FileConfigManager.Save(cfg, path)
}

func TestLocalSaveConcurrentMutationAndRetry(t *testing.T) {
	ctx := t.Context()
	azdCtx := azdcontext.NewAzdContextWithDirectory(t.TempDir())
	cfgManager := &interceptConfigManager{FileConfigManager: config.NewFileConfigManager(config.NewManager())}
	store := NewLocalFileDataStore(azdCtx, cfgManager)
	env := New("test")
	env.DotenvSet("value", "before")
	env.DotenvSet("deleted", "before")
	require.NoError(t, store.Save(ctx, env, nil))
	env.DotenvDelete("deleted")

	writeErr := errors.New("storage unavailable")
	cfgManager.beforeSave = func(config.Config) error { return writeErr }
	require.ErrorIs(t, store.Save(ctx, env, nil), writeErr)
	require.Contains(t, env.deletedKeys, "deleted")

	finished := make(chan error, 1)
	cfgManager.beforeSave = func(config.Config) error {
		if env.mu.TryLock() {
			env.mu.Unlock()
			t.Fatal("local store wrote outside the environment transaction")
		}
		started := make(chan struct{})
		go func() {
			close(started)
			env.DotenvSet("value", "after")
			finished <- env.Config().Set("late", "change")
		}()
		<-started
		return nil
	}
	require.NoError(t, store.Save(ctx, env, nil))
	require.NoError(t, <-finished)
	require.Equal(t, "after", env.Getenv("value"))
	disk, err := godotenv.Read(store.EnvPath(env))
	require.NoError(t, err)
	require.Equal(t, "before", disk["value"])
	require.NotContains(t, disk, "deleted")

	cfgManager.beforeSave = nil
	require.NoError(t, store.Save(ctx, env, nil))
	loaded, err := store.Get(ctx, "test")
	require.NoError(t, err)
	require.Equal(t, "after", loaded.Getenv("value"))
	late, found := loaded.Config().GetString("late")
	require.True(t, found)
	require.Equal(t, "change", late)
}

func TestManagerPersistenceViewRetainsCachedEnvironment(t *testing.T) {
	mockContext := mocks.NewMockContext(t.Context())
	manager, _ := createEnvManager(mockContext, t.TempDir())
	raw := New("test")
	require.NoError(t, manager.Save(t.Context(), raw))
	cached, err := manager.Get(t.Context(), "test")
	require.NoError(t, err)
	view := persistenceOnlyView{cached}
	configView := cached.Config()
	require.NoError(t, view.Config().Set("retained", "value"))
	require.NoError(t, manager.Save(t.Context(), view))
	require.NoError(t, manager.Reload(t.Context(), view))
	again, err := manager.Get(t.Context(), "test")
	require.NoError(t, err)
	require.Same(t, cached, again)
	value, found := configView.GetString("retained")
	require.True(t, found)
	require.Equal(t, "value", value)
}
