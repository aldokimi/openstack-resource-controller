/*
Copyright 2026 The ORC Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package dependency

import (
	"context"
	"fmt"
	"sync"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/k-orc/openstack-resource-controller/v2/internal/logging"
)

// GuardChecker is a function that checks if a dependency has any references.
// It returns true if there are references (blocking deletion), false if there are none (safe to delete).
type GuardChecker func(ctx context.Context, k8sClient client.Client, dep client.Object) (hasReferences bool, err error)

// guardKey uniquely identifies a set of guards that share the same finalizer and dependency type.
type guardKey struct {
	finalizer string
	depKind   string
}

// deletionGuardRegistry tracks all deletion guards by (finalizer, dependency kind) pairs.
// This allows coordination between multiple guards that use the same finalizer for the same dependency type.
type deletionGuardRegistry struct {
	mu      sync.RWMutex
	guards  map[guardKey][]GuardChecker
	loggers map[guardKey][]logr.Logger
}

var globalRegistry = &deletionGuardRegistry{
	guards:  make(map[guardKey][]GuardChecker),
	loggers: make(map[guardKey][]logr.Logger),
}

// RegisterGuard registers a deletion guard's checker function in the global registry.
// This allows the guard to participate in coordinated finalizer removal.
func RegisterGuard(finalizer, depKind string, checker GuardChecker, log logr.Logger) {
	key := guardKey{finalizer: finalizer, depKind: depKind}
	globalRegistry.mu.Lock()
	defer globalRegistry.mu.Unlock()
	globalRegistry.guards[key] = append(globalRegistry.guards[key], checker)
	globalRegistry.loggers[key] = append(globalRegistry.loggers[key], log)
}

// CheckAllGuards checks all registered guards for the given (finalizer, depKind) pair.
// Returns true if ANY guard has references (blocking deletion), false if ALL guards agree there are no references.
// If any guard's check fails with an error, returns true (fail-safe: don't remove finalizer on error).
func CheckAllGuards(ctx context.Context, k8sClient client.Client, dep client.Object, finalizer, depKind string) (hasReferences bool, err error) {
	key := guardKey{finalizer: finalizer, depKind: depKind}
	globalRegistry.mu.RLock()
	checkers := globalRegistry.guards[key]
	loggers := globalRegistry.loggers[key]
	globalRegistry.mu.RUnlock()

	if len(checkers) == 0 {
		// No guards registered for this key, safe to proceed with original behavior
		return false, nil
	}

	log := ctrl.LoggerFrom(ctx)
	log.V(logging.Verbose).Info("Checking all deletion guards for coordination", "finalizer", finalizer, "depKind", depKind, "guardCount", len(checkers))

	// Check all guards - if ANY has references, we must wait
	for i, checker := range checkers {
		guardLog := loggers[i]
		hasRefs, checkErr := checker(ctx, k8sClient, dep)
		if checkErr != nil {
			guardLog.Error(checkErr, "Error checking deletion guard, failing safe (not removing finalizer)")
			return true, fmt.Errorf("deletion guard check failed: %w", checkErr)
		}
		if hasRefs {
			guardLog.V(logging.Verbose).Info("Deletion guard has references, blocking finalizer removal")
			return true, nil
		}
	}

	log.V(logging.Verbose).Info("All deletion guards agree: no references, safe to remove finalizer")
	return false, nil
}

