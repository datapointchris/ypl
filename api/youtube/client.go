// Package youtube reads the playlists a channel owns, and the items in each,
// through the YouTube Data API v3, acting as the channel with an OAuth refresh
// token.
//
// The credentials come from a Google Cloud project with the YouTube Data API v3
// enabled:
//
//  1. Create an OAuth client of type "TVs and Limited Input devices". Its id and
//     secret are YOUTUBE_CLIENT_ID and YOUTUBE_CLIENT_SECRET.
//  2. Publish the app, so its status is "In production". An app in "Testing"
//     issues refresh tokens that expire after 7 days. Publishing asks for a home
//     page, a privacy policy link and a terms link on an authorized domain on the
//     branding page. A logo would send the app to verification.
//  3. Run cmd/authorize-youtube with those two variables set. Open the page it
//     prints, enter the code, and choose the channel's own account: a brand
//     account's playlists belong to the brand account, not to the Google account
//     that manages it. The refresh token it prints is YOUTUBE_REFRESH_TOKEN.
//
// A refresh token keeps the scope its grant gave, and stops working after six
// months unused.
package youtube

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/endpoints"
	ytapi "google.golang.org/api/youtube/v3"
)

// Scope is the OAuth scope a grant gives this service: managing the channel's
// YouTube account, which covers editing its playlists as well as reading them.
const Scope = ytapi.YoutubeScope

// Client is the OAuth client this service is registered as.
type Client struct {
	ID     string
	Secret string
}

// Credentials are the client and the refresh token the channel's owner granted
// it. The refresh token is the long-lived secret.
type Credentials struct {
	Client       Client
	RefreshToken string
}

// ClientFromEnv reads YOUTUBE_CLIENT_ID and YOUTUBE_CLIENT_SECRET, and names
// every one that is unset.
func ClientFromEnv() (Client, error) {
	values, err := requireEnv("YOUTUBE_CLIENT_ID", "YOUTUBE_CLIENT_SECRET")
	if err != nil {
		return Client{}, err
	}
	return Client{ID: values[0], Secret: values[1]}, nil
}

// CredentialsFromEnv reads YOUTUBE_CLIENT_ID, YOUTUBE_CLIENT_SECRET and
// YOUTUBE_REFRESH_TOKEN, and names every one that is unset.
func CredentialsFromEnv() (Credentials, error) {
	values, err := requireEnv("YOUTUBE_CLIENT_ID", "YOUTUBE_CLIENT_SECRET", "YOUTUBE_REFRESH_TOKEN")
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{Client: Client{ID: values[0], Secret: values[1]}, RefreshToken: values[2]}, nil
}

// requireEnv is the value of each named variable, in order, or
// ErrMissingCredentials naming every one that is unset.
func requireEnv(names ...string) ([]string, error) {
	values := make([]string, len(names))
	var missing []string
	for i, name := range names {
		values[i] = os.Getenv(name)
		if values[i] == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, fmt.Errorf("%w: %s", ErrMissingCredentials, strings.Join(missing, ", "))
	}
	return values, nil
}

// oauthConfig is client at Google's endpoints, asking for Scope.
func oauthConfig(client Client) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     client.ID,
		ClientSecret: client.Secret,
		Endpoint:     endpoints.Google,
		Scopes:       []string{Scope},
	}
}

// Authorize runs Google's device flow for client and returns the refresh token
// the channel's owner grants. show receives the page to open and the code to
// enter there, and Authorize then waits for the grant or for ctx to end.
func Authorize(ctx context.Context, client Client, show func(verificationURL, userCode string) error) (string, error) {
	return authorize(ctx, oauthConfig(client), show)
}

func authorize(ctx context.Context, config *oauth2.Config, show func(verificationURL, userCode string) error) (string, error) {
	device, err := config.DeviceAuth(ctx)
	if err != nil {
		return "", fmt.Errorf("start the device flow: %w", err)
	}
	if err := show(device.VerificationURI, device.UserCode); err != nil {
		return "", err
	}
	token, err := config.DeviceAccessToken(ctx, device)
	if err != nil {
		return "", fmt.Errorf("wait for the grant: %w", err)
	}
	if token.RefreshToken == "" {
		return "", fmt.Errorf("%w: no refresh token", ErrIncompleteGrant)
	}
	granted, _ := token.Extra("scope").(string)
	if !slices.Contains(strings.Fields(granted), Scope) {
		return "", fmt.Errorf("%w: granted %q, not %s", ErrIncompleteGrant, granted, Scope)
	}
	return token.RefreshToken, nil
}
