// Command harbourd is the Harbour daemon: HTTP API + N workers.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/Celaris-dev1/Harbour-/internal/api"
	"github.com/Celaris-dev1/Harbour-/internal/demo"
	"github.com/Celaris-dev1/Harbour-/internal/gatetool"
	"github.com/Celaris-dev1/Harbour-/internal/ledger"
	"github.com/Celaris-dev1/Harbour-/internal/registry"
	"github.com/Celaris-dev1/Harbour-/internal/store"
	"github.com/Celaris-dev1/Harbour-/internal/warrant"
	"github.com/Celaris-dev1/Harbour-/internal/worker"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "keys" {
		if err := cmdKeys(os.Args[2:]); err != nil {
			slog.Error("keys", "err", err)
			os.Exit(1)
		}
		return
	}
	addr := flag.String("addr", env("HARBOUR_ADDR", ":8450"), "listen address")
	dbURL := flag.String("db", env("HARBOUR_DATABASE_URL", "postgres://postgres:postgres@localhost:5432/harbour"), "postgres url")
	workers := flag.Int("workers", 2, "worker goroutines (0 = API only)")
	node := flag.String("node", env("HARBOUR_NODE", hostname()), "node id used in lease owner names")
	ttl := flag.Duration("lease-ttl", 10*time.Second, "worker lease TTL")
	dir := flag.String("demo-dir", env("HARBOUR_DEMO_DIR", "./harbour-data"), "directory for demo fs.append tool")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := store.Open(ctx, *dbURL, ledger.FromEnv(ctx))
	if err != nil {
		slog.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()
	reg := registry.New()
	demo.Register(reg, *dir)
	reg.AddTool(&gatetool.Tool{}) // gate.verify: runs `gate run --format json` as an effect
	wclient := warrant.FromEnv()
	if wclient != nil {
		slog.Info("warrant integration enabled", "url", os.Getenv("WARRANT_URL"))
	}

	var wg sync.WaitGroup
	for i := 0; i < *workers; i++ {
		w := worker.New(fmt.Sprintf("%s-%d-%d", *node, os.Getpid(), i), st, reg)
		w.LeaseTTL = *ttl
		w.Exec.Warrant = wclient
		installCrashHook(w)
		wg.Add(1)
		go func() { defer wg.Done(); w.Loop(ctx) }()
	}
	srv := &http.Server{Addr: *addr, Handler: (&api.Server{Store: st, Token: os.Getenv("HARBOUR_TOKEN")}).Handler()}
	go func() {
		<-ctx.Done()
		sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		srv.Shutdown(sctx)
	}()
	slog.Info("harbourd listening", "addr", *addr, "workers", *workers, "agents", reg.AgentNames())
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("serve", "err", err)
	}
	wg.Wait()
}

// installCrashHook: HARBOUR_TEST_CRASH_AFTER_EFFECT_STEP=N makes the process
// die with exit 137 right after step N's side effect, before its result row is
// written. Used by the process-level crash test; never set it in production.
func installCrashHook(w *worker.Worker) {
	v := os.Getenv("HARBOUR_TEST_CRASH_AFTER_EFFECT_STEP")
	if v == "" {
		return
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return
	}
	w.Exec.Hooks.AfterEffect = func(e *store.Effect) error {
		if e.Step == n {
			slog.Warn("simulated crash: exiting after effect, before result", "step", n)
			os.Exit(137)
		}
		return nil
	}
}

func hostname() string {
	h, _ := os.Hostname()
	if h == "" {
		h = "node"
	}
	return h
}
