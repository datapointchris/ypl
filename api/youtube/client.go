package youtube

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/endpoints"
	"google.golang.org/api/option"
	ytapi "google.golang.org/api/youtube/v3"
)

// ErrMissingCredentials is the refusal for an environment that lacks one of
// the three credential variables.
var ErrMissingCredentials = errors.New("missing YouTube credentials")

// Credentials are the OAuth client this service is registered as, and the
// refresh token the channel's owner granted it. The refresh token is the
// long-lived secret.
type Credentials struct {
	ClientID     string
	ClientSecret string
	RefreshToken string
}

// CredentialsFromEnv reads YOUTUBE_CLIENT_ID, YOUTUBE_CLIENT_SECRET and
// YOUTUBE_REFRESH_TOKEN, and names every one that is unset.
func CredentialsFromEnv() (Credentials, error) {
	creds := Credentials{
		ClientID:     os.Getenv("YOUTUBE_CLIENT_ID"),
		ClientSecret: os.Getenv("YOUTUBE_CLIENT_SECRET"),
		RefreshToken: os.Getenv("YOUTUBE_REFRESH_TOKEN"),
	}
	var missing []string
	for name, value := range map[string]string{
		"YOUTUBE_CLIENT_ID":     creds.ClientID,
		"YOUTUBE_CLIENT_SECRET": creds.ClientSecret,
		"YOUTUBE_REFRESH_TOKEN": creds.RefreshToken,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return Credentials{}, fmt.Errorf("%w: %s", ErrMissingCredentials, strings.Join(missing, ", "))
	}
	return creds, nil
}

// NewService is a Data API client acting with creds. oauth2 exchanges the
// refresh token for an access token, and again each time one expires.
func NewService(ctx context.Context, creds Credentials) (*ytapi.Service, error) {
	config := &oauth2.Config{
		ClientID:     creds.ClientID,
		ClientSecret: creds.ClientSecret,
		Endpoint:     endpoints.Google,
		Scopes:       []string{ytapi.YoutubeScope},
	}
	source := config.TokenSource(ctx, &oauth2.Token{RefreshToken: creds.RefreshToken})
	service, err := ytapi.NewService(ctx, option.WithTokenSource(source))
	if err != nil {
		return nil, fmt.Errorf("create the YouTube client: %w", err)
	}
	return service, nil
}
