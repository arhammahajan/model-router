// Clock is the single injectable time source for operational status.
//
// One app-owned process-relative epoch is shared by registry, workers and
// renderers: Mono values serialize against it and Wall values are absolute
// Unix seconds. Restored mono values may be negative. Nil *Mono means
// unknown/never; a real sampled zero is valid. One injected Clock.Sample
// returns a branded wall/mono pair so the two clocks can never mix.
package router

import "time"

// Mono is seconds on the app-owned process-relative epoch.
type Mono float64

// Wall is absolute Unix seconds.
type Wall float64

// Sample is one wall/mono pair from a single clock read.
type Sample struct {
	Mono Mono
	Wall Wall
}

// Clock returns a paired wall/mono sample. Implementations must derive
// both from one read so renderers pin one instant.
type Clock interface {
	Sample() Sample
}

// SystemClock is the production clock: mono since first use, wall as Unix
// time. The epoch is process-owned and shared by all users of this clock.
type SystemClock struct{}

var processEpoch = time.Now()

// Sample implements Clock.
func (SystemClock) Sample() Sample {
	now := time.Now()
	return Sample{
		Mono: Mono(now.Sub(processEpoch).Seconds()),
		Wall: Wall(float64(now.UnixNano()) / 1e9),
	}
}

// FakeClock is a hermetic clock for tests: samples return the set values.
type FakeClock struct {
	Mono Mono
	Wall Wall
}

// Sample implements Clock.
func (f *FakeClock) Sample() Sample { return Sample{Mono: f.Mono, Wall: f.Wall} }

// Advance moves both clocks forward by d seconds.
func (f *FakeClock) Advance(d float64) {
	f.Mono += Mono(d)
	f.Wall += Wall(d)
}
