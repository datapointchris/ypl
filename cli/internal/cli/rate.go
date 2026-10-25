package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/datapointchris/goclikit"
	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/api"
	"github.com/datapointchris/ypl/cli/internal/youtube"
)

// historyOffered is how many of the watch history's newest library videos the
// picker puts above the rest of the library.
const historyOffered = 5

func (a *app) rateCommand() *cobra.Command {
	var (
		like, dislike, note bool
		message             string
		at                  string
		tags                []string
		asJSON              bool
	)
	cmd := &cobra.Command{
		Use:     "rate [video]",
		Short:   "Like, dislike or leave a note on a mix, with a message and tags",
		GroupID: groupRatings,
		Long: "Each rating is kept, so a mix holds every like, dislike and note left on it,\n" +
			"and `ypl videos show` lists them. A note needs a message; a like or a dislike\n" +
			"may carry one.\n\n" +
			"At a terminal with no video named, it offers the library in fzf, with the newest\n" +
			"mixes in YouTube's watch history on top. Every player signed into the channel's\n" +
			"account writes to that history. With no --like, --dislike or --note it then asks\n" +
			"for the kind, the message, the tags and where in the mix, offering YouTube's resume\n" +
			"point; with one, it records what the flags gave and nothing else.\n\n" +
			"Without a terminal, or under --no-input, name the video and give the kind.",
		Example: "  ypl rate                                       pick what is playing, then rate it\n" +
			"  ypl rate --like -m 'great closing hour'        pick it, and like it\n" +
			"  ypl rate dQw4w9WgXcQ --note -m 'too much talk'  a note on one mix\n" +
			"  ypl rate 'mayan warrior' --dislike --tag dark  by part of its title\n" +
			"  ypl rate --dislike --at 1:12:30 -m 'vocals'    and where in the mix",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind := ""
			for flag, given := range map[string]bool{"like": like, "dislike": dislike, "note": note} {
				if !given {
					continue
				}
				if kind != "" {
					return goclikit.UsageError(errors.New("give one of --like, --dislike and --note"))
				}
				kind = flag
			}
			if kind == "note" && strings.TrimSpace(message) == "" {
				return goclikit.UsageError(errors.New("a note needs --message"))
			}
			var position *int64
			if at != "" {
				seconds, err := clockSeconds(at)
				if err != nil {
					return goclikit.UsageError(fmt.Errorf("--at: %w", err))
				}
				position = &seconds
			}
			interactive := !forbidsInput(cmd) && a.terminalIn(cmd)
			if !interactive && len(args) == 0 {
				return goclikit.UsageError(errors.New("name the video; only a terminal can pick it from the watch history"))
			}
			if !interactive && kind == "" {
				return goclikit.UsageError(errors.New("give --like, --dislike or --note; only a terminal is asked"))
			}
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			var picked api.HistoryEntry
			if len(args) == 1 {
				picked.Video, err = namedVideo(cmd.Context(), client, args[0])
			} else {
				picked, err = pickVideo(cmd, client)
			}
			video := picked.Video
			if err != nil {
				return reported(err)
			}
			rating := api.NewRating{VideoID: video.ID, Kind: kind, Tags: tags, PositionSeconds: position}
			if kind == "" && position == nil && picked.ResumeSeconds != nil {
				rating.PositionSeconds, rating.PositionEstimated = picked.ResumeSeconds, true
			}
			if strings.TrimSpace(message) != "" {
				rating.Message = &message
			}
			if kind == "" {
				if err := askRating(cmd.Context(), client, video, &rating); err != nil {
					return err
				}
			}
			id, err := uuid.NewV7()
			if err != nil {
				return fmt.Errorf("make an id for the rating: %w", err)
			}
			rating.ID = id.String()
			stored, err := client.CreateRating(cmd.Context(), rating)
			if err != nil {
				return reported(err)
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), stored)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), describeRating(stored))
			return nil
		},
	}
	cmd.Flags().BoolVar(&like, "like", false, "Like it")
	cmd.Flags().BoolVar(&dislike, "dislike", false, "Dislike it")
	cmd.Flags().BoolVar(&note, "note", false, "Leave a note on it, which needs --message")
	cmd.Flags().StringVarP(&message, "message", "m", "", "What to say about it")
	cmd.Flags().StringVar(&at, "at", "", "Where in the mix, as h:mm:ss, m:ss or seconds")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "A tag for it (repeatable)")
	addJSON(cmd, &asJSON, "the rating")
	return goclikit.WithRecoveryHints(cmd, hintVideos)
}

