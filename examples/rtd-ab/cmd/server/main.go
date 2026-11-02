// SPDX-License-Identifier: Apache-2.0

// Command rtd-ab is an analysis-only A/B experiment packaged as an Index Cloud
// gRPC RTD service. It implements the ARTF RTBExtensionPoint GetMutations RPC
// (always returning zero mutations), the gRPC health service, and the required
// HTTP metrics/health endpoints. An auction-id router (internal/router)
// assigns each request to a control or test arm, and the arm's analysis records
// what it would have decided as rtd_ab_* metrics (internal/ab).
//
// The mix and the parameters of each arm come from the models folder
// (MODEL_PATH/ab-config.json and the model directories it names, see
// internal/abmodel). The test share baked into the binary at build time
// (-X main.bakedTestPct) and the built-in models are the fallback. The share is
// aligned to the nearest 1/16. A request goes to test when the last hex digit of
// the auction id (bid_request.id before the first underscore) is below the number
// of test groups. RTD_AB_TEST_PCT overrides the share for local harness runs only.
//
// Ports/paths come from the environment Index sets at start ("Building your
// container → Environment variables"):
//
//	GRPC_PORT   gRPC RTBExtensionPoint + gRPC health service (default 50051)
//	SERVER_PORT HTTP /metrics, /heartbeat (liveness), /healthcheck (readiness)
//	MODEL_PATH  directory model data is mounted into (checked, not required here)
//
// Run with -healthcheck to self-probe /healthcheck and exit 0/1 (Docker
// HEALTHCHECK; the final image has no shell).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/indexexchange/rtd-ab/internal/ab"
	"github.com/indexexchange/rtd-ab/internal/abinit"
	"github.com/indexexchange/rtd-ab/internal/abmodel"
	"github.com/indexexchange/rtd-ab/internal/agent"
	"github.com/indexexchange/rtd-ab/internal/config"
	"github.com/indexexchange/rtd-ab/internal/health"
	"github.com/indexexchange/rtd-ab/internal/metrics"
	"github.com/indexexchange/rtd-ab/internal/model"
	"github.com/indexexchange/rtd-ab/internal/router"
	"github.com/indexexchange/rtd-ab/internal/runtimetune"
	pb "github.com/indexexchange/rtd-ab/pkg/pb/artf"

	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

var (
	version = "dev"
	// bakedTestPct is the test-arm share in percent, set at build time with
	// -ldflags "-X main.bakedTestPct=<pct>" (Makefile AB_TEST_PCT).
	bakedTestPct = abinit.DefaultPct
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe the local /healthcheck endpoint and exit 0/1")
	flag.Parse()

	cfg := config.Load()
	if cfg.APIVersion == "0.1.0" && version != "dev" {
		cfg.APIVersion = version
	}
	if *healthcheck {
		os.Exit(selfProbe(cfg.ServerPort))
	}
	if err := run(cfg); err != nil {
		log.Fatalf("rtd-ab: %v", err)
	}
}

