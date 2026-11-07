package jobs

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func saturatedWakeDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, DroppedInjectionsFile), make([]byte, maxDroppedFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDroppedSaturationTimestampMonotonic(t *testing.T) {
	dir := saturatedWakeDir(t)
	latest := time.Unix(1800000100, 0)
	noteDroppedInjection(dir, InjectStop, 10, latest)
	// An older hook can finish after a newer hook (or after the clock moved back).
	noteDroppedInjection(dir, InjectStop, 10, latest.Add(-time.Hour))
	if !droppedSaturated(dir, latest) {
		t.Fatal("an older loss hid the latest loss from the report window")
	}
	if droppedSaturated(dir, latest.Add(time.Second)) {
		t.Fatal("loss marker ignored the report window")
	}
}

func TestDroppedSaturationMarkerReplacedAtomically(t *testing.T) {
	dir := saturatedWakeDir(t)
	at := time.Unix(1800000000, 0)
	noteDroppedInjection(dir, InjectStop, 10, at)
	f, err := os.Open(filepath.Join(dir, SaturatedInjectionsFile))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	noteDroppedInjection(dir, InjectStop, 10, at.Add(time.Second))
	// A reader that already opened the marker must retain its complete old snapshot.
	b, err := io.ReadAll(f)
	if err != nil || string(b) != "1800000000\n" {
		t.Fatalf("marker was modified in place: %q, %v", b, err)
	}
	if !droppedSaturated(dir, at.Add(time.Second)) {
		t.Fatal("the replacement marker did not publish the latest loss")
	}
}

func TestDroppedSaturationConcurrentWritersKeepMaximum(t *testing.T) {
	dir := saturatedWakeDir(t)
	start := time.Unix(1800000000, 0)
	noteDroppedInjection(dir, InjectStop, 10, start)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			noteDroppedInjection(dir, InjectStop, 10, start.Add(time.Duration(i+1)*time.Second))
		}()
	}
	wg.Wait()
	b, err := os.ReadFile(filepath.Join(dir, SaturatedInjectionsFile))
	if err != nil || strings.TrimSpace(string(b)) != strconv.FormatInt(start.Unix()+32, 10) {
		t.Fatalf("concurrent writers lost the maximum timestamp: %q, %v", b, err)
	}
}
