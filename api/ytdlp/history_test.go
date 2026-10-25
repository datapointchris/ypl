package ytdlp

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The line yt-dlp 2026.10.21 writes for a history read with no signed-in
// cookies.
const signedOutLine = "ERROR: [youtube:history] Login details are needed to download this content. Use --cookies-from-browser or --cookies for the authentication. See  https://github.com/yt-dlp/yt-dlp/wiki/FAQ#how-do-i-pass-cookies-to-yt-dlp  for how to manually pass cookies."

func TestAHistoryReadNeedingASessionIsSignedOutAndTheBotCheckIsARateLimit(t *testing.T) {
	exit := errors.New("exit status 1")
	if err := historyRefusal(signedOutLine+"\n", exit); !errors.Is(err, ErrSignedOut) {
		t.Errorf("the login line read as %v, want ErrSignedOut", err)
	}
	botCheck := "ERROR: [youtube:history] Sign in to confirm you're not a bot. Use --cookies for the authentication.\n"
	if err := historyRefusal(botCheck, exit); !errors.Is(err, ErrRateLimited) {
		t.Errorf("the bot check read as %v, want ErrRateLimited", err)
	}
}

// The field names are the history page's: a lockup per video,
// with the resume point as a progress bar on its thumbnail.
func TestAHistoryPageGivesEachVideoItsResumePercent(t *testing.T) {
	dir := t.TempDir()
	page := `<script>var ytInitialData = {"contents":[` +
		`{"lockupViewModel":{"contentId":"bSnA4mMcVHU","contentImage":{"overlays":[{"thumbnailBottomOverlayViewModel":{"progressBar":{"thumbnailOverlayProgressBarViewModel":{"startPercent":46}}}}]}}},` +
		`{"lockupViewModel":{"contentId":"8HTSnGLB42A","contentImage":{"overlays":[]}}}` +
		`]};</script>`
	if err := os.WriteFile(filepath.Join(dir, "history_https_-_www.youtube.com_feed_history.dump"), []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resumePercents(dir)
	if err != nil || len(got) != 1 || got["bSnA4mMcVHU"] != 46 {
		t.Errorf("resume percents %v, want bSnA4mMcVHU at 46 and nothing for the video with no bar", got)
	}
}