// namedVideo is the video ref names by its link, id or title, as `ypl videos
// show` resolves one.
func namedVideo(ctx context.Context, client *api.Client, ref string) (api.PlayedVideo, error) {
	if id := youtube.VideoID(ref); id != "" {
		ref = id
	}
	video, err := client.GetVideo(ctx, ref)
	if err != nil {
		return api.PlayedVideo{}, err
	}
	return api.PlayedVideo{ID: video.ID, Title: video.Title, ChannelTitle: video.ChannelTitle}, nil
}

// pickVideo is a video chosen in fzf from the whole library, with the newest
// library videos in the watch history at the top, each marked with its place
// in the history. It answers as a history entry, which for a video picked from
// outside the history carries only the video. A history that cannot be read
// says why on stderr and the library is offered alone, so a session that
// expired never stops a rating.
func pickVideo(cmd *cobra.Command, client *api.Client) (api.HistoryEntry, error) {
	ctx := cmd.Context()
	fzf, err := exec.LookPath("fzf")
	if err != nil {
		return api.HistoryEntry{}, errors.New("picking a mix needs fzf on PATH; name the video instead")
	}
	entries, err := client.History(ctx, historyOffered)
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "The watch history could not be read: %v\n", err)
		entries = nil
	}
	videos, err := client.ListVideos(ctx, api.VideoFilter{Sort: "title"})
	if err != nil {
		return api.HistoryEntry{}, err
	}
	byID := make(map[string]api.HistoryEntry, len(videos))
	var lines strings.Builder
	for _, entry := range entries {
		byID[entry.Video.ID] = entry
		_, _ = fmt.Fprintf(&lines, "%s\t%s\t%s  · %s\n", entry.Video.ID, historyMark(entry.Position), entry.Video.Title, entry.Video.ChannelTitle)
	}
	for _, video := range videos {
		if _, listed := byID[video.ID]; listed {
			continue
		}
		byID[video.ID] = api.HistoryEntry{Video: api.PlayedVideo{ID: video.ID, Title: video.Title, ChannelTitle: video.ChannelTitle}}
		_, _ = fmt.Fprintf(&lines, "%s\t\t%s  · %s\n", video.ID, video.Title, video.ChannelTitle)
	}
	if len(byID) == 0 {
		return api.HistoryEntry{}, errors.New("the library holds no video")
	}
	header := "Which mix? The numbers are places in your watch history, 1 the newest."
	if len(entries) > 0 && entries[0].Position != 1 {
		header += "\nThe newest video in your history is not in the library."
	}
	picker := exec.CommandContext(ctx, fzf, "--delimiter=\t", "--with-nth=2..", "--no-sort", "--height=50%", "--reverse", "--header="+header)
	picker.Stdin = strings.NewReader(lines.String())
	picker.Stderr = cmd.ErrOrStderr()
	chosen, err := picker.Output()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit) && exit.ExitCode() == 130:
		return api.HistoryEntry{}, exitCode(130)
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return api.HistoryEntry{}, errors.New("no mix matched; nothing was rated")
	case err != nil:
		return api.HistoryEntry{}, fmt.Errorf("run fzf: %w", err)
	}
	id, _, _ := strings.Cut(string(chosen), "\t")
	entry, ok := byID[id]
	if !ok {
		return api.HistoryEntry{}, fmt.Errorf("fzf answered %q, which is no video offered", strings.TrimSpace(string(chosen)))
	}
	return entry, nil
}

// historyMark is a history entry's place, written where a library video
// outside the history leaves the column empty.
func historyMark(position int64) string {
	return "▶ " + strconv.FormatInt(position, 10)
}

