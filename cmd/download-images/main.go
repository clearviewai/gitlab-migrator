package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"gitlab-migrator/internal/downloader"

	"github.com/hashicorp/go-hclog"
	"github.com/xanzy/go-gitlab"
)

const (
	defaultGitlabDomain = "gitlab.com"
)

var (
	logger         hclog.Logger
	gl             *gitlab.Client
	gitlabDomain   string
	gitlabToken    string
	maxConcurrency int
)

func main() {
	var err error
	var projectSlug string
	var outputDir string
	var mrIDsRaw string
	var showVersion bool

	flag.StringVar(&projectSlug, "gitlab-project", "", "GitLab project slug (e.g., 'group/project') (required)")
	flag.StringVar(&outputDir, "output", "./downloads", "Output directory for downloaded images")
	flag.StringVar(&mrIDsRaw, "mr-ids", "", "Comma-separated list of specific MR IDs to process (optional, processes all MRs if not specified)")
	flag.StringVar(&gitlabDomain, "gitlab-domain", defaultGitlabDomain, "GitLab domain to use")
	flag.IntVar(&maxConcurrency, "max-concurrency", 50, "Maximum number of concurrent downloads")
	flag.BoolVar(&showVersion, "version", false, "Show version information")
	flag.Parse()

	if showVersion {
		fmt.Println("download-images version development")
		return
	}

	// Initialize logger
	logger = hclog.New(&hclog.LoggerOptions{
		Name:  "download-images",
		Level: hclog.LevelFromString(os.Getenv("LOG_LEVEL")),
	})

	// Validate required flags
	if projectSlug == "" {
		logger.Error("missing required flag: -project")
		flag.Usage()
		os.Exit(1)
	}

	// Get GitLab token
	gitlabToken = os.Getenv("GITLAB_TOKEN")
	if gitlabToken == "" {
		logger.Error("missing environment variable", "name", "GITLAB_TOKEN")
		os.Exit(1)
	}

	// Get GitLab session cookie (optional, for web-accessible upload URLs)
	gitlabSessionCookie := os.Getenv("GITLAB_SESSION_COOKIE")
	if gitlabSessionCookie == "" {
		logger.Error("using GitLab session cookie for web authentication is not supported yet")
		os.Exit(1)
	}

	// Initialize GitLab client
	gitlabOpts := make([]gitlab.ClientOptionFunc, 0)
	if gitlabDomain != defaultGitlabDomain {
		gitlabUrl := fmt.Sprintf("https://%s", gitlabDomain)
		gitlabOpts = append(gitlabOpts, gitlab.WithBaseURL(gitlabUrl))
	}
	if gl, err = gitlab.NewClient(gitlabToken, gitlabOpts...); err != nil {
		logger.Error("failed to create GitLab client", "error", err)
		os.Exit(1)
	}

	// Parse MR IDs if provided
	var mergeRequestIDs *[]int
	if mrIDsRaw != "" {
		parts := strings.Split(mrIDsRaw, ",")
		ids := make([]int, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			id, err := strconv.Atoi(part)
			if err != nil {
				logger.Error("invalid MR ID", "id", part, "error", err)
				os.Exit(1)
			}
			ids = append(ids, id)
		}
		if len(ids) > 0 {
			mergeRequestIDs = &ids
		}
	}

	// Create context
	ctx := context.Background()

	logger.Info("starting image download", "project", projectSlug, "output_dir", outputDir)

	// Create downloader config
	cfg := downloader.Config{
		Logger:              logger,
		GitLabClient:        gl,
		GitLabSessionCookie: gitlabSessionCookie,
		ImageHostingDomain:  gitlabDomain,
		MaxConcurrency:      maxConcurrency,
	}

	if err := downloader.DownloadMRImages(ctx, cfg, projectSlug, mergeRequestIDs, outputDir); err != nil {
		logger.Error("failed to download images", "error", err)
		os.Exit(1)
	}

	logger.Info("image download completed successfully")
}
