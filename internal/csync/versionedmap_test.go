package csync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestVersionedMap_Set(t *testing.T) {
	t.Parallel()

	vm := NewVersionedMap[string, int]()
	require.Equal(t, uint64(0), vm.Version())

	vm.Set("key1", 42)
	require.Equal(t, uint64(1), vm.Version())

	value, ok := vm.Get("key1")
	require.True(t, ok)
	require.Equal(t, 42, value)
}

func TestVersionedMap_Del(t *testing.T) {
	t.Parallel()

	vm := NewVersionedMap[string, int]()
	vm.Set("key1", 42)
	initialVersion := vm.Version()

	vm.Del("key1")
	require.Equal(t, initialVersion+1, vm.Version())

	_, ok := vm.Get("key1")
	require.False(t, ok)
}

func TestVersionedMap_VersionIncrement(t *testing.T) {
	t.Parallel()

	vm := NewVersionedMap[string, int]()
	initialVersion := vm.Version()

	// Setting a value should increment the version
	vm.Set("key1", 42)
	require.Equal(t, initialVersion+1, vm.Version())

	// Deleting a value should increment the version
	vm.Del("key1")
	require.Equal(t, initialVersion+2, vm.Version())

	// Deleting a non-existent key should still increment the version
	vm.Del("nonexistent")
	require.Equal(t, initialVersion+3, vm.Version())
}

func TestVersionedMap_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	vm := NewVersionedMap[int, int]()
	const numGoroutines = 100
	const numOperations = 100

	// Initial version
	initialVersion := vm.Version()

	// Perform concurrent Set and Del operations
	for i := range numGoroutines {
		go func(id int) {
			for j := range numOperations {
				key := id*numOperations + j
				vm.Set(key, key*2)
				vm.Del(key)
			}
		}(i)
	}

	// Wait for operations to complete by checking the version
	// This is a simplified check - in a real test you might want to use sync.WaitGroup
	expectedMinVersion := initialVersion + uint64(numGoroutines*numOperations*2)

	// Allow some time for operations to complete
	for vm.Version() < expectedMinVersion {
		// Busy wait - in a real test you'd use proper synchronization
	}

	// Final version should be at least the expected minimum
	require.GreaterOrEqual(t, vm.Version(), expectedMinVersion)
	require.Equal(t, 0, vm.Len())
}

func TestVersionedMap_WaitForChange_AlreadyChanged(t *testing.T) {
	t.Parallel()

	vm := NewVersionedMap[string, int]()
	baseline := vm.Version()
	vm.Set("key1", 42)

	ch := vm.WaitForChange(baseline)
	select {
	case <-ch:
		// Expected: channel already closed because version changed.
	default:
		t.Fatal("expected channel to be closed when version already changed")
	}
}

func TestVersionedMap_WaitForChange_WakesOnSet(t *testing.T) {
	t.Parallel()

	vm := NewVersionedMap[string, int]()
	baseline := vm.Version()
	ch := vm.WaitForChange(baseline)

	// Channel should not be closed yet.
	select {
	case <-ch:
		t.Fatal("channel should not be closed before a change")
	default:
	}

	// Trigger a change.
	vm.Set("key1", 42)

	// Channel should be closed now.
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for change notification")
	}
}

func TestVersionedMap_WaitForChange_WakesOnDel(t *testing.T) {
	t.Parallel()

	vm := NewVersionedMap[string, int]()
	vm.Set("key1", 42)
	baseline := vm.Version()
	ch := vm.WaitForChange(baseline)

	vm.Del("key1")

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for change notification")
	}
}

func TestVersionedMap_WaitForChange_MultipleWaiters(t *testing.T) {
	t.Parallel()

	vm := NewVersionedMap[string, int]()
	baseline := vm.Version()

	ch1 := vm.WaitForChange(baseline)
	ch2 := vm.WaitForChange(baseline)

	vm.Set("key1", 42)

	select {
	case <-ch1:
	case <-time.After(time.Second):
		t.Fatal("first waiter timed out")
	}
	select {
	case <-ch2:
	case <-time.After(time.Second):
		t.Fatal("second waiter timed out")
	}
}
