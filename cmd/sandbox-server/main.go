/*
Copyright 2026 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command sandbox-server is the in-pod HTTP server of the kode-gopher
// sandbox image. It implements the agent-sandbox runtime protocol (the
// one sigs.k8s.io/agent-sandbox's clients speak through the sandbox
// router; see examples/python-runtime-sandbox upstream):
//
//	GET  /                 health check
//	POST /execute          run {"command": ...} via sh -c in the workdir
//	POST /upload           multipart "file" -> <workdir>/<basename>
//	GET  /download/<path>  file content
//	GET  /list/<path>      directory entries
//	GET  /exists/<path>    {"path", "exists"}
//
// <path> is percent-encoded (the clients encode "/" as %2F) and always
// resolves inside the workdir.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", envOr("SANDBOX_ADDR", ":8888"), "listen address")
	workdir := flag.String("workdir", envOr("SANDBOX_WORKDIR", "/app"), "directory commands run in and files resolve against")
	logLevel := flag.String("log-level", envOr("SANDBOX_LOG_LEVEL", "info"), "debug|info|warn|error")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(*logLevel)}))

	srv, err := newServer(*workdir, log)
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Info("listening", "addr", *addr, "workdir", srv.workdir)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listen failed", "err", err)
			stop()
		}
	}()
	<-ctx.Done()

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown failed", "err", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}
