package ytdlp

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
)

// Pace is how far apart the door starts reads, and how long yt-dlp waits
// between the requests inside one read. The next read starts at least Gap
// after the last ended, plus up to Jitter more at random, so reads keep no
// rhythm.
type Pace struct {
	Gap    time.Duration
	Jitter time.Duration
	Sleeps Sleeps
}

// Sleeps are the seconds yt-dlp waits inside one read, which the door's gap
// does not reach: between requests, before each subtitle, and before each
// download, from Interval up to MaxInterval at random.
type Sleeps struct {
	Requests    float64
	Subtitles   float64
	Interval    float64
	MaxInterval float64
}

// DefaultPace is the pace where the server's configuration names none.
var DefaultPace = Pace{
	Gap:    8 * time.Second,
	Jitter: 8 * time.Second,
	Sleeps: Sleeps{Requests: 2, Subtitles: 5, Interval: 8, MaxInterval: 16},
}

// next is the wait before the next read: Gap and up to Jitter more at random.
func (p Pace) next() time.Duration {
	return p.Gap + time.Duration(rand.Int64N(int64(p.Jitter)+1))
}

// sleepOptions is each yt-dlp option Sleeps sets, by the field it sets.
var sleepOptions = []struct {
	name  string
	field func(*Sleeps) *float64
}{
	{"--sleep-requests", func(s *Sleeps) *float64 { return &s.Requests }},
	{"--sleep-subtitles", func(s *Sleeps) *float64 { return &s.Subtitles }},
	{"--sleep-interval", func(s *Sleeps) *float64 { return &s.Interval }},
	{"--max-sleep-interval", func(s *Sleeps) *float64 { return &s.MaxInterval }},
}

// ParseSleeps reads sleeps written as yt-dlp's options, such as
// "--sleep-requests 2 --sleep-interval 8". An option it leaves out keeps
// DefaultPace's value. It refuses any other option, a value that is not a
// number of seconds of at least 0, and a most sleep before a download shorter
// than the least, which yt-dlp itself would refuse at every read.
func ParseSleeps(written string) (Sleeps, error) {
	sleeps := DefaultPace.Sleeps
	fields := strings.Fields(written)
	for i := 0; i < len(fields); i += 2 {
		name := fields[i]
		at := -1
		for j, option := range sleepOptions {
			if option.name == name {
				at = j
			}
		}
		if at < 0 {
			return Sleeps{}, fmt.Errorf("%q is not one of --sleep-requests, --sleep-subtitles, --sleep-interval or --max-sleep-interval", name)
		}
		if i+1 == len(fields) {
			return Sleeps{}, fmt.Errorf("%s has no number of seconds after it", name)
		}
		seconds, err := strconv.ParseFloat(fields[i+1], 64)
		if err != nil || seconds < 0 {
			return Sleeps{}, fmt.Errorf("%s %q is not a number of seconds of at least 0", name, fields[i+1])
		}
		*sleepOptions[at].field(&sleeps) = seconds
	}
	if sleeps.MaxInterval < sleeps.Interval {
		return Sleeps{}, fmt.Errorf("--max-sleep-interval %v is shorter than --sleep-interval %v", sleeps.MaxInterval, sleeps.Interval)
	}
	return sleeps, nil
}

// arguments is the sleeps as yt-dlp's options.
func (s Sleeps) arguments() []string {
	var args []string
	for _, option := range sleepOptions {
		args = append(args, option.name, strconv.FormatFloat(*option.field(&s), 'f', -1, 64))
	}
	return args
}

// String is the sleeps as yt-dlp's options, in one line.
func (s Sleeps) String() string {
	return strings.Join(s.arguments(), " ")
}

// longerOf is each of s's sleeps, or other's where that is longer. A most
// sleep before a download is kept at least the least.
func (s Sleeps) longerOf(other Sleeps) Sleeps {
	longer := Sleeps{
		Requests:    max(s.Requests, other.Requests),
		Subtitles:   max(s.Subtitles, other.Subtitles),
		Interval:    max(s.Interval, other.Interval),
		MaxInterval: max(s.MaxInterval, other.MaxInterval),
	}
	longer.MaxInterval = max(longer.MaxInterval, longer.Interval)
	return longer
}

// paced is what every read passes yt-dlp ahead of what it reads. It reads no
// config file and loads no plugin, which are separate settings and either of
// which would let the host change what a read does. The sleeps pace the
// requests inside one read, which the door's gap does not reach.
func paced(sleeps Sleeps) []string {
	return append([]string{"--ignore-config", "--no-plugin-dirs"}, sleeps.arguments()...)
}
