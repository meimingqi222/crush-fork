package config

import (
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSetDefaultsDoesNotMutateSharedDefaults guards the copy in setDefaults:
// appending straight to the package level defaultContextPaths returns that
// slice itself whenever Options.ContextPaths is empty, because a composite
// literal has cap == len. The Sort that follows would then reorder the shared
// backing array, racing across concurrent Init calls and corrupting the
// defaults for every later caller. Run under -race.
func TestSetDefaultsDoesNotMutateSharedDefaults(t *testing.T) {
	before := slices.Clone(defaultContextPaths)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = Init(t.TempDir(), "", false)
		}()
	}
	wg.Wait()
	require.Equal(t, before, defaultContextPaths, "package level defaultContextPaths was mutated")
}