func run(cfg config.Config) error {
	// The models folder decides the mix and the parameters; the baked percentage and
	// the built-in models are the fallback when it has no (valid) ab-config.json.
	mi := model.Check(cfg.ModelPath)
	root := mi.Dir
	if root == "" {
		root = cfg.ModelPath
	}
	loaded := abmodel.Load(root)
	base := bakedTestPct
	if loaded.Source == abmodel.SourceFile {
		base = loaded.TestPct
	}
	split := abinit.Resolve(base, os.Getenv)
	log.Printf("rtd-ab %s starting: grpc=:%d http=:%d model_path=%s models=%s control=%s test=%s test_pct=%s (hex digits 0..%x of 16, requested %s, baked %s) summary_every=%s",
		cfg.APIVersion, cfg.GRPCPort, cfg.ServerPort, cfg.ModelPath, loaded.Source, loaded.Control, loaded.Test, split.Pct, int(split.Groups)-1, split.Requested, bakedTestPct, cfg.SummaryEvery)
	if loaded.Err != nil {
		log.Printf("rtd-ab: ERROR %v; using the built-in models at the baked %s%%", loaded.Err, bakedTestPct)
	}
	for _, l := range split.Lines {
		log.Print("rtd-ab: " + l)
	}

	// Match the Go runtime to the cgroup limits (cuts GC overhead and avoids CPU
	// oversubscription/throttling). Env overrides win. See internal/runtimetune.
	log.Printf("runtime tuning: %s", runtimetune.Apply())

	reg := metrics.New(cfg.APIVersion)
	eng := ab.NewEngine(router.New(split.Groups), split.Pct, ab.NewContentAnalysis(loaded.Content))
	eng.SetModels(loaded.Source, loaded.Control, loaded.Test)
	reg.AddExporter(eng.WriteMetrics)
	checker := health.New(cfg.APIVersion)
	httpSrv := startHTTP(cfg.ServerPort, reg, checker)

	// Report the delivered model (models are optional; rtd-ab reads only ab-config.json
	// and the parameter files it names).
	logModel(cfg.ModelPath, mi)
	reg.SetModel(mi.Present, mi.Label())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // stops the summary goroutine on any return path
	go eng.RunSummary(ctx, cfg.SummaryEvery, log.Printf)
	ag := agent.New(cfg.APIVersion, eng)

	grpcSrv := grpc.NewServer(grpc.UnaryInterceptor(metricsInterceptor(reg)))
	pb.RegisterRTBExtensionPointServer(grpcSrv, ag)
	healthpb.RegisterHealthServer(grpcSrv, checker.GRPCServer())
	reflection.Register(grpcSrv)

	lis, err := net.Listen("tcp", ":"+strconv.Itoa(cfg.GRPCPort))
	if err != nil {
		return fmt.Errorf("grpc listen: %w", err)
	}
	grpcErr := make(chan error, 1)
	go func() { grpcErr <- grpcSrv.Serve(lis) }()

	// No model to warm; ready immediately.
	checker.SetReady(true)
	reg.SetReady(true)
	log.Printf("rtd-ab ready (analysis only: every response returns zero mutations)")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-stop:
		log.Printf("received %s, shutting down", sig)
	case err := <-grpcErr:
		if err != nil {
			return fmt.Errorf("grpc serve: %w", err)
		}
	}
	checker.SetReady(false)
	reg.SetReady(false)
	grpcSrv.GracefulStop()
	shCtx, shCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shCancel()
	_ = httpSrv.Shutdown(shCtx)
	log.Printf("rtd-ab stopped")
	return nil
}

// logModel reports the model-presence check so onboarding can confirm the
// model-delivery step wired up. Models are optional; absence is not an error.
func logModel(path string, mi model.Info) {
	switch {
	case mi.Err != nil:
		log.Printf("model check at %s: %v (models are optional; continuing as no-op)", path, mi.Err)
	case mi.Dir == "":
		log.Printf("model check at %s: no model found (optional; container runs as a no-op)", path)
	case !mi.Present:
		log.Printf("model check at %s: manifest %q INCOMPLETE, missing %v (optional)", mi.Dir, mi.Name, mi.Missing)
	default:
		log.Printf("model present: %q files=%d %v (at %s)", mi.Name, len(mi.Files), mi.Files, mi.Dir)
	}
}

func startHTTP(port int, reg *metrics.Registry, checker *health.Checker) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", reg.Handler())
	mux.HandleFunc("/heartbeat", checker.LivenessHandler)
	mux.HandleFunc("/healthcheck", checker.ReadinessHandler)
	srv := &http.Server{Addr: ":" + strconv.Itoa(port), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http serve: %v", err)
		}
	}()
	return srv
}

// metricsInterceptor records the required rpc_server_duration histogram for every
// RPC, tagged with the gRPC status code (numeric) and method.
func metricsInterceptor(reg *metrics.Registry) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		durSec := time.Since(start).Seconds() // metric is in seconds per the doc
		code := int(status.Code(err))
		errType := ""
		if err != nil {
			errType = status.Code(err).String()
		}
		service, method := splitMethod(info.FullMethod)
		reg.ObserveRPC(service, method, strconv.Itoa(code), errType, durSec)
		// Log errors only (per-request happy-path logging is opt-in in the agent),
		// so normal traffic does not flood the logs during validation or in prod.
		if err != nil {
			log.Printf("rtd-ab rpc error: method=%s code=%s: %v", method, status.Code(err), err)
		}
		return resp, err
	}
}

// splitMethod turns "/pkg.Service/Method" into ("Service", "Method").
func splitMethod(full string) (service, method string) {
	full = strings.TrimPrefix(full, "/")
	i := strings.LastIndex(full, "/")
	if i < 0 {
		return "", full
	}
	svc := full[:i]
	method = full[i+1:]
	if j := strings.LastIndex(svc, "."); j >= 0 {
		svc = svc[j+1:] // short service name, e.g. RTBExtensionPoint
	}
	return svc, method
}

func selfProbe(port int) int {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthcheck", port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return 0
	}
	fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
	return 1
}
