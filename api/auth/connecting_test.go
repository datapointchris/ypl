package auth

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func connecting(issuer string) *Connecting {
	c := NewConnecting(issuer, "ypl-cli-")
	c.firstRetry, c.lastRetry = time.Millisecond, 4*time.Millisecond
	return c
}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestATokenBeforeTheProviderIsReadIsRefusedAsUnavailable(t *testing.T) {
	p := newTestProvider(t)
	c := connecting(p.server.URL)

	if _, err := c.Verify(context.Background(), p.token(t)); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("Verify before Run = %v, want ErrProviderUnavailable", err)
	}
	if c.Ready() {
		t.Fatal("Ready before Run")
	}
}

func TestAProviderDownAtFirstIsReadOnceItAnswers(t *testing.T) {
	p := newTestProvider(t)
	p.set(func(p *testProvider) { p.discoveryFailures = 3 })
	c := connecting(p.server.URL)

	done := make(chan struct{})
	go func() {
		c.Run(context.Background(), quiet())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not read a provider that answered on its fourth try")
	}
	if !c.Ready() {
		t.Fatal("not Ready after Run read the provider")
	}
	if _, err := c.Verify(context.Background(), p.token(t)); err != nil {
		t.Fatalf("Verify after Run: %v", err)
	}
}

func TestAMisconfiguredIssuerStopsRetrying(t *testing.T) {
	p := newTestProvider(t)
	c := connecting(p.server.URL + "/")

	done := make(chan struct{})
	go func() {
		c.Run(context.Background(), quiet())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run kept retrying an issuer whose discovery document names another")
	}
	if c.Ready() {
		t.Fatal("Ready with a misconfigured issuer")
	}
}

func TestAStartCanceledBeforeTheProviderAnswersReportsNoFailure(t *testing.T) {
	p := newTestProvider(t)
	c := connecting(p.server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var logs bytes.Buffer

	c.Run(ctx, slog.New(slog.NewTextHandler(&logs, nil)))
	if logs.Len() != 0 {
		t.Fatalf("Run on a canceled context logged %s, want nothing", logs.String())
	}
}

func TestRunEndsWithItsContextWhileTheProviderIsDown(t *testing.T) {
	p := newTestProvider(t)
	p.server.Close()
	c := connecting(p.server.URL)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		c.Run(ctx, quiet())
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not end with its context")
	}
}
