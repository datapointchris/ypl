package ytdlp

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// RunArguments are a person's arguments to yt-dlp once ypl has read them, the
// way yt-dlp's own parser would. Each option is kept in the one spelling yt-dlp
// cannot read another way, --name or --name=value, and each input goes after
// --. So the command yt-dlp runs is the one ypl checked, however it was typed.
type RunArguments struct {
	options []string
	inputs  []string
	// sleeps holds each sleep that the person asked for. A run keeps one where
	// it is longer than the door's own.
	sleeps Sleeps
	// maxFilesize is the person's own --max-filesize in bytes, and 0 where
	// they gave none.
	maxFilesize int64
}

// Inputs are the links, video ids and searches the run reads, in order.
func (a RunArguments) Inputs() []string { return a.inputs }

// RefusedArgument names an argument that ypl keeps from yt-dlp, and why.
type RefusedArgument struct {
	Argument string
	Why      string
}

func (r *RefusedArgument) Error() string {
	return fmt.Sprintf("ypl does not pass %s to yt-dlp: %s", r.Argument, r.Why)
}

// runOption is one passed option, by each of its spellings. value says it
// takes one value, which check refuses with a reason, or allows with "". sleep
// names the field of Sleeps it sets, where it is one of the sleeps, and
// maxSize marks --max-filesize.
type runOption struct {
	spellings []string
	value     bool
	check     func(value string) string
	sleep     func(*Sleeps) *float64
	maxSize   bool
}

// canonical is the option's long spelling, which is how it is passed on.
func (o *runOption) canonical() string {
	for _, spelling := range o.spellings {
		if strings.HasPrefix(spelling, "--") {
			return spelling
		}
	}
	return o.spellings[0]
}

func flags(spellings ...string) *runOption { return &runOption{spellings: spellings} }

func valued(spellings ...string) *runOption { return &runOption{spellings: spellings, value: true} }

