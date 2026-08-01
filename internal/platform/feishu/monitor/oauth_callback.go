package monitor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const oauthCallbackShutdownTimeout = 5 * time.Second

type oauthCallbackServer struct {
	server   *http.Server
	listener net.Listener
	errCh    chan error
}

func startResourceAccessOAuthServer(ctx context.Context, manager *resourceAccessManager) (*oauthCallbackServer, error) {
	if manager == nil || !manager.oauthEnabled() {
		return nil, nil
	}
	redirect, err := url.Parse(manager.oauth.RedirectURI)
	if err != nil {
		return nil, fmt.Errorf("parse feishu OAuth redirect URI: %w", err)
	}
	callbackPath := strings.TrimSpace(redirect.Path)
	if callbackPath == "" {
		callbackPath = "/"
	}
	listener, err := net.Listen("tcp", manager.oauth.ListenAddress)
	if err != nil {
		return nil, fmt.Errorf("listen feishu OAuth callback address %s: %w", manager.oauth.ListenAddress, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, manager.HandleOAuthCallback)
	server := &oauthCallbackServer{
		server: &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		},
		listener: listener,
		errCh:    make(chan error, 1),
	}
	go func() {
		err := server.server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		server.errCh <- err
		close(server.errCh)
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), oauthCallbackShutdownTimeout)
		defer cancel()
		if err := server.server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			feishuLog.Warn(context.Background(), "shutdown feishu OAuth callback server account=%s: %v", manager.account.ID, err)
		}
	}()
	feishuLog.Info(ctx, "started feishu OAuth callback server account=%s listen=%s path=%s", manager.account.ID, manager.oauth.ListenAddress, callbackPath)
	return server, nil
}

func (s *oauthCallbackServer) Errors() <-chan error {
	if s == nil {
		return nil
	}
	return s.errCh
}

func (s *oauthCallbackServer) Close() error {
	if s == nil || s.server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), oauthCallbackShutdownTimeout)
	defer cancel()
	err := s.server.Shutdown(ctx)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
