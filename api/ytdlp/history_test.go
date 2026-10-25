package ytdlp

import (
	"errors"
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