// passedOptions is every option ypl passes to yt-dlp. Each reads YouTube,
// shapes what is printed, or shapes the files written within the run's own
// directory. Any option that is not here is refused, so an option yt-dlp adds
// later is refused until it is weighed.
var passedOptions = []*runOption{
	// What yt-dlp is and what it would do.
	flags("-h", "--help"), flags("--version"), flags("--list-extractors"), flags("--extractor-descriptions"),
	flags("-i", "--ignore-errors"), flags("--no-abort-on-error"), flags("--abort-on-error", "--no-ignore-errors"),
	flags("--ignore-config", "--no-config"), flags("--no-config-locations"), flags("--no-plugin-dirs"),
	flags("--flat-playlist"), flags("--no-flat-playlist"), flags("--live-from-start"), flags("--no-live-from-start"),
	flags("--no-wait-for-video"), flags("--no-mark-watched"), flags("--no-colors"), valued("--color"),
	{spellings: []string{"-t", "--preset-alias"}, value: true, check: presetAlias},
	flags("-4", "--force-ipv4"), flags("-6", "--force-ipv6"), valued("--socket-timeout"),

	// Which videos.
	valued("-I", "--playlist-items"), valued("--playlist-start"), valued("--playlist-end"),
	valued("--min-filesize"),
	{spellings: []string{"--max-filesize"}, value: true, maxSize: true},
	valued("--date"), valued("--datebefore"), valued("--dateafter"),
	valued("--match-filters"), flags("--no-match-filters"), valued("--break-match-filters"), flags("--no-break-match-filters"),
	flags("--no-playlist"), flags("--yes-playlist"), valued("--age-limit"), flags("--no-download-archive"),
	valued("--max-downloads"), flags("--break-on-existing"), flags("--no-break-on-existing"),
	flags("--break-per-input"), flags("--no-break-per-input"), valued("--skip-playlist-after-errors"),

	// How it downloads.
	valued("-r", "--limit-rate", "--rate-limit"), valued("--throttled-rate"), valued("-R", "--retries"),
	valued("--file-access-retries"), valued("--fragment-retries"), valued("--retry-sleep"),
	flags("--skip-unavailable-fragments", "--no-abort-on-unavailable-fragments"),
	flags("--abort-on-unavailable-fragments", "--no-skip-unavailable-fragments"),
	flags("--keep-fragments"), flags("--no-keep-fragments"), valued("--buffer-size"),
	flags("--resize-buffer"), flags("--no-resize-buffer"), valued("--http-chunk-size"),
	flags("--playlist-random"), flags("--lazy-playlist"), flags("--no-lazy-playlist"),
	flags("--hls-use-mpegts"), flags("--no-hls-use-mpegts"), valued("--download-sections"),

	// The files it writes, all within the run's own directory.
	{spellings: []string{"-o", "--output"}, value: true, check: outputTemplate},
	{spellings: []string{"--output-na-placeholder"}, value: true, check: placeholder},
	flags("--restrict-filenames"), flags("--no-restrict-filenames"), flags("--windows-filenames"), flags("--no-windows-filenames"),
	valued("--trim-filenames", "--trim-file-names"), flags("-w", "--no-overwrites"),
	flags("--force-overwrites", "--yes-overwrites"), flags("--no-force-overwrites"),
	flags("-c", "--continue"), flags("--no-continue"), flags("--part"), flags("--no-part"), flags("--mtime"), flags("--no-mtime"),
	flags("--write-description"), flags("--no-write-description"), flags("--write-info-json"), flags("--no-write-info-json"),
	flags("--write-playlist-metafiles"), flags("--no-write-playlist-metafiles"),
	flags("--clean-info-json", "--clean-infojson"), flags("--no-clean-info-json", "--no-clean-infojson"),
	flags("--write-comments", "--get-comments"), flags("--no-write-comments", "--no-get-comments"),
	flags("--no-cookies"), flags("--no-cookies-from-browser"), flags("--no-cache-dir"),
	flags("--write-thumbnail"), flags("--no-write-thumbnail"), flags("--write-all-thumbnails"), flags("--list-thumbnails"),
	flags("--write-link"), flags("--write-url-link"), flags("--write-webloc-link"), flags("--write-desktop-link"),

	// What it prints.
	flags("-q", "--quiet"), flags("--no-quiet"), flags("--no-warnings"), flags("-s", "--simulate"), flags("--no-simulate"),
	flags("--ignore-no-formats-error"), flags("--no-ignore-no-formats-error"), flags("--skip-download", "--no-download"),
	valued("-O", "--print"), flags("-j", "--dump-json"), flags("-J", "--dump-single-json"),
	flags("-g", "--get-url"), flags("-e", "--get-title"), flags("--get-id"), flags("--get-thumbnail"),
	flags("--get-description"), flags("--get-duration"), flags("--get-filename"), flags("--get-format"),
	flags("--newline"), flags("--no-progress"), flags("--progress"), flags("--console-title"),
	valued("--progress-template"), valued("--progress-delta"), flags("-v", "--verbose"),
	flags("--dump-pages"), flags("--write-pages"), flags("--print-traffic"),

	// The door keeps the longest of each sleep.
	{spellings: []string{"--sleep-requests"}, value: true, sleep: func(s *Sleeps) *float64 { return &s.Requests }},
	{spellings: []string{"--sleep-interval", "--min-sleep-interval"}, value: true, sleep: func(s *Sleeps) *float64 { return &s.Interval }},
	{spellings: []string{"--max-sleep-interval"}, value: true, sleep: func(s *Sleeps) *float64 { return &s.MaxInterval }},
	{spellings: []string{"--sleep-subtitles"}, value: true, sleep: func(s *Sleeps) *float64 { return &s.Subtitles }},

	// Formats and subtitles.
	valued("-f", "--format"), valued("-S", "--format-sort"), flags("--format-sort-reset"),
	flags("--format-sort-force", "--S-force"), flags("--no-format-sort-force"),
	flags("--video-multistreams"), flags("--no-video-multistreams"), flags("--audio-multistreams"), flags("--no-audio-multistreams"),
	flags("--prefer-free-formats"), flags("--no-prefer-free-formats"),
	flags("--check-formats"), flags("--check-all-formats"), flags("--no-check-formats"),
	flags("-F", "--list-formats"), valued("--merge-output-format"),
	flags("--write-subs", "--write-srt"), flags("--no-write-subs", "--no-write-srt"),
	flags("--write-auto-subs", "--write-automatic-subs"), flags("--no-write-auto-subs", "--no-write-automatic-subs"),
	flags("--list-subs"), valued("--sub-format"), valued("--sub-langs", "--srt-langs"),

	// What is done with a file once it is written.
	flags("-x", "--extract-audio"), valued("--audio-format"), valued("--audio-quality"),
	valued("--remux-video"), valued("--recode-video"), flags("-k", "--keep-video"), flags("--no-keep-video"),
	flags("--post-overwrites"), flags("--no-post-overwrites"), flags("--embed-subs"), flags("--no-embed-subs"),
	flags("--embed-thumbnail"), flags("--no-embed-thumbnail"),
	flags("--embed-metadata", "--add-metadata"), flags("--no-embed-metadata", "--no-add-metadata"),
	flags("--embed-chapters", "--add-chapters"), flags("--no-embed-chapters", "--no-add-chapters"),
	flags("--embed-info-json"), flags("--no-embed-info-json"),
	valued("--convert-subs", "--convert-sub", "--convert-subtitles"), valued("--convert-thumbnails"),
	flags("--split-chapters", "--split-tracks"), flags("--no-split-chapters", "--no-split-tracks"),
	valued("--remove-chapters"), flags("--no-remove-chapters"),
	flags("--force-keyframes-at-cuts"), flags("--no-force-keyframes-at-cuts"), valued("--fixup"),
	flags("--no-exec"), flags("--no-sponsorblock"),
}

