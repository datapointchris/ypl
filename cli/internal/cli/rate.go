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
		up, down bool
		note     string
		tags     []string
		asJSON   bool
	)
	cmd := &cobra.Command{
		Use:     "rate [video]",
		Short:   "Rate a mix up or down, or leave a note and tags on it",
		GroupID: groupRatings,
		Long: "At a terminal with no video named, it offers the newest mixes in YouTube's watch\n" +
			"history, which every player signed into the channel's account writes to, then a\n" +
			"search of the library. It asks for each of the verdict, note and tags that no\n" +
			"flag gave, and each may be left out, though one has to be given. A rating with\n" +
			"no verdict leaves the mix's verdict as it was.\n\n" +
			"Without a terminal, or under --no-input, name the video and give the flags.",
		Example: "  ypl rate                                    pick what is playing, then rate it\n" +
			"  ypl rate --up                               pick it, and rate it up\n" +
			"  ypl rate dQw4w9WgXcQ --note 'too much talk'  a note, no verdict\n" +
			"  ypl rate 'mayan warrior' --down --tag dark  by part of its title",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if up && down {
				return goclikit.UsageError(errors.New("--up and --down cannot both be given"))
			}
			interactive := !forbidsInput(cmd) && a.terminalIn(cmd)
			given := up || down || cmd.Flags().Changed("note") || cmd.Flags().Changed("tag")
			if !interactive && len(args) == 0 {
				return goclikit.UsageError(errors.New("name the video; only a terminal can pick it from the watch history"))
			}
			if !interactive && !given {
				return goclikit.UsageError(errors.New("give --up, --down, --note or --tag; only a terminal is asked for them"))
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
			rating := api.NewRating{VideoID: video.ID, Tags: tags}
			switch {
			case up:
				rating.Verdict = ptr("up")
			case down:
				rating.Verdict = ptr("down")
			}
			if cmd.Flags().Changed("note") {
				rating.Note = &note
			}
			if interactive && !given {
				if err := askRating(cmd.Context(), client, video, &rating); err != nil {
					return err
				}
			}
			if rating.Verdict == nil && (rating.Note == nil || strings.TrimSpace(*rating.Note) == "") && len(rating.Tags) == 0 {
				nothing(cmd, "Nothing was rated: no verdict, note or tag was given.")
				return exitCode(1)
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
	cmd.Flags().BoolVar(&up, "up", false, "Rate it up")
	cmd.Flags().BoolVar(&down, "down", false, "Rate it down")
	cmd.Flags().StringVar(&note, "note", "", "A note on the mix")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "A tag for the mix (repeatable)")
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

// askRating asks for the verdict, a note and tags, each of which may be left
// out. Tags already used are offered to choose from, beside a line for new
// ones.
func askRating(ctx context.Context, client *api.Client, video api.PlayedVideo, rating *api.NewRating) error {
	verdict := "up"
	note := ""
	fresh := ""
	var chosen []string
	used, err := client.ListTags(ctx)
	if err != nil {
		return reported(err)
	}
	fields := []huh.Field{
		huh.NewSelect[string]().Title(video.Title).Description("Your verdict").Options(
			huh.NewOption("Up", "up"),
			huh.NewOption("Down", "down"),
			huh.NewOption("No verdict, just a note or tags", ""),
		).Height(5).Value(&verdict),
		huh.NewText().Title("Note").Description("Optional").Value(&note),
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
	if verdict != "" {
		rating.Verdict = &verdict
	}
	if strings.TrimSpace(note) != "" {
		rating.Note = &note
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

func ptr[T any](v T) *T { return &v }

// describeRating is one line saying what a rating recorded.
func describeRating(r api.Rating) string {
	parts := []string{r.Video.Title}
	if r.Verdict != nil {
		parts = append(parts, "rated "+*r.Verdict)
	}
	if r.Note != nil {
		parts = append(parts, "noted")
	}
	if len(r.Tags) > 0 {
		parts = append(parts, "tagged "+strings.Join(r.Tags, ", "))
	}
	return strings.Join(parts, " · ")
}

func (a *app) ratingsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ratings",
		Short:   "What you said about mixes",
		GroupID: groupRatings,
		Long:    "`ypl rate` records a rating. A mix's verdict is its latest up or down.",
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

func printRatings(out io.Writer, width int, ratings []api.Rating) {
	rows := make([][]string, len(ratings))
	for i, r := range ratings {
		rows[i] = []string{r.RatedTs, text(r.Verdict), r.Video.Title, strings.Join(r.Tags, ", "), text(r.Note)}
	}
	table(out, width, []column{whole("WHEN"), whole("VERDICT"), prose("TITLE"), detail("TAGS"), detail("NOTE")}, rows)
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
