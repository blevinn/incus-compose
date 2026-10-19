package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/lxc/incus-compose/internal/nix2containershim"
)

type customBuilderManifest struct {
	Rootfs string `json:"rootfs"`
	Config string `json:"config"`
}

func runContainerNix(ctx context.Context, c *Client, cfg *BuildConfig, imageName, arch string, stdout, stderr io.Writer) (*buildArtifacts, error) {
	if len(cfg.CustomBuilderArgs) < 1 {
		return nil, errors.New("nix2container builder requires at least one target argument")
	}
	target := cfg.CustomBuilderArgs[0]

	c.LogDebug("Executing in-process nix2container builder", "target", target, "arch", arch)

	artifacts, err := nix2containershim.Build(ctx, cfg.Context, target, arch, stdout, stderr)
	if err != nil {
		return nil, fmt.Errorf("building nix2container target %s: %w", target, err)
	}

	config, configJSON, err := parseImageConfig(artifacts.Config, artifacts.RootfsPath)
	if err != nil {
		_ = artifacts.Rootfs.Close()
		return nil, err
	}

	return &buildArtifacts{
		Rootfs:     artifacts.Rootfs,
		ConfigJSON: configJSON,
		Config:     config,
	}, nil
}

// tarBuilder returns a builderFunc that invokes an external custom builder binary.
func tarBuilder(builder string) builderFunc {
	return func(ctx context.Context, c *Client, cfg *BuildConfig, imageName, arch string, stdout, stderr io.Writer) (*buildArtifacts, error) {
		platform := cfg.Platform
		if platform == "" {
			platform = "linux/" + archPlatform(arch)
		}
		targetArch, _, _ := platformParts(arch)
		if targetArch == "" {
			targetArch = arch
		}

		cmd := exec.CommandContext(ctx, builder, cfg.CustomBuilderArgs...) //nolint:gosec
		cmd.Stderr = stderr
		var outBuf bytes.Buffer
		if stdout != nil {
			cmd.Stdout = io.MultiWriter(&outBuf, stdout)
		} else {
			cmd.Stdout = &outBuf
		}

		env := os.Environ()
		env = putEnv(env, "TARGETPLATFORM="+platform, true)
		env = putEnv(env, "TARGETOS=linux", true)
		env = putEnv(env, "TARGETARCH="+targetArch, true)
		env = putEnv(env, "IMAGE_NAME="+imageName, true)
		env = putEnv(env, "INCUS_PROJECT="+c.incusProject, true)
		for k, v := range cfg.Args {
			env = putEnv(env, k+"="+v, true)
		}
		cmd.Env = env
		if cfg.Context != "" {
			cmd.Dir = cfg.Context
		}

		c.LogDebug("Executing external builder", "command", builder, "args", cfg.CustomBuilderArgs)

		err := cmd.Run()
		if err != nil {
			return nil, fmt.Errorf("executing external builder %s: %w", builder, err)
		}

		rawOut := strings.TrimSpace(outBuf.String())
		idx := strings.Index(rawOut, "{")
		if idx >= 0 {
			rawOut = rawOut[idx:]
		}

		var manifest customBuilderManifest
		err = json.Unmarshal([]byte(rawOut), &manifest)
		if err != nil {
			return nil, fmt.Errorf("parsing external builder output JSON (%q): %w", rawOut, err)
		}

		if manifest.Rootfs == "" || manifest.Config == "" {
			return nil, fmt.Errorf("external builder output missing rootfs or config path: %s", rawOut)
		}

		configBytes, err := os.ReadFile(manifest.Config)
		if err != nil {
			_ = os.Remove(manifest.Rootfs)
			return nil, fmt.Errorf("reading external builder config %s: %w", manifest.Config, err)
		}
		_ = os.Remove(manifest.Config)

		var imgCfg map[string]any
		err = json.Unmarshal(configBytes, &imgCfg)
		if err != nil {
			_ = os.Remove(manifest.Rootfs)
			return nil, fmt.Errorf("parsing external builder config JSON: %w", err)
		}

		config, configJSON, err := parseImageConfig(imgCfg, manifest.Rootfs)
		if err != nil {
			_ = os.Remove(manifest.Rootfs)
			return nil, err
		}

		f, err := os.Open(manifest.Rootfs)
		if err != nil {
			_ = os.Remove(manifest.Rootfs)
			return nil, fmt.Errorf("opening external builder rootfs: %w", err)
		}

		var dirToClean string
		parent := filepath.Dir(manifest.Rootfs)
		if parent != "" && parent != os.TempDir() && filepath.Dir(parent) == os.TempDir() {
			dirToClean = parent
		}

		return &buildArtifacts{
			Rootfs:     &tempFile{File: f, path: manifest.Rootfs, dir: dirToClean},
			ConfigJSON: configJSON,
			Config:     config,
		}, nil
	}
}
