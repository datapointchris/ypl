package ytdlp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// run is yt-dlp writing a caption file where --output names, and saying so.
func run(args []string) int {
	for _, arg := range args {
		if template, ok := strings.CutPrefix(arg, "--output="); ok {
			name := strings.ReplaceAll(template, "%(ext)s", "en.vtt")
			if err := os.WriteFile(name, []byte("WEBVTT\n"), 0o600); err != nil {
				fmt.Fprintln(os.Stderr, "ERROR:", err)
				return 1
			}
			fmt.Println("[info] Writing video subtitles to:", name)
		}
	}
	return 0
}

const link = "https://youtu.be/Wr1tt3nVid0"

// Each option is passed in its one long spelling, short ones run together
// included, and each sleep is kept aside for the door to weigh.
func TestARunsArgumentsAreReadAsYtdlpReadsThem(t *testing.T) {
	read, err := ReadRunArguments([]string{"-f", "bestaudio", "-xo", "a.%(ext)s", "--sleep-requests=5", "-I1:3", link, "--", "-kbYeaEP-ME"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"--format=bestaudio", "--extract-audio", "--output=a.%(ext)s", "--playlist-items=1:3"}; !slices.Equal(read.options, want) {
		t.Errorf("options %q, want %q", read.options, want)
	}
	if want := []string{link, "-kbYeaEP-ME"}; !slices.Equal(read.inputs, want) {
		t.Errorf("inputs %q, want %q", read.inputs, want)
	}
	if read.sleeps.Requests != 5 {
		t.Errorf("sleeps %+v, want the person's 5 between requests", read.sleeps)
	}
}

// The refusal names the argument and why, for each kind ypl does not pass.
func TestARunRefusesEachArgumentItDoesNotPassNamingWhy(t *testing.T) {
	for _, c := range []struct {
		args     []string
		argument string
		reason   reason
	}{
		{[]string{"--cookies", "c.txt", link}, "--cookies", signsIn},
		{[]string{"--cookies-from-browser=firefox", link}, "--cookies-from-browser", signsIn},
		{[]string{"-u", "me", link}, "-u", signsIn},
		{[]string{"--exec", "rm -rf /", link}, "--exec", runsOnServer},
		{[]string{"-a", "links.txt"}, "-a", readsServer},
		{[]string{"-P", "/tmp", link}, "-P", writesElsewhere},
		{[]string{"--ignore-no-formats-error", "-J", link}, "--ignore-no-formats-error", hidesRefusal},
		{[]string{"-o", "/etc/x.%(ext)s", link}, "-o /etc/x.%(ext)s", absolutePath},
		{[]string{"-o", "subtitle:../x.%(ext)s", link}, "-o subtitle:../x.%(ext)s", climbsOut},
		{[]string{"--output=$HOME/x", link}, "--output $HOME/x", expandsOnServer},
		{[]string{"--cook", "c.txt", link}, "--cook", notWeighed},
		{[]string{"--skip-download=yes", link}, "--skip-download", takesNoValue},
		{[]string{link, "-f"}, "-f", needsValue},
		{[]string{"https://vimeo.com/1"}, "https://vimeo.com/1", notYouTubeInput},
		{[]string{"--sleep-interval", "soon", link}, "--sleep-interval soon", notSeconds},
	} {
		_, err := ReadRunArguments(c.args)
		var refused *RefusedArgument
		if !errors.As(err, &refused) || refused.Argument != c.argument || refused.reason != c.reason {
			t.Errorf("%q was refused with %v, want %s named as %s", c.args, err, c.argument, c.reason)
		}
	}
}

// A run passes the door's sleeps, or the person's where those are longer, and
// a most sleep before a download no shorter than the least.
func TestARunKeepsTheLongerOfEachSleep(t *testing.T) {
	read, err := ReadRunArguments([]string{"--sleep-interval", "30", link})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(read.argv(DefaultPace.Sleeps, 0), " ")
	if !strings.Contains(got, "--sleep-requests 2 --sleep-subtitles 5 --sleep-interval 30 --max-sleep-interval 30 --use-extractors=youtube.* --color=never -- "+link) {
		t.Fatalf("argv %q, want the door's sleeps but the person's longer interval", got)
	}
}

// A run writes its file in a directory of its own and answers its exit code.
// One that YouTube refuses mid-run is stopped at the refusal, not when yt-dlp
// next gives up, and pauses the door for every read after it.
func TestARunIsStoppedAtYouTubesRefusalAndPausesTheDoor(t *testing.T) {
	d := reader(t, "run")
	read, err := ReadRunArguments([]string{"--skip-download", "-o", "x.%(ext)s", link})
	if err != nil {
		t.Fatal(err)
	}
	made, err := d.Run(context.Background(), Person, read, 0)
	if err != nil {
		t.Fatal(err)
	}
	files, err := made.Files()
	if err != nil || len(files) != 1 || files[0].Path != "x.en.vtt" || made.ExitCode != 0 {
		t.Fatalf("the run wrote %+v (%v) and exited %d, want x.en.vtt and 0", files, err, made.ExitCode)
	}
	if err := made.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(made.FilesDir()); !os.IsNotExist(err) {
		t.Fatalf("the run's directory is still there after Close: %v", err)
	}

	t.Setenv(fakeMode, "refused mid-run")
	began := time.Now()
	_, err = d.Run(context.Background(), Person, read, 0)
	if _, paused := PauseOf(err); !errors.Is(err, ErrRateLimited) || !paused || time.Since(began) > 10*time.Second {
		t.Fatalf("the refused run answered %v after %v, want the refusal and its pause at once", err, time.Since(began))
	}
	if _, err := d.Run(context.Background(), Person, read, 0); !errors.Is(err, ErrPaused) {
		t.Fatalf("the run after the refusal answered %v, want the pause", err)
	}
}

// A lease holds the door for its hold, so the next read starts no sooner, and
// the breaker turns a lease away as it does a read.
func TestALeaseHoldsTheDoorAndThePauseTurnsItAway(t *testing.T) {
	d := reader(t, "run")
	d.leaseHold = 300 * time.Millisecond
	began := time.Now()
	if _, err := d.Lease(context.Background(), Person); err != nil {
		t.Fatal(err)
	}
	read, err := ReadRunArguments([]string{"--skip-download", link})
	if err != nil {
		t.Fatal(err)
	}
	made, err := d.Run(context.Background(), Person, read, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = made.Close()
	if waited := time.Since(began); waited < d.leaseHold {
		t.Fatalf("the read after a lease ended %v after it, want it to wait the hold of %v", waited, d.leaseHold)
	}

	// A desk's resolve that YouTube refused pauses the door as a read of its
	// own would, though the desk's yt-dlp wrote its error in color.
	lease, err := d.Lease(context.Background(), Person)
	if err != nil {
		t.Fatal(err)
	}
	d.EndLease(lease.ID, 1, "\x1b[0;31mERROR:\x1b[0m [youtube] ccccccccccc: Sign in to confirm you're not a bot\n")
	if _, err := d.Lease(context.Background(), Person); !errors.Is(err, ErrPaused) {
		t.Fatalf("a lease after a desk's refused read answered %v, want the pause", err)
	}
}

// Runs that fail on their own options, such as -x on a server with no
// ffmpeg, pause the person's runs alone. The server's own reads and a desk's
// leases go on.
func TestRunsFailingInARowPauseRunsAloneAndLeasesGoOn(t *testing.T) {
	d := reader(t, "fail")
	d.leaseHold = time.Millisecond
	t.Setenv(fakeError, "ERROR: Postprocessing: ffprobe and ffmpeg not found. Please install or provide the path using --ffmpeg-location")
	for _, id := range []string{"aaaaaaaaaaa", "bbbbbbbbbbb", "ccccccccccc", "ddddddddddd"} {
		read, err := ReadRunArguments([]string{"-x", "--", id})
		if err != nil {
			t.Fatal(err)
		}
		made, err := d.Run(context.Background(), Person, read, 0)
		if id == "ddddddddddd" {
			if !errors.Is(err, ErrPaused) {
				t.Fatalf("the run after %d failed answered %v, want the runs' pause", MaxFailuresInARow, err)
			}
			break
		}
		if err != nil || made.ExitCode != 1 {
			t.Fatalf("a failing run answered %+v, %v, want its exit code", made, err)
		}
		_ = made.Close()
	}
	if _, paused := d.PausedUntil(); paused {
		t.Fatal("failing runs paused every read")
	}
	if _, err := d.Lease(context.Background(), Person); err != nil {
		t.Fatalf("a lease after failing runs answered %v, want a turn", err)
	}
}

// A run stops once the disk it writes to falls below the floor, so a playlist
// downloaded whole cannot fill the disk the store is on. The stop says nothing
// of YouTube, so it moves no failures in a row.
func TestARunStopsWhereTheDiskFallsBelowItsFloor(t *testing.T) {
	d := reader(t, "sleep")
	d.diskFloor = math.MaxInt64
	read, err := ReadRunArguments([]string{link})
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	if _, err := d.Run(context.Background(), Person, read, 0); !errors.Is(err, ErrDiskLow) || time.Since(began) > 5*time.Second {
		t.Fatalf("a run with no room answered %v after %v, want ErrDiskLow at once", err, time.Since(began))
	}
	if len(d.failedRuns) != 0 {
		t.Errorf("the stop counted %v as failures", d.failedRuns)
	}
}

func TestSleepsAreReadAsYtdlpsOptionsKeepingTheRestAtTheirDefault(t *testing.T) {
	sleeps, err := ParseSleeps("--sleep-requests 3 --max-sleep-interval 20")
	if err != nil || sleeps != (Sleeps{Requests: 3, Subtitles: 5, Interval: 8, MaxInterval: 20}) {
		t.Fatalf("ParseSleeps = %+v, %v", sleeps, err)
	}
	for _, bad := range []string{"--sleep-requests", "--sleep-requests -1", "--limit-rate 5", "--sleep-interval 30"} {
		if _, err := ParseSleeps(bad); err == nil {
			t.Errorf("ParseSleeps(%q) was taken, want it refused", bad)
		}
	}
}

// helpOption is one option line of yt-dlp's help: its spellings, and the
// placeholder for its value where it takes one.
var helpOption = regexp.MustCompile(`^ {4}((?:-[^ ,]+, )?--[^ ]+)( [^ ]+)?(?: {2,}|$)`)

// hiddenOptions are passed spellings that yt-dlp's help leaves out: options
// it keeps for old command lines, and second spellings of options it lists.
var hiddenOptions = []string{
	"--playlist-start", "--playlist-end", "-g", "--get-url", "-e", "--get-title", "--get-id",
	"--get-thumbnail", "--get-description", "--get-duration", "--get-filename", "--get-format",
	"--no-colors", "--rate-limit", "--trim-file-names", "--yes-overwrites",
	"--clean-infojson", "--no-clean-infojson", "--write-srt", "--no-write-srt", "--srt-langs",
	"--convert-subtitles", "--split-tracks", "--no-split-tracks",
}

// Every passed option is one the real yt-dlp has, and takes a value exactly
// where ypl reads one after it. An option yt-dlp renamed or dropped would
// otherwise fail every run that names it, and a value read wrongly would turn
// an input into an option's value.
func TestEveryPassedOptionIsOneTheRealYtdlpTakesWithItsValue(t *testing.T) {
	path, err := exec.LookPath("yt-dlp")
	if err != nil {
		t.Skipf("yt-dlp is not installed: %v", err)
	}
	help, err := exec.CommandContext(t.Context(), path, "--help").Output()
	if err != nil {
		t.Fatal(err)
	}
	takes := map[string]bool{}
	for line := range strings.Lines(string(help)) {
		match := helpOption.FindStringSubmatch(strings.TrimRight(line, "\n"))
		if match == nil {
			continue
		}
		for spelling := range strings.SplitSeq(match[1], ", ") {
			takes[spelling] = match[2] != ""
		}
	}
	for _, option := range passedOptions {
		for _, spelling := range option.spellings {
			value, listed := takes[spelling]
			switch {
			case listed && value != option.value:
				t.Errorf("%s takes a value: %v in yt-dlp's help, %v here", spelling, value, option.value)
			case !listed && !strings.Contains(string(help), spelling) && !slices.Contains(hiddenOptions, spelling):
				t.Errorf("%s is not in yt-dlp's help", spelling)
			}
		}
	}
}