// Why ypl refuses an option it knows yt-dlp has.
const (
	signsIn      = "it signs the run in, and a signed-in read risks the account"
	runsOnServer = "it runs a program on the server"
	readsServer  = "it reads a file on the server"
	notYouTube   = "it sends the server's requests somewhere other than YouTube"
)

// refusedOptions is why each option ypl knows of and does not pass is refused,
// by every spelling yt-dlp takes for it. An option on neither list is refused
// as one ypl has not weighed.
var refusedOptions = map[string]string{
	"--cookies":                     signsIn,
	"--cookies-from-browser":        signsIn,
	"-u":                            signsIn,
	"--username":                    signsIn,
	"-p":                            signsIn,
	"--password":                    signsIn,
	"-2":                            signsIn,
	"--twofactor":                   signsIn,
	"-n":                            signsIn,
	"--netrc":                       signsIn,
	"--netrc-location":              signsIn,
	"--netrc-cmd":                   signsIn,
	"--video-password":              signsIn,
	"--ap-mso":                      signsIn,
	"--ap-username":                 signsIn,
	"--ap-password":                 signsIn,
	"--client-certificate":          signsIn,
	"--client-certificate-key":      signsIn,
	"--client-certificate-password": signsIn,
	"--add-headers":                 "it can send YouTube a cookie, and a signed-in read risks the account",
	"--mark-watched":                signsIn,

	"--exec":                     runsOnServer,
	"--exec-before-download":     runsOnServer,
	"--use-postprocessor":        runsOnServer,
	"--downloader":               runsOnServer,
	"--external-downloader":      runsOnServer,
	"--downloader-args":          runsOnServer,
	"--external-downloader-args": runsOnServer,
	"--postprocessor-args":       "it passes arguments to a program on the server",
	"--ppa":                      "it passes arguments to a program on the server",
	"--ffmpeg-location":          runsOnServer,
	"--js-runtimes":              runsOnServer,
	"--config-locations":         readsServer,
	"--plugin-dirs":              "it loads plugins from the server's disk",
	"-a":                         readsServer + "; name the links themselves",
	"--batch-file":               readsServer + "; name the links themselves",
	"--load-info-json":           readsServer,
	"--load-pages":               readsServer,
	"--enable-file-urls":         readsServer,
	"--download-archive":         "it reads and writes a file on the server",
	"--print-to-file":            "it writes a file on the server outside the run's own directory",
	"--cache-dir":                "it writes a directory on the server outside the run's own",
	"--rm-cache-dir":             "it removes a directory on the server",
	"-P":                         "it writes outside the run's own directory on the server; -o names a path under where ypl runs",
	"--paths":                    "it writes outside the run's own directory on the server; -o names a path under where ypl runs",
	"-U":                         "it replaces the server's yt-dlp, which only the server's image does",
	"--update":                   "it replaces the server's yt-dlp, which only the server's image does",
	"--update-to":                "it replaces the server's yt-dlp, which only the server's image does",

	"--proxy":                      notYouTube,
	"--geo-verification-proxy":     notYouTube,
	"--remote-components":          notYouTube,
	"--sponsorblock-mark":          notYouTube,
	"--sponsorblock-remove":        notYouTube,
	"--sponsorblock-chapter-title": notYouTube,
	"--sponsorblock-api":           notYouTube,
	"--use-extractors":             "ypl runs YouTube's extractors alone",
	"--ies":                        "ypl runs YouTube's extractors alone",
	"--force-generic-extractor":    "ypl runs YouTube's extractors alone",
	"--default-search":             "ypl runs YouTube's extractors alone; search with ytsearch:<words>",

	"-N":                     "it fetches several pieces at once, faster than the door's pace allows",
	"--concurrent-fragments": "it fetches several pieces at once, faster than the door's pace allows",
	"--wait-for-video":       "it holds the server's one yt-dlp while it waits for a video to begin",
	"--alias":                "it defines options ypl cannot check before yt-dlp reads them",
}

