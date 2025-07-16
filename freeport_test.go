// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package freeport

import (
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestTakeReturn(t *testing.T) {
	// NOTE: for global var reasons this cannot execute in parallel
	// t.Parallel()

	// Since this test is destructive (i.e. it leaks all ports) it means that
	// any other test cases in this package will not function after it runs. To
	// help out we reset the global state after we run this test.
	defer reset()

	// OK: do a simple take/return cycle to trigger the package initialization
	func() {
		ports, err := Take(1)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		defer Return(ports)

		if len(ports) != 1 {
			t.Fatalf("expected %d but got %d ports", 1, len(ports))
		}
	}()

	waitForStatsReset := func() (numTotal int) {
		t.Helper()
		numTotal, numPending, numFree := stats()
		if numTotal != numFree+numPending {
			t.Fatalf("expected total (%d) and free+pending (%d) ports to match", numTotal, numFree+numPending)
		}
		assert.Eventually(t, func() bool {
			numTotal, numPending, numFree = stats()
			return numTotal == numFree && numPending == 0
		}, 5*time.Second, 100*time.Millisecond, "expected total (%d) and free (%d) ports to match", numTotal, numFree)

		return numTotal
	}

	// Reset
	numTotal := waitForStatsReset()

	// --------------------
	// OK: take the max
	func() {
		ports, err := Take(numTotal)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		defer Return(ports)

		if len(ports) != numTotal {
			t.Fatalf("expected %d but got %d ports", numTotal, len(ports))
		}
	}()

	// Reset
	numTotal = waitForStatsReset()

	expectError := func(expected string, got error) {
		t.Helper()
		if got == nil {
			t.Fatalf("expected error but was nil")
		}
		if got.Error() != expected {
			t.Fatalf("expected error %q but got %q", expected, got.Error())
		}
	}

	// --------------------
	// ERROR: take too many ports
	func() {
		ports, err := Take(numTotal + 1)
		defer Return(ports)
		expectError("freeport: block size too small", err)
	}()

	// --------------------
	// ERROR: invalid ports request (negative)
	func() {
		_, err := Take(-1)
		expectError("freeport: cannot take -1 ports", err)
	}()

	// --------------------
	// ERROR: invalid ports request (zero)
	func() {
		_, err := Take(0)
		expectError("freeport: cannot take 0 ports", err)
	}()

	// --------------------
	// OK: Steal a port under the covers and let freeport detect the theft and compensate
	leakedPort := peekFree()
	func() {
		leakyListener, err := net.ListenTCP("tcp", tcpAddr("127.0.0.1", leakedPort))
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		defer leakyListener.Close()

		func() {
			ports, err := Take(3)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			defer Return(ports)

			if len(ports) != 3 {
				t.Fatalf("expected %d but got %d ports", 3, len(ports))
			}

			for _, port := range ports {
				if port == leakedPort {
					t.Fatalf("did not expect for Take to return the leaked port")
				}
			}
		}()

		newNumTotal := waitForStatsReset()
		if newNumTotal != numTotal-1 {
			t.Fatalf("expected total to drop to %d but got %d", numTotal-1, newNumTotal)
		}
		numTotal = newNumTotal // update outer variable for later tests
	}()

	// --------------------
	// OK: sequence it so that one Take must wait on another Take to Return.
	func() {
		mostPorts, err := Take(numTotal - 5)
		if err != nil {
			t.Fatalf("err: %v", err)
		}

		type reply struct {
			ports []int
			err   error
		}
		ch := make(chan reply, 1)
		go func() {
			ports, err := Take(10)
			ch <- reply{ports: ports, err: err}
		}()

		Return(mostPorts)

		r := <-ch
		if r.err != nil {
			t.Fatalf("err: %v", r.err)
		}
		defer Return(r.ports)

		if len(r.ports) != 10 {
			t.Fatalf("expected %d ports but got %d", 10, len(r.ports))
		}
	}()

	// Reset
	numTotal = waitForStatsReset()

	// --------------------
	// ERROR: Now we end on the crazy "Ocean's 11" level port theft where we
	// orchestrate a situation where all ports are stolen and we don't find out
	// until Take.
	func() {
		// 1. Grab all of the ports.
		allPorts := peekAllFree()

		// 2. Leak all of the ports
		leaked := make([]io.Closer, 0, len(allPorts))
		defer func() {
			for _, c := range leaked {
				c.Close()
			}
		}()
		for i, port := range allPorts {
			ln, err := net.ListenTCP("tcp", tcpAddr("127.0.0.1", port))
			if err != nil {
				t.Fatalf("%d err: %v", i, err)
			}
			leaked = append(leaked, ln)
		}

		// 3. Request 1 port which will detect the leaked ports and fail.
		_, err := Take(1)
		expectError("freeport: impossible to satisfy request; there are no actual free ports in the block anymore", err)

		// 4. Wait for the block to zero out.
		newNumTotal := waitForStatsReset()
		if newNumTotal != 0 {
			t.Fatalf("expected total to drop to %d but got %d", 0, newNumTotal)
		}
	}()
}

func TestIntervalOverlap(t *testing.T) {
	cases := []struct {
		min1, max1, min2, max2 int
		overlap                bool
	}{
		{0, 0, 0, 0, true},
		{1, 1, 1, 1, true},
		{1, 3, 1, 3, true},  // same
		{1, 3, 4, 6, false}, // serial
		{1, 4, 3, 6, true},  // inner overlap
		{1, 6, 3, 4, true},  // nest
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d:%d vs %d:%d", tc.min1, tc.max1, tc.min2, tc.max2), func(t *testing.T) {
			if tc.overlap != intervalOverlap(tc.min1, tc.max1, tc.min2, tc.max2) { // 1 vs 2
				t.Fatalf("expected %v but got %v", tc.overlap, !tc.overlap)
			}
			if tc.overlap != intervalOverlap(tc.min2, tc.max2, tc.min1, tc.max1) { // 2 vs 1
				t.Fatalf("expected %v but got %v", tc.overlap, !tc.overlap)
			}
		})
	}
}

