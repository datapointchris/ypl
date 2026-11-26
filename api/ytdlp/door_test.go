package ytdlp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStamps is the file a stamping fake appends when each read starts and
// ends.
const fakeStamps = "YPL_FAKE_YTDLP_STAMPS"

// stamp is yt-dlp taking a moment to read the video id, writing when it began
// and ended.
func stamp(id string) int {
	write := func(what string) {
		file, err := os.OpenFile(os.Getenv(fakeStamps), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer func() { _ = file.Close() }()
		_, _ = fmt.Fprintf(file, "%s %d\n", what, time.Now().UnixNano())
	}
	write("start")
	time.Sleep(150 * time.Millisecond)
	write("end")
	fmt.Printf(`{"id": %q}`+"\n", id)
	return 0
}

// Two reads asked for at once run one after the other, the second starting
// the door's gap after the first ended. The gap itself is 8 to 16 seconds.
func TestTwoReadsAtOnceRunOneAtATimeAGapApart(t *testing.T) {
	stamps := filepath.Join(t.TempDir(), "stamps")
	t.Setenv(fakeStamps, stamps)
	d := reader(t, "stamp")
	const testGap = 300 * time.Millisecond
	d.gap = func() time.Duration { return testGap }

	var wg sync.WaitGroup
	for _, id := range []string{"Wr1tt3nVid0", "Wr1tt3nVid1"} {
		wg.Go(func() {
			if _, err := d.Video(context.Background(), id); err != nil {
				t.Errorf("Video %s: %v", id, err)
			}
		})
	}
	wg.Wait()

	written, err := os.ReadFile(stamps)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	var at []time.Time
	for line := range strings.Lines(strings.TrimSpace(string(written))) {
		kind, ns, _ := strings.Cut(strings.TrimSpace(line), " ")
		n, err := strconv.ParseInt(ns, 10, 64)
		if err != nil {
			t.Fatalf("stamp %q: %v", line, err)
		}
		kinds, at = append(kinds, kind), append(at, time.Unix(0, n))
	}
	if !slices.Equal(kinds, []string{"start", "end", "start", "end"}) {
		t.Fatalf("stamps %v, want one read to end before the other starts", kinds)
	}
	if apart := at[2].Sub(at[1]); apart < testGap {
		t.Fatalf("the second read started %v after the first ended, want at least %v", apart, testGap)
	}
	for range 1000 {
		if g := gap(); g < 8*time.Second || g > 16*time.Second {
			t.Fatalf("gap() = %v, want 8 to 16 seconds", g)
		}
	}
}

// waitFor fails the test unless cond holds within a second, checking it under
// the door's lock.
func waitFor(t *testing.T, d *Door, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		d.mu.Lock()
		held := cond()
		d.mu.Unlock()
		if held {
			return
		}
	}
	t.Fatalf("%s did not happen within a second", what)
}

// A person's read waiting at the door goes ahead of a service's read that
// arrived before it, and both go ahead of enrichment's.
func TestAPersonsReadGoesAheadOfAServicesThatArrivedFirst(t *testing.T) {
	d := openDoor(t, &memoryPauses{})
	var mu sync.Mutex
	var order []Class
	read := func(class Class, hold <-chan struct{}) {
		_, err := through(context.Background(), d, class, time.Minute, func(context.Context) (struct{}, error) {
			if hold != nil {
				<-hold
			}
			mu.Lock()
			order = append(order, class)
			mu.Unlock()
			return struct{}{}, nil
		})
		if err != nil {
			t.Errorf("a %s read: %v", class, err)
		}
	}
	hold := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() { read(Enrichment, hold) })
	waitFor(t, d, "the first read taking the door", func() bool { return d.busy })
	wg.Go(func() { read(Enrichment, nil) })
	waitFor(t, d, "enrichment queuing", func() bool { return len(d.waiting[Enrichment]) == 1 })
	wg.Go(func() { read(Service, nil) })
	waitFor(t, d, "the service queuing", func() bool { return len(d.waiting[Service]) == 1 })
	wg.Go(func() { read(Person, nil) })
	waitFor(t, d, "the person queuing", func() bool { return len(d.waiting[Person]) == 1 })
	close(hold)
	wg.Wait()

	if want := []Class{Enrichment, Person, Service, Enrichment}; !slices.Equal(order, want) {
		t.Fatalf("reads ran in the order %v, want %v", order, want)
	}
}