// notWeighed is why an option on neither list is refused.
const notWeighed = "it is not among the options ypl passes, which read YouTube and write within the run's own directory"

// optionsBySpelling is every passed option by each of its spellings.
var optionsBySpelling = func() map[string]*runOption {
	by := map[string]*runOption{}
	for _, option := range passedOptions {
		for _, spelling := range option.spellings {
			if _, twice := by[spelling]; twice {
				panic("ytdlp: " + spelling + " is passed twice")
			}
			if _, refused := refusedOptions[spelling]; refused {
				panic("ytdlp: " + spelling + " is both passed and refused")
			}
			by[spelling] = option
		}
	}
	return by
}()

// ReadRunArguments reads args as yt-dlp's parser does: --name=value or --name
// value for a long option, -xvalue or -x value for a short one, short flags
// run together, and every argument after -- an input. It refuses the first
// option that is not passed, naming it and why, and the first input that is
// not a YouTube link, a video id or a ytsearch query. A long option is matched
// by its whole name, never by a prefix.
func ReadRunArguments(args []string) (RunArguments, error) {
	var read RunArguments
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			for _, input := range args[i+1:] {
				if err := read.addInput(input); err != nil {
					return RunArguments{}, err
				}
			}
			return read, nil
		case strings.HasPrefix(arg, "--"):
			name, value, hasValue := strings.Cut(arg, "=")
			option, err := lookUp(name)
			if err != nil {
				return RunArguments{}, err
			}
			switch {
			case !option.value && hasValue:
				return RunArguments{}, &RefusedArgument{Argument: name, Why: "it takes no value"}
			case option.value && !hasValue:
				if i+1 == len(args) {
					return RunArguments{}, &RefusedArgument{Argument: name, Why: "it needs a value after it"}
				}
				i++
				value = args[i]
			}
			if err := read.addOption(name, option, value); err != nil {
				return RunArguments{}, err
			}
		case strings.HasPrefix(arg, "-") && arg != "-":
			for j := 1; j < len(arg); j++ {
				name := "-" + arg[j:j+1]
				option, err := lookUp(name)
				if err != nil {
					return RunArguments{}, err
				}
				if !option.value {
					if err := read.addOption(name, option, ""); err != nil {
						return RunArguments{}, err
					}
					continue
				}
				value := arg[j+1:]
				if value == "" {
					if i+1 == len(args) {
						return RunArguments{}, &RefusedArgument{Argument: name, Why: "it needs a value after it"}
					}
					i++
					value = args[i]
				}
				if err := read.addOption(name, option, value); err != nil {
					return RunArguments{}, err
				}
				break
			}
		default:
			if err := read.addInput(arg); err != nil {
				return RunArguments{}, err
			}
		}
	}
	return read, nil
}

// lookUp is the passed option spelled name, and its refusal where none is.
func lookUp(name string) (*runOption, error) {
	if option, ok := optionsBySpelling[name]; ok {
		return option, nil
	}
	if why, ok := refusedOptions[name]; ok {
		return nil, &RefusedArgument{Argument: name, Why: why}
	}
	return nil, &RefusedArgument{Argument: name, Why: notWeighed}
}

// addOption keeps option, spelled name, with value where it takes one.
func (a *RunArguments) addOption(name string, option *runOption, value string) error {
	if option.check != nil {
		if why := option.check(value); why != "" {
			return &RefusedArgument{Argument: name + " " + value, Why: why}
		}
	}
	switch {
	case option.sleep != nil:
		seconds, err := strconv.ParseFloat(value, 64)
		if err != nil || seconds < 0 {
			return &RefusedArgument{Argument: name + " " + value, Why: "it is not a number of seconds of at least 0"}
		}
		*option.sleep(&a.sleeps) = seconds
		return nil
	case option.maxSize:
		size, ok := bytesOf(value)
		if !ok {
			return &RefusedArgument{Argument: name + " " + value, Why: "it is not a size, such as 50k or 44.6M"}
		}
		a.maxFilesize = size
		return nil
	}
	if option.value {
		a.options = append(a.options, option.canonical()+"="+value)
	} else {
		a.options = append(a.options, option.canonical())
	}
	return nil
}

