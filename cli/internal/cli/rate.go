package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
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
// picker offers before searching the library.
const historyOffered = 5

// searchTheLibrary is the picker's option for a video the history does not
// hold, a value no video id can be.
const searchTheLibrary = "search"

func (a *app) rateCommand() *cobra.Command {
	var (
		like, dislike, note bool
		message             string
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
			"At a terminal with no video named, it offers the newest mixes in YouTube's watch\n" +
			"history, which every player signed into the channel's account writes to, then a\n" +
			"search of the library. With no --like, --dislike or --note it then asks for the\n" +
			"kind, the message and the tags.\n\n" +
			"Without a terminal, or under --no-input, name the video and give the kind.",
		Example: "  ypl rate                                       pick what is playing, then rate it\n" +
			"  ypl rate --like -m 'great closing hour'        pick it, and like it\n" +
			"  ypl rate dQw4w9WgXcQ --note -m 'too much talk'  a note on one mix\n" +
			"  ypl rate 'mayan warrior' --dislike --tag dark  by part of its title",
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
			var video api.PlayedVideo
			if len(args) == 1 {
				video, err = namedVideo(cmd.Context(), client, args[0])
			} else {
				video, err = pickVideo(cmd, client)
			}
			if err != nil {
				return reported(err)
			}
			rating := api.NewRating{VideoID: video.ID, Kind: kind, Tags: tags}
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

// pickVideo offers the newest library videos in the watch history and a
// search of the library, and is the one chosen. A history that cannot be read
// says why on stderr and goes straight to the search, so a session that
// expired never stops a rating.
func pickVideo(cmd *cobra.Command, client *api.Client) (api.PlayedVideo, error) {
	ctx := cmd.Context()
	entries, err := client.History(ctx, historyOffered)
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "The watch history could not be read: %v\n", err)
		entries = nil
	}
	if len(entries) > 0 {
		options := make([]huh.Option[string], 0, len(entries)+1)
		for i, entry := range entries {
			label := entry.Video.Title
			if i == 0 {
				label += "  (newest in your history)"
			}
			options = append(options, huh.NewOption(label, entry.Video.ID))
		}
		options = append(options, huh.NewOption("Search the library…", searchTheLibrary))
		chosen := entries[0].Video.ID
		if err := huh.NewSelect[string]().Title("Which mix?").Options(options...).Value(&chosen).Run(); err != nil {
			return api.PlayedVideo{}, aborted(err)
		}
		for _, entry := range entries {
			if entry.Video.ID == chosen {
				return entry.Video, nil
			}
		}
	}
	return searchVideo(ctx, client)
}

// searchVideo is a video chosen from the whole library, filtered as it is
// typed.
func searchVideo(ctx context.Context, client *api.Client) (api.PlayedVideo, error) {
	videos, err := client.ListVideos(ctx, api.VideoFilter{Sort: "title"})
	if err != nil {
		return api.PlayedVideo{}, err
	}
	if len(videos) == 0 {
		return api.PlayedVideo{}, errors.New("the library holds no video")
	}
	options := make([]huh.Option[string], len(videos))
	for i, video := range videos {
		options[i] = huh.NewOption(video.Title+"  · "+video.ChannelTitle, video.ID)
	}
	var chosen string
	if err := huh.NewSelect[string]().Title("Which mix? Type / to filter").Options(options...).Filtering(true).Height(15).Value(&chosen).Run(); err != nil {
		return api.PlayedVideo{}, aborted(err)
	}
	for _, video := range videos {
		if video.ID == chosen {
			return api.PlayedVideo{ID: video.ID, Title: video.Title, ChannelTitle: video.ChannelTitle}, nil
		}
	}
	return api.PlayedVideo{}, errors.New("no video was chosen")
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
	fields = append(fields, huh.NewInput().Title("New tags").Description("Optional, separated by commas").Value(&fresh))
	if err := huh.NewForm(huh.NewGroup(fields...)).Run(); err != nil {
		return aborted(err)
	}
	rating.Kind = kind
	rating.Message = nil
	if strings.TrimSpace(message) != "" {
		rating.Message = &message
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
	cmd.AddCommand(a.ratingsListCommand())
	return cmd
}

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
func printRatingsOf(out io.Writer, width int, ratings []api.Rating) {
	rows := make([][]string, len(ratings))
	for i, r := range ratings {
		rows[i] = []string{r.RatedTs, r.Kind, text(r.Message), strings.Join(r.Tags, ", ")}
	}
	table(out, width, []column{whole("WHEN"), whole("KIND"), prose("MESSAGE"), detail("TAGS")}, rows)
}

func printRatings(out io.Writer, width int, ratings []api.Rating) {
	rows := make([][]string, len(ratings))
	for i, r := range ratings {
		rows[i] = []string{r.RatedTs, r.Kind, r.Video.Title, text(r.Message), strings.Join(r.Tags, ", ")}
	}
	table(out, width, []column{whole("WHEN"), whole("KIND"), prose("TITLE"), detail("MESSAGE"), detail("TAGS")}, rows)
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