// A person's read whose turn does not come within the door's most wait is
// answered ErrBusy, while enrichment waits on. Once the door closes, a read
// still waiting is turned away with ErrClosed, and so is each read asked for
// after.
func TestAPersonWaitsAtMostMostWaitAndAClosedDoorTurnsEveryReadAway(t *testing.T) {
	d := openDoor(t, &memoryPauses{})
	d.mostWait = 50 * time.Millisecond
	hold := make(chan struct{})
	held := make(chan error, 1)
	go func() {
		_, err := through(context.Background(), d, Service, time.Minute, func(context.Context) (struct{}, error) {
			<-hold
			return struct{}{}, nil
		})
		held <- err
	}()
	waitFor(t, d, "the first read taking the door", func() bool { return d.busy })
	none := func(context.Context) (struct{}, error) { return struct{}{}, nil }
	if _, err := through(context.Background(), d, Person, time.Minute, none); !errors.Is(err, ErrBusy) {
		t.Fatalf("a person's read behind a held door = %v, want ErrBusy", err)
	}
	enrichment := make(chan error, 1)
	go func() {
		_, err := through(context.Background(), d, Enrichment, time.Minute, none)
		enrichment <- err
	}()
	waitFor(t, d, "enrichment queuing", func() bool { return len(d.waiting[Enrichment]) == 1 })
	time.Sleep(2 * d.mostWait)
	d.Close()
	if err := <-enrichment; !errors.Is(err, ErrClosed) {
		t.Fatalf("enrichment waiting as the door closed = %v, want ErrClosed", err)
	}
	if _, err := through(context.Background(), d, Person, time.Minute, none); !errors.Is(err, ErrClosed) {
		t.Fatalf("a read asked for after the door closed = %v, want ErrClosed", err)
	}
	close(hold)
	if err := <-held; err != nil {
		t.Fatalf("the read holding the door as it closed = %v, want it run to its end", err)
	}
}

// YouTube refusing the address pauses the door for a day for every class: the
// read waiting when it happens, and each read asked for after. A door opened
// again on the same store is still paused.
func TestARefusalPausesEveryClassForADayAcrossARestart(t *testing.T) {
	pauses := &memoryPauses{}
	d := openDoor(t, pauses)
	now := time.Now()
	d.now = func() time.Time { return now }

	refuse := make(chan struct{})
	refused := make(chan error, 1)
	go func() {
		_, err := through(context.Background(), d, Service, time.Minute, func(context.Context) (struct{}, error) {
			<-refuse
			return struct{}{}, fmt.Errorf("%w: HTTP Error 429", ErrRateLimited)
		})
		refused <- err
	}()
	waitFor(t, d, "the refused read taking the door", func() bool { return d.busy })
	waiting := make(chan error, 1)
	go func() {
		_, err := through(context.Background(), d, Person, time.Minute, func(context.Context) (struct{}, error) {
			return struct{}{}, nil
		})
		waiting <- err
	}()
	waitFor(t, d, "a person's read queuing", func() bool { return len(d.waiting[Person]) == 1 })
	close(refuse)
	if err := <-refused; !errors.Is(err, ErrRateLimited) {
		t.Fatalf("the refused read = %v, want ErrRateLimited", err)
	}

	until := now.Add(RefusedPause)
	turnedAway := func(what string, err error) {
		t.Helper()
		var paused *PausedError
		if !errors.As(err, &paused) || !errors.Is(err, ErrPaused) || !paused.Pause.Until.Equal(until) || paused.Pause.Cause != PausedRefused {
			t.Fatalf("%s = %v, want it turned away until %v for the refusal", what, err, until)
		}
	}
	turnedAway("the read waiting", <-waiting)
	for _, class := range []Class{Person, Service, Enrichment} {
		_, err := through(context.Background(), d, class, time.Minute, func(context.Context) (struct{}, error) {
			t.Fatalf("a %s read ran while the door was paused", class)
			return struct{}{}, nil
		})
		turnedAway("a "+class.String()+" read", err)
	}

	if len(pauses.recorded) != 1 || !pauses.recorded[0].Until.Equal(until) {
		t.Fatalf("pauses recorded %+v, want the one until %v", pauses.recorded, until)
	}
	reopened, err := NewDoor(context.Background(), Binary{}, pauses, nil)
	if err != nil {
		t.Fatal(err)
	}
	reopened.now = d.now
	if pause, held := reopened.PausedUntil(); !held || !pause.Until.Equal(until) {
		t.Fatalf("a door reopened on the store is paused until %v (%v), want %v", pause.Until, held, until)
	}
}

// Three reads failing in a row pause the door for an hour, whatever they failed
// with. A read YouTube answered between failures starts the count again, and so
// does a video closed to reading.
func TestThreeFailuresInARowPauseTheDoorForAnHour(t *testing.T) {
	d := openDoor(t, &memoryPauses{})
	now := time.Now()
	d.now = func() time.Time { return now }
	failed := errors.New("yt-dlp: Unable to extract initial player response")
	for i, outcome := range []error{failed, failed, nil, failed, failed, ErrUnreadable, failed, failed} {
		if _, err := through(context.Background(), d, Enrichment, time.Minute, func(context.Context) (struct{}, error) {
			return struct{}{}, outcome
		}); !errors.Is(err, outcome) {
			t.Fatalf("read %d = %v, want %v", i, err, outcome)
		}
		if _, held := d.PausedUntil(); held {
			t.Fatalf("the door paused after read %d, with no three failures in a row", i)
		}
	}
	_, _ = through(context.Background(), d, Enrichment, time.Minute, func(context.Context) (struct{}, error) {
		return struct{}{}, failed
	})
	if pause, held := d.PausedUntil(); !held || pause.Cause != PausedFailing || !pause.Until.Equal(now.Add(FailingPause)) {
		t.Fatalf("after three failures in a row the door holds %+v (%v), want a failing pause of an hour", pause, held)
	}
}