// addInput keeps input where it is a YouTube link, a video id or a ytsearch
// query.
func (a *RunArguments) addInput(input string) error {
	if !youTubeInput(input) {
		return &RefusedArgument{Argument: input, Why: "ypl runs yt-dlp on YouTube alone; name a YouTube link, a video id, or ytsearch:<words>"}
	}
	a.inputs = append(a.inputs, input)
	return nil
}

// youTubeHosts are where YouTube serves its links.
var youTubeHosts = map[string]bool{
	"youtube.com":              true,
	"www.youtube.com":          true,
	"m.youtube.com":            true,
	"music.youtube.com":        true,
	"youtu.be":                 true,
	"youtube-nocookie.com":     true,
	"www.youtube-nocookie.com": true,
}

// ytsearch is yt-dlp's search of YouTube: ytsearch, ytsearchdate, either with
// how many to take, then a colon and the words.
var ytsearch = regexp.MustCompile(`^ytsearch(date)?([1-9][0-9]*|all)?:`)

// youTubeInput is whether input is a link on one of youTubeHosts, with or
// without its scheme, a video id, or a ytsearch query.
func youTubeInput(input string) bool {
	if videoID.MatchString(input) || ytsearch.MatchString(input) {
		return true
	}
	if !strings.Contains(input, "://") {
		input = "https://" + input
	}
	link, err := url.Parse(input)
	if err != nil || (link.Scheme != "https" && link.Scheme != "http") || link.User != nil {
		return false
	}
	return youTubeHosts[strings.ToLower(link.Hostname())]
}

// templateTypes are the kinds of file an output template can name before a
// colon, as in subtitle:%(title)s.%(ext)s.
var templateTypes = regexp.MustCompile(`(?i)^(chapter|subtitle|thumbnail|description|annotation|infojson|link|pl_video|pl_thumbnail|pl_description|pl_infojson)(,(chapter|subtitle|thumbnail|description|annotation|infojson|link|pl_video|pl_thumbnail|pl_description|pl_infojson))*:`)

// outputTemplate refuses a template that names a path outside the run's own
// directory: an absolute one, one yt-dlp expands from ~ or an environment
// variable on the server, and one holding .., which can climb out.
func outputTemplate(template string) string {
	template = templateTypes.ReplaceAllString(template, "")
	switch {
	case strings.HasPrefix(template, "/"):
		return "the template is an absolute path on the server; name one relative to where ypl runs"
	case strings.HasPrefix(template, "~"), strings.Contains(template, "$"):
		return "yt-dlp expands ~ and $ to a path on the server; name one relative to where ypl runs"
	case strings.Contains(template, ".."):
		return "the template climbs out of the run's own directory"
	}
	return ""
}

// placeholder refuses a placeholder for a missing field that could put a
// file outside the run's own directory.
func placeholder(text string) string {
	if strings.Contains(text, "/") || strings.Contains(text, "..") {
		return "the placeholder names a path, and a field it stands in for is part of a file's name"
	}
	return ""
}

// presetAliases are the presets -t takes.
var presetAliases = []string{"mp3", "aac", "mp4", "mkv", "sleep"}

func presetAlias(name string) string {
	for _, preset := range presetAliases {
		if name == preset {
			return ""
		}
	}
	return "it is not one of the presets " + strings.Join(presetAliases, ", ")
}

// sizeSuffix is a size as yt-dlp writes one: a number, then k, M, G, T, P, E,
// Z or Y for each power of 1024, in either case, and an optional B.
var sizeSuffix = regexp.MustCompile(`^(\d+(?:\.\d+)?)\s*([kKmMgGtTpPeEzZyY]?)[bB]?$`)

// bytesOf is size in bytes, as yt-dlp reads --max-filesize, and false where it
// is not one.
func bytesOf(size string) (int64, bool) {
	match := sizeSuffix.FindStringSubmatch(size)
	if match == nil {
		return 0, false
	}
	number, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0, false
	}
	power := strings.Index("kmgtpezy", strings.ToLower(match[2])) + 1
	for range power {
		number *= 1024
	}
	return int64(number), true
}