// askRating asks for the kind, the message and tags, starting from what the
// flags gave. A note needs a message. Tags already used are offered to choose
// from, beside a line for new ones.
func askRating(ctx context.Context, client *api.Client, video api.PlayedVideo, rating *api.NewRating) error {
	kind := "like"
	message := ""
	if rating.Message != nil {
		message = *rating.Message
	}
	fresh := ""
	estimatedOffer := rating.PositionEstimated
	at := clock(rating.PositionSeconds)
	where := "Optional, as 1:12:30 or 45:10"
	if rating.PositionEstimated {
		where = "Estimated from YouTube's resume point; edit or clear it"
	}
	var chosen []string
	used, err := client.ListTags(ctx)
	if err != nil {
		return reported(err)
	}
	fields := []huh.Field{
		huh.NewSelect[string]().Title(video.Title).Options(
			huh.NewOption("Like", "like"),
			huh.NewOption("Dislike", "dislike"),
			huh.NewOption("Note", "note"),
		).Height(4).Value(&kind),
		huh.NewText().Title("Message").Description("Needed for a note, optional otherwise").Value(&message).
			Validate(func(text string) error {
				if kind == "note" && strings.TrimSpace(text) == "" {
					return errors.New("a note needs a message")
				}
				return nil
			}),
	}
	if len(used) > 0 {
		options := make([]huh.Option[string], len(used))
		for i, tag := range used {
			options[i] = huh.NewOption(fmt.Sprintf("%s (%d)", tag.Tag, tag.Videos), tag.Tag)
		}
		fields = append(fields, huh.NewMultiSelect[string]().Title("Tags you have used").Description("Optional; / filters").Options(options...).Filterable(true).Height(10).Value(&chosen))
	}
	fields = append(fields,
		huh.NewInput().Title("New tags").Description("Optional, separated by commas").Value(&fresh),
		huh.NewInput().Title("Where in the mix").Description(where).Value(&at).
			Validate(func(reading string) error {
				if strings.TrimSpace(reading) == "" {
					return nil
				}
				_, err := clockSeconds(reading)
				return err
			}),
	)
	if err := huh.NewForm(huh.NewGroup(fields...)).Run(); err != nil {
		return aborted(err)
	}
	rating.Kind = kind
	rating.Message = nil
	if strings.TrimSpace(message) != "" {
		rating.Message = &message
	}
	offered := rating.PositionSeconds
	rating.PositionSeconds, rating.PositionEstimated = nil, false
	if strings.TrimSpace(at) != "" {
		seconds, _ := clockSeconds(at)
		rating.PositionSeconds = &seconds
		rating.PositionEstimated = offered != nil && *offered == seconds && estimatedOffer
	}
	rating.Tags = append(rating.Tags, chosen...)
	for tag := range strings.SplitSeq(fresh, ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			rating.Tags = append(rating.Tags, tag)
		}
	}
	return nil
}

// aborted is a prompt's error, with an interrupt as the exit code a person
// pressing ^C expects and no error line.
func aborted(err error) error {
	if errors.Is(err, huh.ErrUserAborted) {
		return exitCode(130)
	}
	return err
}

// describeRating is one line saying what a rating recorded.
func describeRating(r api.Rating) string {
	line := map[string]string{"like": "Liked ", "dislike": "Disliked ", "note": "Noted on "}[r.Kind] + r.Video.Title
	if at := positionCell(r); at != "" {
		line += " at " + at
	}
	if r.Message != nil {
		line += ": " + *r.Message
	}
	if len(r.Tags) > 0 {
		line += " [" + strings.Join(r.Tags, ", ") + "]"
	}
	return line
}

func (a *app) ratingsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ratings",
		Short:   "What you said about mixes",
		GroupID: groupRatings,
		Long:    "`ypl rate` records a rating. `ypl videos show` lists every rating of one mix.",
	}
	goclikit.AsNamespace(cmd)
	cmd.AddCommand(a.ratingsListCommand(), a.ratingsDeleteCommand())
	return cmd
}

func (a *app) ratingsDeleteCommand() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <rating>",
		Short: "Delete a rating left by mistake, with its tags",
		Long: "Name the rating by the ID column of `ypl ratings list` or `ypl videos show`.\n" +
			"It asks first; --yes answers for it.",
		Example: "  ypl ratings delete 3f9c2a1b        ask, then delete it\n" +
			"  ypl ratings delete 3f9c2a1b --yes  delete it without asking",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				if err := a.confirmable(cmd); err != nil {
					return err
				}
			}
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			if !yes {
				approved, err := a.confirm(cmd, "Delete rating "+args[0]+"?")
				if err != nil {
					return err
				}
				if !approved {
					nothing(cmd, "Nothing was deleted.")
					return exitCode(1)
				}
			}
			if err := client.DeleteRating(cmd.Context(), args[0]); err != nil {
				return reported(err)
			}
			nothing(cmd, "Deleted rating "+args[0]+".")
			return nil
		},
	}
	addYes(cmd, &yes, "delete it")
	return cmd
}

