package ytdlp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrTooLarge is a video whose best audio is larger than the most a read was
// allowed to fetch.
var ErrTooLarge = errors.New("the video's audio is larger than this read may fetch")

// audioLimit bounds one read of a video's audio. A long mix's best audio is a
// few hundred megabytes, and a throttled read fetches tens of kilobytes a
// second.
const audioLimit = 30 * time.Minute

// Audio is a video's best audio, fetched to a file of Size bytes at Path whose
// extension Ext names its container. Close removes it.
type Audio struct {
	Path string
	Ext  string
	Size int64
	dir  string
}

// Close removes the file and the directory holding it.
func (a *Audio) Close() error {
	return os.RemoveAll(a.dir)
}

// Audio fetches the video id's best audio for class to a file the caller
// streams and then closes. maxBytes is the most it fetches, and 0 is no bound.
// The error wraps ErrTooLarge where the audio is larger.
func (d *Door) Audio(ctx context.Context, class Class, id string, maxBytes int64) (*Audio, error) {
	if !videoID.MatchString(id) {
		return nil, fmt.Errorf("read the audio of %q: not a YouTube video id", id)
	}
	audio, err := through(ctx, d, readTerms{class: class, subject: id, limit: audioLimit}, func(ctx context.Context) (*Audio, error) {
		return d.binary.audio(ctx, id, maxBytes)
	})
	if err != nil {
		return nil, fmt.Errorf("read the audio of %s: %w", id, err)
	}
	return audio, nil
}

func (b Binary) audio(ctx context.Context, id string, maxBytes int64) (*Audio, error) {
	dir, err := os.MkdirTemp("", "ypl-audio-")
	if err != nil {
		return nil, err
	}
	audio, err := b.fetchAudio(ctx, dir, id, maxBytes)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return audio, nil
}

// fetchAudio has yt-dlp write the audio into dir. yt-dlp skips a download
// larger than --max-filesize with no error, so a read that wrote nothing under
// a bound found the audio too large.
func (b Binary) fetchAudio(ctx context.Context, dir, id string, maxBytes int64) (*Audio, error) {
	if _, err := b.run(ctx, dir, audioArguments(dir, id, maxBytes), refusal); err != nil {
		return nil, err
	}
	written, err := filepath.Glob(filepath.Join(dir, "audio.*"))
	switch {
	case err != nil:
		return nil, err
	case len(written) == 0 && maxBytes > 0:
		return nil, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, maxBytes)
	case len(written) != 1:
		return nil, fmt.Errorf("yt-dlp wrote %d audio files, want 1", len(written))
	}
	info, err := os.Stat(written[0])
	if err != nil {
		return nil, err
	}
	if maxBytes > 0 && info.Size() > maxBytes {
		return nil, fmt.Errorf("%w: %d bytes, more than %d", ErrTooLarge, info.Size(), maxBytes)
	}
	return &Audio{Path: written[0], Ext: strings.TrimPrefix(filepath.Ext(written[0]), "."), Size: info.Size(), dir: dir}, nil
}

// audioArguments is what a read of the video id's audio passes yt-dlp: the
// best audio-only format, written whole to dir with no part file, and under
// maxBytes where it is not 0. yt-dlp otherwise skips a fragment it cannot
// fetch and exits 0 with a shorter file, so a missing fragment fails the read.
func audioArguments(dir, id string, maxBytes int64) []string {
	args := append(paced(),
		"--no-playlist",
		"--no-part",
		"--no-mtime",
		"--abort-on-unavailable-fragments",
		"--format", "bestaudio",
		"-o", filepath.Join(dir, "audio.%(ext)s"),
	)
	if maxBytes > 0 {
		args = append(args, "--max-filesize", strconv.FormatInt(maxBytes, 10))
	}
	return append(args, "--", watchURL(id))
}
