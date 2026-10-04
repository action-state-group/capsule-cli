package cli

import (
	"hash/fnv"
	"os"
	"testing"
)

// The race detector makes this package's single-goroutine case loops (random
// rewritings through the share gate, end-to-end runs over fixed case lists)
// about ten times slower and finds nothing in them. Under it, those loops run
// a fixed sample; without it (CI runs both) they run in full. Set
// CAPSULE_TEST_RACE_FULL=1 to run every case under the race detector too.

// raceStride keeps one case in raceStride under the race detector.
const raceStride = 8

func raceSampling() bool {
	return raceDetector && os.Getenv("CAPSULE_TEST_RACE_FULL") == ""
}

// raceRounds is n, or a tenth of it (at least one) under the race detector.
func raceRounds(n int) int {
	if raceSampling() {
		return max(1, n/10)
	}
	return n
}

// raceSample skips case i of a list unless it is in the race detector's
// sample. Case 0 always runs.
func raceSample(t *testing.T, i int) {
	t.Helper()
	if raceSampling() && i%raceStride != 0 {
		t.Skip("sampled under the race detector; every case runs without it")
	}
}

// raceSampleKey is raceSample for a case named by key (map iteration has no
// stable index): a fixed hash of the name decides.
func raceSampleKey(t *testing.T, key string) {
	t.Helper()
	h := fnv.New32a()
	h.Write([]byte(key))
	raceSample(t, int(h.Sum32()%raceStride))
}
