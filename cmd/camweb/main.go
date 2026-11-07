// Command camweb serves a low-latency web UI for a Raspberry Pi camera. It
// runs the camera helper (camhelper.py, Picamera2) and talks to it over Unix
// sockets (docs/HELPER_API.md).
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mhbvr/camera_rpi/camhelper"
	"github.com/mhbvr/camera_rpi/internal/camera"
	"github.com/mhbvr/camera_rpi/internal/helper"
	"github.com/mhbvr/camera_rpi/internal/stream"
	"github.com/mhbvr/camera_rpi/internal/web"
)

func main() {
	var (
		listen     = flag.String("listen", ":8080", "HTTP listen address")
		dataDir    = flag.String("data", "./data", "data directory (config.json)")
		captures   = flag.String("captures", "", "directory for snapshot files while they are downloaded; each is deleted at once (default: RAM, "+defaultCapturesDir()+")")
		runtimeDir = flag.String("runtime-dir", "", "directory for the helper sockets (default $XDG_RUNTIME_DIR/camweb or <data>/run)")
		mode       = flag.String("helper", "spawn", "spawn: start and supervise the helper; external: only listen for one")
		helperCmd  = flag.String("helper-cmd", "", "helper command (default: python3 <embedded camhelper.py>); --ctl/--video are appended")
		python     = flag.String("python", "python3", "Python interpreter for the embedded helper")
		hflip      = flag.Bool("hflip", false, "flip the image horizontally")
		vflip      = flag.Bool("vflip", false, "flip the image vertically (with -hflip: rotate 180°)")
		tlsCert    = flag.String("tls-cert", "", "serve HTTPS with this certificate (PEM); see `make cert`")
		tlsKey     = flag.String("tls-key", "", "private key (PEM) for -tls-cert")
		corsOrigin = flag.String("cors-origin", "", "allow a UI served from this origin to use the API (Access-Control-Allow-Origin; \"*\" = any)")
	)
	flag.Parse()
	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)

	// Load the certificate up front so a bad path fails at startup.
	var tlsConfig *tls.Config
	if (*tlsCert == "") != (*tlsKey == "") {
		logger.Fatal("-tls-cert and -tls-key must be given together")
	}
	if *tlsCert != "" {
		pair, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			logger.Fatalf("TLS: %v", err)
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	}

	if *runtimeDir == "" {
		if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
			*runtimeDir = filepath.Join(x, "camweb")
		} else {
			*runtimeDir = filepath.Join(*dataDir, "run")
		}
	}
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		logger.Fatal(err)
	}
	cfgPath := filepath.Join(*dataDir, "config.json")
	cfg, err := camera.LoadConfig(cfgPath)
	if err != nil {
		logger.Printf("config: %v (using defaults)", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hub := stream.NewHub(30)
	mgr := camera.NewManager(cfg, hub, camera.Options{
		ConfigPath:  cfgPath,
		CapturesDir: capturesDir(*captures),
		HFlip:       *hflip,
		VFlip:       *vflip,
		Log:         logger,
	})

	lis, err := helper.Listen(*runtimeDir, logger)
	if err != nil {
		logger.Fatal(err)
	}
	defer lis.Close()
	logger.Printf("helper sockets: %s, %s", lis.CtlPath(), lis.VideoPath())

	var sup *helper.Supervisor
	switch *mode {
	case "spawn":
		args, err := helperArgs(*helperCmd, *python, *runtimeDir)
		if err != nil {
			logger.Fatal(err)
		}
		args = append(args, "--ctl", lis.CtlPath(), "--video", lis.VideoPath())
		sup = &helper.Supervisor{Args: args, Log: logger}
		go sup.Run(ctx)
	case "external":
		logger.Printf("waiting for an external helper")
	default:
		logger.Fatalf("unknown -helper mode %q", *mode)
	}

	// Session loop: one helper at a time.
	sessionsDone := make(chan struct{})
	go func() {
		defer close(sessionsDone)
		for {
			sess, err := lis.Accept(ctx, mgr.OnVideo)
			if err != nil {
				return
			}
			logger.Printf("helper connected")
			err = mgr.RunSession(ctx, sess)
			logger.Printf("helper session ended: %v", err)
			if ctx.Err() != nil {
				mgr.Shutdown(context.Background())
				sess.Close()
				return
			}
			sess.Close()
			if sup != nil {
				sup.Kill() // the supervisor restarts it
			}
		}
	}()

	ws := web.New(mgr, hub, logger)
	ws.CORSOrigin = *corsOrigin
	srv := &http.Server{
		Addr:              *listen,
		Handler:           ws.Handler(),
		ConnContext:       web.ConnContext,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         tlsConfig,
	}
	go func() {
		var err error
		if tlsConfig != nil {
			logger.Printf("listening on %s (HTTPS)", *listen)
			err = srv.ListenAndServeTLS("", "") // certificate is in TLSConfig
		} else {
			logger.Printf("listening on %s (HTTP; WebCodecs needs HTTPS or localhost, see -tls-cert)", *listen)
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatal(err)
		}
	}()

	<-ctx.Done()
	logger.Printf("shutting down")
	hub.Close() // ends /stream responses
	mgr.Frames().Close()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	select {
	case <-sessionsDone:
	case <-sctx.Done():
	}
}

// helperArgs returns the helper command line without the socket arguments.
func helperArgs(cmd, python, runtimeDir string) ([]string, error) {
	if cmd != "" {
		return strings.Fields(cmd), nil
	}
	script := filepath.Join(runtimeDir, "camhelper.py")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(script, camhelper.Script, 0o644); err != nil {
		return nil, err
	}
	return []string{python, script}, nil
}

// capturesDir returns the snapshot directory: flag value or the default.
func capturesDir(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return defaultCapturesDir()
}

// defaultCapturesDir is a RAM-backed directory, so snapshots cause no SD card
// writes. /dev/shm is tmpfs on every Raspberry Pi OS version; /tmp is on the
// SD card on Bookworm (Debian 12), so it is only the fallback.
func defaultCapturesDir() string {
	if fi, err := os.Stat("/dev/shm"); err == nil && fi.IsDir() {
		return "/dev/shm/camweb-captures"
	}
	return filepath.Join(os.TempDir(), "camweb-captures")
}
