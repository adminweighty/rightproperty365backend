package app

import (
	"sync/atomic"
	"time"

	"rightproperty365.com/backend/internal/platform/metrics"
)

// sweepMetrics receives the outcome of every instrumented sweep (API
// 0.61.0); nil (the default, and in tests) records nothing.
var sweepMetrics atomic.Pointer[metrics.Process]

// SetSweepMetrics makes the sweeps of this process report to p.
func SetSweepMetrics(p *metrics.Process) { sweepMetrics.Store(p) }

func sweepDone(name string, started time.Time, err error) {
	sweepMetrics.Load().SweepDone(name, started, err)
}
