package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	UIProxyToken, SummaryModel, BookOrbitPublicURL              string
	SummaryInterval                                             time.Duration
	Listen, StatePath, AdminToken, BookOrbitURL, BookOrbitToken string
	ModelURL, ModelKey, VisionModel, MetadataModel              string
	LibraryRoot, OriginalsRoot, PostgresDSN                     string
	Providers                                                   []string
	LibraryID, Workers, MaxAttempts                             int
	AutoApply, AutoDiscover, Organize                           bool
	PollInterval, JobTimeout, ModelInterval                     time.Duration
}

func Load() (Config, error) {
	c := Config{Listen: value("LISTEN_ADDR", ":8080"), StatePath: value("STATE_PATH", "/data/librarian.db"), AdminToken: os.Getenv("ADMIN_TOKEN"), BookOrbitURL: os.Getenv("BOOKORBIT_URL"), BookOrbitToken: os.Getenv("BOOKORBIT_MAGIC_LINK_TOKEN"), ModelURL: os.Getenv("MODEL_BASE_URL"), ModelKey: os.Getenv("MODEL_API_KEY"), VisionModel: value("MODEL_VISION", "claude/haiku-5-5"), MetadataModel: value("MODEL_METADATA", "chatgpt/gpt-6.1-terra"), LibraryRoot: os.Getenv("LIBRARY_ROOT"), OriginalsRoot: os.Getenv("ORIGINALS_ROOT"), PostgresDSN: os.Getenv("BOOKORBIT_DATABASE_URL"), Providers: strings.Split(value("METADATA_PROVIDERS", "hardcover,goodreads"), ",")}
	c.UIProxyToken = os.Getenv("UI_PROXY_TOKEN")
	c.SummaryModel = value("MODEL_SUMMARY", c.VisionModel)
	c.BookOrbitPublicURL = os.Getenv("BOOKORBIT_PUBLIC_URL")
	if c.UIProxyToken != "" && len(c.UIProxyToken) < 24 {
		return c, errors.New("UI_PROXY_TOKEN must be at least 24 characters")
	}
	var err error
	c.SummaryInterval, err = time.ParseDuration(value("ACTIVITY_SUMMARY_INTERVAL", "5m"))
	if err != nil || c.SummaryInterval < time.Minute {
		return c, errors.New("ACTIVITY_SUMMARY_INTERVAL must be at least 1m")
	}
	if c.BookOrbitPublicURL != "" {
		u, e := url.Parse(c.BookOrbitPublicURL)
		if e != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
			return c, errors.New("BOOKORBIT_PUBLIC_URL must be an HTTP(S) URL without credentials")
		}
	}
	for _, v := range []struct {
		name     string
		dst      *int
		fallback int
	}{{"LIBRARY_ID", &c.LibraryID, 1}, {"WORKERS", &c.Workers, 1}, {"MAX_ATTEMPTS", &c.MaxAttempts, 3}} {
		*v.dst, err = strconv.Atoi(value(v.name, strconv.Itoa(v.fallback)))
		if err != nil || *v.dst < 1 {
			return c, fmt.Errorf("%s must be a positive integer", v.name)
		}
	}
	if c.Workers > 4 {
		return c, errors.New("WORKERS must be at most 4")
	}
	for _, v := range []struct {
		name     string
		dst      *bool
		fallback string
	}{{"AUTO_APPLY", &c.AutoApply, "false"}, {"AUTO_DISCOVER", &c.AutoDiscover, "false"}, {"ORGANIZE_FILES", &c.Organize, "false"}} {
		*v.dst, err = strconv.ParseBool(value(v.name, v.fallback))
		if err != nil {
			return c, fmt.Errorf("invalid %s", v.name)
		}
	}
	c.PollInterval, err = time.ParseDuration(value("DISCOVERY_INTERVAL", "1h"))
	if err != nil || c.PollInterval < time.Minute {
		return c, errors.New("DISCOVERY_INTERVAL must be at least 1m")
	}
	c.JobTimeout, err = time.ParseDuration(value("JOB_TIMEOUT", "20m"))
	if err != nil || c.JobTimeout < time.Minute || c.JobTimeout > 30*time.Minute {
		return c, errors.New("JOB_TIMEOUT must be between 1m and 30m")
	}
	c.ModelInterval, err = time.ParseDuration(value("MODEL_REQUEST_INTERVAL", "7s"))
	if err != nil || c.ModelInterval < 0 || c.ModelInterval > time.Minute {
		return c, errors.New("MODEL_REQUEST_INTERVAL must be between 0s and 1m")
	}
	for name, v := range map[string]string{"BOOKORBIT_URL": c.BookOrbitURL, "MODEL_BASE_URL": c.ModelURL} {
		u, e := url.Parse(v)
		if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
			return c, fmt.Errorf("%s must be an HTTP(S) URL without credentials", name)
		}
	}
	if len(c.AdminToken) < 24 || c.BookOrbitToken == "" || c.ModelKey == "" {
		return c, errors.New("ADMIN_TOKEN (at least 24 characters), BOOKORBIT_MAGIC_LINK_TOKEN, and MODEL_API_KEY are required")
	}
	if c.LibraryRoot == "" || c.OriginalsRoot == "" {
		return c, errors.New("LIBRARY_ROOT and ORIGINALS_ROOT are required")
	}
	if c.Organize && c.PostgresDSN == "" {
		return c, errors.New("ORGANIZE_FILES requires BOOKORBIT_DATABASE_URL")
	}
	for i, p := range c.Providers {
		c.Providers[i] = strings.TrimSpace(p)
		if c.Providers[i] == "" {
			return c, errors.New("empty METADATA_PROVIDERS entry")
		}
	}
	return c, nil
}

func value(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
