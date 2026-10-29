package rotation

import (
	"math"
	"runtime"
	"time"
)

// Trigger names what started a pass. It is logged and never stored.
type Trigger string

// Triggers of a pass.
const (
	// TriggerNightly: the nightly full pass (rotation.full_pass_time).
	TriggerNightly Trigger = "nightly"
	// TriggerButton: POST /api/track-plan/rebuild-full.
	TriggerButton Trigger = "button"
	// TriggerJob: the 5-minute short pass.
	TriggerJob Trigger = "job"
	// TriggerClockBuild: a planner rebuild after a clock, grid, or insert change.
	TriggerClockBuild Trigger = "clock_build"
)

// Names of the two passes in the logs (the "pass" attribute).
const (
	passShort = "short"
	passFull  = "full"
)

// memStats is a sample of runtime.MemStats. runtime.ReadMemStats stops the
// world, so a sample is taken at batch boundaries only, never per hour.
type memStats struct {
	heapAlloc, heapSys, totalAlloc uint64
	numGC                          uint32
}

func readMem() memStats {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return memStats{heapAlloc: m.HeapAlloc, heapSys: m.HeapSys, totalAlloc: m.TotalAlloc, numGC: m.NumGC}
}

// attrs returns the memory attributes of a log line. heap_alloc_mb and
// heap_sys_mb are live values; total_alloc_delta_mb (bytes allocated since
// base) and num_gc_delta (collections since base) measure the run or the
// batch; num_gc is the count since the process started.
func (m memStats) attrs(base memStats) []any {
	return []any{
		"heap_alloc_mb", mib(m.heapAlloc),
		"heap_sys_mb", mib(m.heapSys),
		"total_alloc_delta_mb", mib(m.totalAlloc - base.totalAlloc),
		"num_gc", m.numGC,
		"num_gc_delta", m.numGC - base.numGC,
	}
}

// mib converts bytes to MiB with one decimal.
func mib(b uint64) float64 { return math.Round(float64(b)/(1<<20)*10) / 10 }

// logPassStart logs the start of a pass that builds hours.
func (s *Service) logPassStart(pass string, trigger Trigger, from, to time.Time, toBuild, toSkip int, mem memStats) {
	args := []any{
		"pass", pass, "trigger", string(trigger),
		"range_from", from.UTC(), "range_to", to.UTC(),
		"hours_to_build", toBuild, "hours_to_skip", toSkip,
	}
	s.log.Info("track plan pass started", append(args, mem.attrs(mem)...)...)
}

// runTotals is what a pass logs at its end.
type runTotals struct {
	pass     string
	trigger  Trigger
	elapsed  time.Duration
	built    int
	skipped  int
	items    int
	byCode   map[WarningCode]int
	queries  int
	batches  int
	mem      memStats
	base     memStats // the sample at the start of the run
	slowMS   int
	heapWarn int
}

// logPassEnd logs the end of a pass, and a warning line when it ran longer
// than rotation.full_pass_slow_ms or held more heap than
// rotation.full_pass_heap_warn_mb. The warnings are by code; no track
// content is logged.
func (s *Service) logPassEnd(t runTotals) {
	codes := make(map[string]int, len(t.byCode))
	for code, n := range t.byCode {
		codes[string(code)] = n
	}
	args := []any{
		"pass", t.pass, "trigger", string(t.trigger),
		"duration_ms", t.elapsed.Milliseconds(),
		"hours_built", t.built, "hours_skipped", t.skipped, "items", t.items,
		"warnings_by_code", codes, "queries", t.queries, "batches", t.batches,
	}
	s.log.Info("track plan pass finished", append(args, t.mem.attrs(t.base)...)...)
	if t.slowMS > 0 && t.elapsed > time.Duration(t.slowMS)*time.Millisecond {
		s.log.Warn("track plan pass slow", "pass", t.pass, "duration_ms", t.elapsed.Milliseconds(),
			"threshold_ms", t.slowMS, "hours_built", t.built)
	}
	s.checkHeap(t.pass, t.mem, t.heapWarn)
}

// checkHeap warns when the heap in use is above the threshold in MiB.
func (s *Service) checkHeap(pass string, mem memStats, thresholdMB int) {
	if thresholdMB > 0 && mib(mem.heapAlloc) > float64(thresholdMB) {
		s.log.Warn("track plan heap above threshold", "pass", pass,
			"heap_alloc_mb", mib(mem.heapAlloc), "threshold_mb", thresholdMB)
	}
}

// logBatch logs the start or the end of a batch of a full pass, with the
// memory at that point and the allocation since the batch started (base).
func (s *Service) logBatch(msg string, index, batches int, from, to time.Time, built, skipped int, mem, base memStats) {
	args := []any{
		"batch", index, "batches", batches, "range_from", from.UTC(), "range_to", to.UTC(),
		"hours_built", built, "hours_skipped", skipped,
	}
	s.log.Info(msg, append(args, mem.attrs(base)...)...)
}
