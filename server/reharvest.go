package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"time"
)

const (
	obscuraBin    = "/app/obscura"
	nodeBin       = "/usr/local/bin/node"
	harvestScript = "/app/harvest/harvest.mjs"
	obscuraPort   = "9222"
	obscuraCDP    = "ws://127.0.0.1:" + obscuraPort
	obscuraProbe  = "http://127.0.0.1:" + obscuraPort + "/json/version"

	reharvestMinInterval = 5 * time.Minute
	harvestTimeout       = 90 * time.Second
)

var (
	reharvestRunning atomic.Bool
	reharvestLastTry atomic.Int64
)

func triggerReharvest() {
	if _, err := os.Stat(obscuraBin); err != nil {
		return
	}
	now := time.Now().UnixMilli()
	if last := reharvestLastTry.Load(); last != 0 && now-last < reharvestMinInterval.Milliseconds() {
		return
	}
	if !reharvestRunning.CompareAndSwap(false, true) {
		return
	}
	reharvestLastTry.Store(now)
	go func() {
		defer reharvestRunning.Store(false)
		if err := runReharvest(); err != nil {
			slog.Warn("external-helper key re-harvest failed", "error", err)
		}
	}()
}

func runReharvest() error {
	ctx, cancel := context.WithTimeout(context.Background(), harvestTimeout)
	defer cancel()

	ob := exec.CommandContext(ctx, obscuraBin, "serve", "--port", obscuraPort, "--host", "127.0.0.1")
	ob.Env = []string{"HOME=/tmp"} // the harvester runs third-party JS; never hand it app secrets
	if err := ob.Start(); err != nil {
		return err
	}
	defer func() {
		_ = ob.Process.Kill()
		_ = ob.Wait()
	}()
	if err := waitObscura(ctx); err != nil {
		return err
	}

	hv := exec.CommandContext(ctx, nodeBin, harvestScript, obscuraCDP, externalHelperSite)
	hv.Env = ob.Env
	out, err := hv.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("harvester failed: %w: %s", err, bytes.TrimSpace(exitErr.Stderr))
		}
		return fmt.Errorf("harvester failed: %w", err)
	}

	var res struct {
		Key     string `json:"key"`
		FixedTs int64  `json:"fixedTs"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return err
	}

	if res.FixedTs == 0 || !seedSignCreds(res.Key, strconv.FormatInt(res.FixedTs, 10)) {
		return errors.New("harvested key malformed")
	}
	slog.Info("external-helper key re-harvested", "fixed_ts", res.FixedTs)
	return nil
}

func waitObscura(ctx context.Context) error {
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, obscuraProbe, nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
