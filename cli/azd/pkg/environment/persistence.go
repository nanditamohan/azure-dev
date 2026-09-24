// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package environment

import (
	"context"
	"fmt"
	"maps"
	"os"

	"github.com/azure/azure-dev/cli/azd/internal/tracing"
	"github.com/azure/azure-dev/cli/azd/internal/tracing/fields"
	"github.com/azure/azure-dev/cli/azd/pkg/config"
	"github.com/google/uuid"
)

// PersistedState contains detached, raw environment data, without process values
// or provider mappings. Config retains unresolved secret references and vault data.
type PersistedState struct {
	Dotenv map[string]string
	Config config.Config
}

// Persistence provides raw state operations. Environment views must delegate these
// operations unchanged to their underlying environment.
type Persistence interface {
	// PersistenceName returns the stored name without fallback or mutation.
	PersistenceName() string
	// Config returns a synchronized, unmapped view of the current configuration.
	Config() config.Config
	SnapshotState() (PersistedState, error)
	// ReplaceState discards pending changes and installs loaded state in place.
	ReplaceState(PersistedState) error
	// MergeAndSave holds the environment lock through merge, write, and commit.
	// The callback must not call back into this environment or re-enter manager/local
	// store Save/Reload paths that reacquire manager.saveMu or the .env flock. It may
	// call config.FileConfigManager.Save with detached state.Config; that mutex is
	// acquired after the environment lock.
	MergeAndSave(context.Context, map[string]string, func(context.Context, PersistedState) error) error
}

// Env is an environment's variable view and raw persistence capability.
// A mapped view can translate variable access while delegating Persistence and Name.
type Env interface {
	Persistence
	Name() string
	Getenv(string) string
	LookupEnv(string) (string, bool)
	Dotenv() map[string]string
	DotenvSet(string, string)
	DotenvDelete(string)
	Environ() []string
	GetSubscriptionId() string
	SetSubscriptionId(string)
	GetTenantId() string
	GetLocation() string
	SetLocation(string)
	GetServiceProperty(string, string) string
	SetServiceProperty(string, string, string)
}

var _ Env = (*Environment)(nil)

// PersistenceName returns the stored name, without Name's fallback behavior.
func (e *Environment) PersistenceName() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.name
}

// SnapshotState returns a detached copy of raw dotenv and configuration.
func (e *Environment) SnapshotState() (PersistedState, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	cfg, err := config.Clone(e.config)
	if err != nil {
		return PersistedState{}, fmt.Errorf("snapshotting environment config: %w", err)
	}
	return PersistedState{Dotenv: maps.Clone(e.dotenv), Config: cfg}, nil
}

// ReplaceState installs a detached copy of loaded state and resets deletion
// tracking. It clones the config before acquiring mu because state.Config may be
// this environment's synchronized view, whose Clone method takes mu.RLock. The
// environment identity and any retained variable/config views remain intact.
func (e *Environment) ReplaceState(state PersistedState) error {
	cfg, err := config.Clone(state.Config)
	if err != nil {
		return fmt.Errorf("loading environment config: %w", err)
	}
	dotenv := make(map[string]string, len(state.Dotenv))
	maps.Copy(dotenv, state.Dotenv)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dotenv = dotenv
	e.config = cfg
	clear(e.deletedKeys)
	return nil
}

// MergeAndSave overlays current values and pending deletions on diskValues,
// writes a detached snapshot, and commits the merged dotenv only on success.
// Concurrent setters wait until completion, so their changes remain pending for
// the next save. The writer must follow [Persistence.MergeAndSave]'s lock
// restrictions and must not call back into this environment.
func (e *Environment) MergeAndSave(
	ctx context.Context,
	diskValues map[string]string,
	write func(context.Context, PersistedState) error,
) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	merged := make(map[string]string, len(diskValues)+len(e.dotenv))
	maps.Copy(merged, diskValues)
	maps.Copy(merged, e.dotenv)
	for key := range e.deletedKeys {
		delete(merged, key)
	}
	cfg, err := config.Clone(e.config)
	if err != nil {
		return fmt.Errorf("snapshotting environment config: %w", err)
	}
	if err := write(ctx, PersistedState{Dotenv: maps.Clone(merged), Config: cfg}); err != nil {
		return err
	}
	e.dotenv = merged
	clear(e.deletedKeys)
	return nil
}

func traceSavedName(name string) {
	tracing.SetUsageAttributes(fields.StringHashed(fields.EnvNameKey, name))
}

func traceLoadedState(name string, state PersistedState) {
	if name == "" {
		var found bool
		name, found = state.Dotenv[EnvNameEnvVarName]
		if !found {
			name = os.Getenv(EnvNameEnvVarName)
		}
	}
	if name != "" {
		traceSavedName(name)
	}
	subscription, found := state.Dotenv[SubscriptionIdEnvVarName]
	if !found {
		subscription = os.Getenv(SubscriptionIdEnvVarName)
	}
	if _, err := uuid.Parse(subscription); err == nil {
		tracing.SetGlobalAttributes(fields.SubscriptionIdKey.String(subscription))
	} else {
		tracing.SetGlobalAttributes(fields.StringHashed(fields.SubscriptionIdKey, subscription))
	}
}