func TestLargePortAllocationHang(t *testing.T) {
	// NOTE: for global var reasons this cannot execute in parallel
	// t.Parallel()

	// Since this test is destructive (i.e. it leaks all ports) it means that
	// any other test cases in this package will not function after it runs. To
	// help out we reset the global state after we run this test.
	defer reset()

	// Initialize the system first with a simple take/return cycle
	func() {
		ports, err := Take(1)
		if err != nil {
			t.Fatalf("initialization failed: %v", err)
		}
		defer Return(ports)
	}()

	// Wait for stats to reset
	waitForStatsReset := func() (numTotal int) {
		t.Helper()
		numTotal, numPending, numFree := stats()
		if numTotal != numFree+numPending {
			t.Fatalf("expected total (%d) and free+pending (%d) ports to match", numTotal, numFree+numPending)
		}
		assert.Eventually(t, func() bool {
			numTotal, numPending, numFree = stats()
			return numTotal == numFree && numPending == 0
		}, 5*time.Second, 100*time.Millisecond, "expected total (%d) and free (%d) ports to match", numTotal, numFree)

		return numTotal
	}

	numTotal := waitForStatsReset()
	t.Logf("Total available ports: %d", numTotal)

	// Test case 1: Try to allocate exactly 127 ports (one less than blockSize)
	// This should work but might reveal timing issues
	t.Run("allocate_127_ports", func(t *testing.T) {
		if numTotal < 127 {
			t.Skipf("Not enough ports available (%d) to test 127 port allocation", numTotal)
		}

		done := make(chan struct{})
		var ports []int
		var err error

		// Use a goroutine with timeout to detect hanging
		go func() {
			defer close(done)
			ports, err = Take(127)
		}()

		// Wait for the allocation to complete with a timeout
		select {
		case <-done:
			if err != nil {
				t.Fatalf("Failed to allocate 127 ports: %v", err)
			}
			t.Logf("Successfully allocated %d ports", len(ports))
			Return(ports)
		case <-time.After(10 * time.Second):
			t.Fatalf("Take(127) hung for more than 10 seconds - this indicates the bug")
		}
	})

	// Reset
	numTotal = waitForStatsReset()

	// Test case 2: Try to allocate more than available ports in one shot
	t.Run("allocate_all_ports", func(t *testing.T) {
		want := numTotal + 1 // Requesting one more than available to trigger the bug
		// Use a goroutine with timeout to detect hanging

		ports, err := Take(want)
		assert.ErrorContains(t, err, "block size too small")
		Return(ports)
	})

	// Reset
	numTotal = waitForStatsReset()

	// Test case 3: Try to allocate more than available ports while some are in use
	// This scenario is most likely to trigger the hanging behavior
	t.Run("allocate_with_interference", func(t *testing.T) {
		heldPorts, err := Take(numTotal)
		assert.NoError(t, err, "Failed to take all ports")
		t.Cleanup(func() { Return(heldPorts) })
		// try to take one more than available and expect a timeout
		done := make(chan struct{})
		var ports []int
		go func() {
			defer close(done)
			ports, err = Take(1)
			t.Cleanup(func() { Return(ports) })
		}()
		// This should either succeed or fail quickly, not hang
		var hung bool
		select {
		case <-done:
			if err != nil {
				t.Logf("Expected failure when requesting %d ports with %d held: %v", 1, len(heldPorts), err)
			} else {
				t.Logf("Unexpectedly succeeded in allocating %d ports with %d held", len(ports), len(heldPorts))
				Return(ports)
			}
		case <-time.After(1 * time.Second):
			hung = true
		}
		assert.True(t, hung, "Take(%d) with %d ports held should have hung but did not", 1, len(heldPorts))
	})
}

func TestCLReservePortsEnvVar(t *testing.T) {
	// NOTE: for global var reasons this cannot execute in parallel
	// t.Parallel()

	// Since this test modifies global state, reset after it runs
	defer reset()

	testCases := []struct {
		name         string
		envValue     string
		expectedSize int
		shouldUseEnv bool
	}{
		{
			name:         "valid_small_block_size",
			envValue:     "128",
			expectedSize: 128,
			shouldUseEnv: true,
		},
		{
			name:         "valid_large_block_size",
			envValue:     "4096",
			expectedSize: 4096,
			shouldUseEnv: true,
		},
		{
			name:         "invalid_negative_value",
			envValue:     "-100",
			expectedSize: 2048, // should fall back to default
			shouldUseEnv: false,
		},
		{
			name:         "invalid_zero_value",
			envValue:     "0",
			expectedSize: 2048, // should fall back to default
			shouldUseEnv: false,
		},
		{
			name:         "invalid_non_numeric",
			envValue:     "not_a_number",
			expectedSize: 2048, // should fall back to default
			shouldUseEnv: false,
		},
		{
			name:         "empty_env_var",
			envValue:     "",
			expectedSize: 2048, // should use default
			shouldUseEnv: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Reset state before each test case
			reset()
			t.Setenv("CL_RESERVE_PORTS", tc.envValue)
			initialize()

			// Check the stats to verify the block size was applied correctly
			assert.Equal(t, blockSize, tc.expectedSize, "Expected total ports to match expected size")

		})
	}
}
