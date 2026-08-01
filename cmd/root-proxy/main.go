// Command root-proxy runs the lightweight ChatGPT Desktop routing boundary.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/joho/godotenv"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/rootproxy"
	log "github.com/sirupsen/logrus"
)

func init() {
	log.SetOutput(os.Stdout)
	log.SetFormatter(&log.JSONFormatter{})
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.WithError(err).Error("root proxy exited")
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("root-proxy", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var configPath string
	flags.StringVar(&configPath, "config", "root-proxy.yaml", "Root Proxy configuration file")
	if errParse := flags.Parse(args); errParse != nil {
		return errParse
	}
	if len(flags.Args()) != 0 {
		return fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}

	workingDirectory, errWorkingDirectory := os.Getwd()
	if errWorkingDirectory != nil {
		return fmt.Errorf("get working directory: %w", errWorkingDirectory)
	}
	envPath := filepath.Join(workingDirectory, ".env")
	if errEnv := loadPrivateDotEnv(envPath); errEnv != nil {
		return errEnv
	}
	if !filepath.IsAbs(configPath) {
		configPath = filepath.Join(workingDirectory, configPath)
	}
	config, errConfig := rootproxy.LoadConfig(configPath)
	if errConfig != nil {
		return errConfig
	}
	if config.Debug {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}
	server, errServer := rootproxy.NewServer(config)
	if errServer != nil {
		return errServer
	}
	return server.Run(ctx)
}

func loadPrivateDotEnv(path string) error {
	info, errStat := os.Stat(path)
	if errStat != nil {
		if errors.Is(errStat, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat .env: %w", errStat)
	}
	if !info.Mode().IsRegular() {
		return errors.New(".env must be a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf(".env permissions %04o are too open; require 0600", info.Mode().Perm())
	}
	if errLoad := godotenv.Load(path); errLoad != nil {
		return fmt.Errorf("load .env: %w", errLoad)
	}
	return nil
}