// ratingHandle is the end of a rating's id that ypl ratings delete takes.
func ratingHandle(id string) string {
	if len(id) <= ratingHandleLength {
		return id
	}
	return id[len(id)-ratingHandleLength:]
}

const ratingHandleLength = 8

func (a *app) ratingsListCommand() *cobra.Command {
	var (
		video  string
		limit  = defaultPlays
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List ratings, newest first",
		Example: "  ypl ratings list                          what you rated lately\n" +
			"  ypl ratings list --video 'mayan warrior'  every rating of one mix",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			if id := youtube.VideoID(video); id != "" {
				video = id
			}
			ratings, err := client.ListRatings(cmd.Context(), video, limit)
			if err != nil {
				return reported(err)
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), ratings.Rows)
			}
			if len(ratings.Rows) == 0 {
				nothing(cmd, "Nothing has been rated yet. `ypl rate` rates what is playing.")
				return nil
			}
			printRatings(cmd.OutOrStdout(), a.width(cmd.OutOrStdout()), ratings.Rows)
			if ratings.More {
				nothing(cmd, fmt.Sprintf("More ratings follow. `ypl ratings list --limit %d` reads further back.", limit*2))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&video, "video", "", "Only the ratings of this video, by its link, id or title")
	addLimit(cmd, &limit, 0, "How many ratings to read, over as many pages as that takes")
	addJSON(cmd, &asJSON, "the ratings")
	return goclikit.WithRecoveryHints(cmd, hintVideos)
}

// printRatingsOf lists one mix's ratings, where the title would repeat on
// every row.
func printRatingsOf(out io.Writer, width int, ratings []api.Rating, tracks []api.Track) {
	rows := make([][]string, len(ratings))
	for i, r := range ratings {
		rows[i] = []string{ratingHandle(r.ID), r.RatedTs, r.Kind, positionCell(r), trackCell(tracks, r.PositionSeconds), text(r.Message), strings.Join(r.Tags, ", ")}
	}
	table(out, width, []column{whole("ID"), whole("WHEN"), whole("KIND"), whole("AT"), detail("TRACK"), prose("MESSAGE"), detail("TAGS")}, rows)
}

// trackCell names the track playing at position, and is "" where no track
// holds it.
func trackCell(tracks []api.Track, position *int64) string {
	return trackName(trackAt(tracks, position))
}

func printRatings(out io.Writer, width int, ratings []api.Rating) {
	rows := make([][]string, len(ratings))
	for i, r := range ratings {
		rows[i] = []string{ratingHandle(r.ID), r.RatedTs, r.Kind, r.Video.Title, positionCell(r), text(r.Message), strings.Join(r.Tags, ", ")}
	}
	table(out, width, []column{whole("ID"), whole("WHEN"), whole("KIND"), prose("TITLE"), whole("AT"), detail("MESSAGE"), detail("TAGS")}, rows)
}

func (a *app) tagsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tags",
		Short:   "The tags your ratings carried",
		GroupID: groupRatings,
	}
	goclikit.AsNamespace(cmd)
	cmd.AddCommand(a.tagsListCommand())
	return cmd
}

func (a *app) tagsListCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List every tag with how many mixes carry it",
		Example: "  ypl tags list  most carried first",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			tags, err := client.ListTags(cmd.Context())
			if err != nil {
				return reported(err)
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), tags)
			}
			if len(tags) == 0 {
				nothing(cmd, "No rating carried a tag yet. `ypl rate --tag <tag>` adds one.")
				return nil
			}
			rows := make([][]string, len(tags))
			for i, tag := range tags {
				rows[i] = []string{strconv.FormatInt(tag.Videos, 10), tag.Tag}
			}
			table(cmd.OutOrStdout(), a.width(cmd.OutOrStdout()), []column{whole("MIXES"), prose("TAG")}, rows)
			return nil
		},
	}
	addJSON(cmd, &asJSON, "the tags")
	return cmd
}
