package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"
)

// DBConfig describes allowed DB config parameters read from YAML.
type DBConfig struct {
	Host    string `yaml:"host"`
	Port    int    `yaml:"port"`
	DBName  string `yaml:"dbname"`
	SSLMode string `yaml:"sslmode"`
}

// readConfig reads and validates YAML configuration.
func readConfig(path string) (DBConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return DBConfig{}, fmt.Errorf("cannot read config file: %w", err)
	}
	var cfg DBConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return DBConfig{}, fmt.Errorf("cannot parse yaml: %w", err)
	}

	cfg.Host = strings.TrimSpace(cfg.Host)
	cfg.DBName = strings.TrimSpace(cfg.DBName)
	cfg.SSLMode = strings.TrimSpace(cfg.SSLMode)

	if cfg.Host == "" {
		return DBConfig{}, errors.New("host is empty in config")
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return DBConfig{}, errors.New("port out of range in config")
	}
	reDB := regexp.MustCompile(`^[a-zA-Z0-9_]+$`)
	if cfg.DBName == "" || !reDB.MatchString(cfg.DBName) {
		return DBConfig{}, errors.New("dbname has invalid characters in config")
	}
	if cfg.SSLMode == "" {
		cfg.SSLMode = "disable"
	}
	return cfg, nil
}

// Logger duplicates logs to stdout/stderr and, optionally, to a file.
type Logger struct {
	out io.Writer
	err io.Writer
}

func newLogger(logPath string) (*Logger, func(), error) {
	l := &Logger{out: os.Stdout, err: os.Stderr}
	cleanup := func() {}

	if strings.TrimSpace(logPath) == "" {
		return l, cleanup, nil
	}

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, cleanup, fmt.Errorf("cannot open log file: %w", err)
	}
	l.out = io.MultiWriter(os.Stdout, f)
	l.err = io.MultiWriter(os.Stderr, f)
	cleanup = func() { _ = f.Close() }
	return l, cleanup, nil
}

func (l *Logger) Infof(format string, args ...any) {
	fmt.Fprintf(l.out, time.Now().Format(time.RFC3339)+" INFO  "+format+"\n", args...)
}

func (l *Logger) Errorf(format string, args ...any) {
	fmt.Fprintf(l.err, time.Now().Format(time.RFC3339)+" ERROR "+format+"\n", args...)
}

// Env helpers

func getenvRequired(key string) (string, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return "", fmt.Errorf("env %s is required", key)
	}
	return v, nil
}

func getenvInt(key string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("env %s must be int: %w", key, err)
	}
	return n, nil
}

func getenvDurationSeconds(key string, defSec int) (time.Duration, error) {
	sec, err := getenvInt(key, defSec)
	if err != nil {
		return 0, err
	}
	if sec <= 0 {
		return 0, fmt.Errorf("env %s must be > 0", key)
	}
	return time.Duration(sec) * time.Second, nil
}

// Typical PostgreSQL versions considered "normal": 16, 17, 18.
var typicalVersionRe = regexp.MustCompile(`(?i)^PostgreSQL\s+(16|17|18)\.`)

// checkOnce performs a single heartbeat check: connect, run SELECT VERSION().
func checkOnce(cfg DBConfig, user, pass string, timeout time.Duration, l *Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	connConfig, err := pgx.ParseConfig("")
	if err != nil {
		l.Errorf("ParseConfig error: %v", err)
		return
	}

	connConfig.Host = cfg.Host
	connConfig.Port = uint16(cfg.Port)
	connConfig.Database = cfg.DBName
	connConfig.User = user
	connConfig.Password = pass
	// No TLS inside docker network by default.
	connConfig.TLSConfig = nil

	conn, err := pgx.ConnectConfig(ctx, connConfig)
	if err != nil {
		l.Errorf("DB connect failed: %v", err)
		return
	}
	defer conn.Close(context.Background())

	l.Infof("DB connect ok")

	qctx, qcancel := context.WithTimeout(context.Background(), timeout)
	defer qcancel()

	var version string
	if err := conn.QueryRow(qctx, "SELECT VERSION();").Scan(&version); err != nil {
		l.Errorf("Query failed: %v", err)
		return
	}

	v := strings.TrimSpace(version)
	if !typicalVersionRe.MatchString(v) {
		l.Infof("Non-typical VERSION() response: %q", v)
	} else {
		l.Infof("VERSION(): %s", v)
	}
}

func main() {
	// Config path
	configPath := strings.TrimSpace(os.Getenv("DB_CONFIG_PATH"))
	if configPath == "" {
		configPath = "dbconf.yaml"
	}
	cfg, err := readConfig(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Config error:", err)
		os.Exit(1)
	}

	// Credentials from env
	user, err := getenvRequired("DB_USER")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	pass, err := getenvRequired("DB_PASSWORD")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	interval, err := getenvDurationSeconds("PING_INTERVAL_SECONDS", 300)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Interval error:", err)
		os.Exit(1)
	}
	timeout, err := getenvDurationSeconds("PING_TIMEOUT_SECONDS", int(float32(interval.Seconds())*0.9))
	if err != nil {
		fmt.Fprintln(os.Stderr, "Timeout error:", err)
		os.Exit(1)
	}

	logPath := strings.TrimSpace(os.Getenv("LOG_FILE_PATH"))
	logger, cleanup, err := newLogger(logPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Logger error:", err)
		os.Exit(1)
	}
	defer cleanup()

	logger.Infof("Starting pg pinger. interval=%s timeout=%s host=%s port=%d db=%s",
		interval, timeout, cfg.Host, cfg.Port, cfg.DBName)

	// First check immediately
	checkOnce(cfg, user, pass, timeout, logger)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		checkOnce(cfg, user, pass, timeout, logger)
	}
}
