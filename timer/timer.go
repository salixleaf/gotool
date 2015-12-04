/*
 * yubo@yubo.org
 * 2015-12-04
 */
package timer

/*
#include "timer.h"
*/
import "C"
import (
	"sync"
	"time"
)

type Timer struct {
	i int // heap index

	// Timer wakes up at when, and then at when+period, ... (period > 0 only)
	// each time calling f(now, arg) in the timer goroutine, so f must be
	// a well-behaved function and not block.
	when   int64
	period int64
	expiry int64
	f      func(interface{})
	arg    interface{}
}

var timers struct {
	T            *time.Timer
	lock         sync.Mutex
	t            []*Timer
	created      bool
	sleeping     bool
	rescheduling bool
	sleep        chan bool
	reschedule   chan bool
}

// when is a helper function for setting the 'when' field of a runtimeTimer.
// It returns what the time will be, in nanoseconds, Duration d in the future.
// If d is negative, it is ignored.  If the returned value would be less than
// zero because of an overflow, MaxInt64 is returned.
func when(d time.Duration) int64 {
	if d <= 0 {
		return Nanotime()
	}
	t := Nanotime() + int64(d)
	if t < 0 {
		t = 1<<63 - 1 // math.MaxInt64
	}
	return t
}

// startTimer adds t to the timer heap.
func startTimer(t *Timer) {
	addtimer(t)
}

// stopTimer removes t from the timer heap if it is there.
// It returns true if t was removed, false if t wasn't even there.
func stopTimer(t *Timer) bool {
	return deltimer(t)
}

func addtimer(t *Timer) {
	timers.lock.Lock()
	addtimerLocked(t)
	timers.lock.Unlock()
}

// Add a timer to the heap and start or kick the timer proc.
// If the new timer is earlier than any of the others.
// Timers are locked.
func addtimerLocked(t *Timer) {
	// when must never be negative; otherwise timerproc will overflow
	// during its delta calculation and never expire other runtime·timers.
	if t.when < 0 {
		t.when = 1<<63 - 1
	}
	t.i = len(timers.t)
	timers.t = append(timers.t, t)
	siftupTimer(t.i)
	if t.i == 0 {
		// siftup moved to top: new earliest deadline.
		if timers.sleeping {
			timers.sleeping = false
			select {
			case timers.sleep <- timers.sleeping:
			default:
			}
		}
		/*
			if timers.rescheduling {
				timers.rescheduling = false
				select {
				case timers.reschedule <- timers.rescheduling:
				default:
				}
			}
		*/
	}
	if !timers.created {
		timers.created = true
		timers.sleep = make(chan bool, 1)
		timers.reschedule = make(chan bool, 1)
		timers.T = time.NewTimer(time.Duration(t.when - Nanotime()))
		go timerproc()
	}
}

// Delete timer t from the heap.
// Do not need to update the timerproc: if it wakes up early, no big deal.
func deltimer(t *Timer) bool {
	// Dereference t so that any panic happens before the lock is held.
	// Discard result, because t might be moving in the heap.
	_ = t.i

	timers.lock.Lock()
	// t may not be registered anymore and may have
	// a bogus i (typically 0, if generated by Go).
	// Verify it before proceeding.
	i := t.i
	last := len(timers.t) - 1
	if i < 0 || i > last || timers.t[i] != t {
		timers.lock.Unlock()
		return false
	}
	if i != last {
		timers.t[i] = timers.t[last]
		timers.t[i].i = i
	}
	timers.t[last] = nil
	timers.t = timers.t[:last]
	if i != last {
		siftupTimer(i)
		siftdownTimer(i)
	}
	timers.lock.Unlock()
	return true
}

// Timerproc runs the time-driven events.
// It sleeps until the next event in the timers heap.
// If addtimer inserts a new earlier event, addtimer1 wakes timerproc early.
func timerproc() {
	for {
		timers.lock.Lock()
		now := Nanotime()
		delta := int64(-1)
		for {
			if len(timers.t) == 0 {
				delta = -1
				break
			}
			t := timers.t[0]
			delta = t.when - now
			if delta > 0 {
				break
			}
			if t.period > 0 && (t.expiry == 0 || t.when < t.expiry) {
				// leave in heap but adjust next time to fire
				t.when += t.period * (1 + -delta/t.period)
				siftdownTimer(0)
			} else {
				// remove from heap
				last := len(timers.t) - 1
				if last > 0 {
					timers.t[0] = timers.t[last]
					timers.t[0].i = 0
				}
				timers.t[last] = nil
				timers.t = timers.t[:last]
				if last > 0 {
					siftdownTimer(0)
				}
				t.i = -1 // mark as removed
			}
			f := t.f
			arg := t.arg
			timers.lock.Unlock()
			f(arg)
			timers.lock.Lock()
		}
		if delta < 0 {
			// No timers left - put goroutine to sleep.
			//timers.rescheduling = true
			timers.sleeping = true
			<-timers.sleep
			continue
		}
		// At least one timer pending.  Sleep until then.
		timers.sleeping = true
		timers.T.Reset(time.Duration(delta))
		timers.lock.Unlock()
		select {
		case <-timers.sleep:
		case <-timers.T.C:
		}
	}
}

// Heap maintenance algorithms.

func siftupTimer(i int) {
	t := timers.t
	when := t[i].when
	tmp := t[i]
	for i > 0 {
		p := (i - 1) / 4 // parent
		if when >= t[p].when {
			break
		}
		t[i] = t[p]
		t[i].i = i
		t[p] = tmp
		t[p].i = p
		i = p
	}
}

func siftdownTimer(i int) {
	t := timers.t
	n := len(t)
	when := t[i].when
	tmp := t[i]
	for {
		c := i*4 + 1 // left child
		c3 := c + 2  // mid child
		if c >= n {
			break
		}
		w := t[c].when
		if c+1 < n && t[c+1].when < w {
			w = t[c+1].when
			c++
		}
		if c3 < n {
			w3 := t[c3].when
			if c3+1 < n && t[c3+1].when < w3 {
				w3 = t[c3+1].when
				c3++
			}
			if w3 < w {
				w = w3
				c = c3
			}
		}
		if w >= when {
			break
		}
		t[i] = t[c]
		t[i].i = i
		t[c] = tmp
		t[c].i = c
		i = c
	}
}

func (t *Timer) Del() bool {
	return deltimer(t)
}

func NewTicker2(d, expiry time.Duration, cb func(interface{}), arg interface{}) *Timer {
	t := &Timer{
		when:   when(d),
		period: int64(d),
		expiry: when(expiry),
		f:      cb,
		arg:    arg,
	}
	startTimer(t)
	return t
}

func NewTicker(d time.Duration, cb func(interface{}), arg interface{}) *Timer {
	t := &Timer{
		when:   when(d),
		period: int64(d),
		f:      cb,
		arg:    arg,
	}
	startTimer(t)
	return t
}

func NewTimer(d time.Duration, cb func(interface{}), arg interface{}) *Timer {
	t := &Timer{
		when: when(d),
		f:    cb,
		arg:  arg,
	}
	startTimer(t)
	return t
}

func Nanotime() int64 {
	return int64(C.nanotime())
}
