package downloader

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/hashicorp/go-hclog"
	"github.com/xanzy/go-gitlab"
)

// Config holds the configuration for downloading images
type Config struct {
	Logger              hclog.Logger
	GitLabClient        *gitlab.Client
	GitLabSessionCookie string // Session cookie for web-accessible URLs (e.g., _gitlab_session)
	ImageHostingDomain  string
	MaxConcurrency      int
}

// DownloadMRImages downloads all images from merge requests and their comments
// projectSlug: GitLab project slug (e.g., "group/project")
// mergeRequestIDs: Optional list of specific MR IDs to process, nil for all MRs
// outputDir: Directory to save images (will create upload/ subdirectory with same structure)
func DownloadMRImages(ctx context.Context, cfg Config, projectSlug string, mergeRequestIDs *[]int, outputDir string) error {
	// Get project to retrieve project ID
	cfg.Logger.Info("searching for GitLab project", "slug", projectSlug)
	project, _, err := cfg.GitLabClient.Projects.GetProject(projectSlug, nil)
	if err != nil {
		return fmt.Errorf("retrieving project: %v", err)
	}
	if project == nil {
		return fmt.Errorf("no matching GitLab project found: %s", projectSlug)
	}
	projectID := project.ID
	cfg.Logger.Info("found GitLab project", "slug", projectSlug, "project_id", projectID)

	// Fetch all merge requests
	mergeRequests := make(map[int]*gitlab.MergeRequest)
	opts := &gitlab.ListProjectMergeRequestsOptions{
		ListOptions: gitlab.ListOptions{
			PerPage: 100,
		},
		OrderBy: pointer("created_at"),
		Sort:    pointer("asc"),
	}
	if mergeRequestIDs != nil {
		opts.IIDs = mergeRequestIDs
	}

	cfg.Logger.Info("retrieving GitLab merge requests", "project_id", projectID)
	nPages := 0
	for {
		result, resp, err := cfg.GitLabClient.MergeRequests.ListProjectMergeRequests(projectID, opts)
		if err != nil {
			return fmt.Errorf("retrieving gitlab merge requests: %v", err)
		}

		for _, mergeRequest := range result {
			mergeRequests[mergeRequest.IID] = mergeRequest
		}

		nPages++
		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}
	cfg.Logger.Info("retrieved merge requests", "project_id", projectID, "count", len(mergeRequests), "n_pages", nPages)

	// Determine which MRs to process
	var mrIDs []int
	if mergeRequestIDs == nil {
		for iid := range mergeRequests {
			mrIDs = append(mrIDs, iid)
		}
	} else {
		mrIDs = *mergeRequestIDs
	}

	totalCount := len(mrIDs)

	// Ensure output directory exists
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("creating output directory: %v", err)
	}

	cfg.Logger.Info("downloading images from merge requests", "project_id", projectID, "count", totalCount, "max_concurrency", cfg.MaxConcurrency, "output_dir", outputDir)

	type migrationResult struct {
		ok  bool
		err error
	}

	resultChan := make(chan migrationResult, totalCount)

	go func() {
		// Launch all jobs with semaphore limiting concurrency
		semaphore := make(chan struct{}, cfg.MaxConcurrency)
		wg := sync.WaitGroup{}
		for _, mergeRequestID := range mrIDs {
			wg.Add(1)
			go func(mergeRequestID int) {
				defer wg.Done()
				// Acquire semaphore
				semaphore <- struct{}{}
				defer func() { <-semaphore }() // Release semaphore

				mergeRequest := mergeRequests[mergeRequestID]
				if mergeRequest == nil {
					// No MR found, skip
					resultChan <- migrationResult{ok: true, err: nil}
					return
				}

				cfg.Logger.Info("downloading images from MR", "merge_request_id", mergeRequestID, "project_id", projectID)

				// Download images from MR body
				if err := downloadImagesFromText(ctx, cfg, projectID, mergeRequest.Description, mergeRequestID, 0, outputDir); err != nil {
					resultChan <- migrationResult{ok: false, err: fmt.Errorf("downloading images from MR %d body: %v", mergeRequestID, err)}
					return
				}

				// Fetch and download images from MR comments
				if err := downloadMRCommentImages(ctx, cfg, projectID, mergeRequestID, outputDir); err != nil {
					resultChan <- migrationResult{ok: false, err: fmt.Errorf("downloading images from MR %d comments: %v", mergeRequestID, err)}
					return
				}

				resultChan <- migrationResult{ok: true, err: nil}
			}(mergeRequestID)
		}

		// Wait for all jobs to complete and close result channel
		wg.Wait()
		close(resultChan)
	}()

	// Aggregate results
	var successCount, failureCount int
	for result := range resultChan {
		if result.err != nil {
			cfg.Logger.Error("error downloading images", "error", result.err)
			failureCount++
		} else if result.ok {
			successCount++
		}
	}

	cfg.Logger.Info("completed image download", "project_id", projectID, "success", successCount, "failures", failureCount)
	return nil
}

func downloadMRCommentImages(ctx context.Context, cfg Config, projectID, mergeRequestID int, outputDir string) error {
	// Get all gitlab merge request comments
	var comments []*gitlab.Note
	commentsOpts := &gitlab.ListMergeRequestNotesOptions{
		ListOptions: gitlab.ListOptions{
			PerPage: 100,
		},
		OrderBy: pointer("created_at"),
		Sort:    pointer("asc"),
	}
	nPages := 0
	for {
		result, resp, err := cfg.GitLabClient.Notes.ListMergeRequestNotes(projectID, mergeRequestID, commentsOpts)
		if err != nil {
			return fmt.Errorf("listing merge request notes: %v", err)
		}

		comments = append(comments, result...)

		nPages++
		if resp.NextPage == 0 {
			break
		}

		commentsOpts.Page = resp.NextPage
	}

	// Filter and download images from comments (excluding system comments)
	for _, comment := range comments {
		if comment == nil || comment.System {
			continue
		}

		if strings.HasPrefix(comment.Author.Username, "GitLab-") || strings.HasPrefix(comment.Author.Username, "gitlab-") {
			cfg.Logger.Debug("skipping system comment", "merge_request_id", mergeRequestID, "gl_comment_id", comment.ID)
			continue
		}

		if err := downloadImagesFromText(ctx, cfg, projectID, comment.Body, mergeRequestID, comment.ID, outputDir); err != nil {
			return fmt.Errorf("downloading images from comment %d: %v", comment.ID, err)
		}
	}

	return nil
}

func downloadImagesFromText(ctx context.Context, cfg Config, projectID int, textBody string, mergeRequestID, commentID int, outputDir string) error {
	if textBody == "" {
		return nil
	}

	// Pattern: ![alt text](/uploads/path/to/file.png)
	imageLinkRegex := regexp.MustCompile(`!\[([^\]]*)\]\((/uploads/[^)]+)\)`)
	matches := imageLinkRegex.FindAllStringSubmatch(textBody, -1)

	for _, match := range matches {
		if len(match) != 3 {
			continue
		}
		uploadPath := match[2] // /uploads/path/to/file.png

		// Construct full URL
		imageURL := fmt.Sprintf("https://%s/-/project/%d%s", cfg.ImageHostingDomain, projectID, uploadPath)

		// Download the image
		if err := downloadImage(ctx, cfg, imageURL, uploadPath, outputDir); err != nil {
			return fmt.Errorf("downloading image %s: %v", imageURL, err)
		}

		cfg.Logger.Debug("downloaded image", "merge_request_id", mergeRequestID, "comment_id", commentID, "path", uploadPath, "url", imageURL)
	}

	// Pattern: <img src="/uploads/path/to/file.png" .../>
	htmlImageRegex := regexp.MustCompile(`<img\s+[^>]*src=["'](/uploads/[^"']+)["'][^>]*/?>`)
	htmlMatches := htmlImageRegex.FindAllStringSubmatch(textBody, -1)

	for _, match := range htmlMatches {
		if len(match) != 2 {
			continue
		}
		uploadPath := match[1] // /uploads/path/to/file.png

		// Construct full URL
		imageURL := fmt.Sprintf("https://%s/-/project/%d%s", cfg.ImageHostingDomain, projectID, uploadPath)

		// Download the image
		if err := downloadImage(ctx, cfg, imageURL, uploadPath, outputDir); err != nil {
			return fmt.Errorf("downloading image %s: %v", imageURL, err)
		}

		cfg.Logger.Debug("downloaded image", "merge_request_id", mergeRequestID, "comment_id", commentID, "path", uploadPath, "url", imageURL)
	}

	return nil
}

func downloadImage(ctx context.Context, cfg Config, imageURL, uploadPath, outputDir string) error {
	// Create the full output path maintaining the same directory structure
	// uploadPath is like /uploads/path/to/file.png
	// We want to save to outputDir/uploads/path/to/file.png
	outputPath := filepath.Join(outputDir, strings.TrimPrefix(uploadPath, "/"))
	outputDirPath := filepath.Dir(outputPath)

	// Create directory structure
	if err := os.MkdirAll(outputDirPath, 0755); err != nil {
		return fmt.Errorf("creating directory %s: %v", outputDirPath, err)
	}

	// Check if file already exists
	if _, err := os.Stat(outputPath); err == nil {
		cfg.Logger.Debug("image already exists, skipping", "path", outputPath)
		return nil
	}

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "GET", imageURL, nil)
	if err != nil {
		return fmt.Errorf("creating request: %v", err)
	}

	// Add session cookie if available (required for web-accessible upload URLs)
	// Can be either just the session ID or the full cookie string from browser
	if cfg.GitLabSessionCookie != "" {
		cookieValue := strings.TrimSpace(cfg.GitLabSessionCookie)
		// If it doesn't contain "=", assume it's just the session ID
		if !strings.Contains(cookieValue, "=") {
			cookieValue = fmt.Sprintf("_gitlab_session=%s", cookieValue)
		}
		// Remove any newlines or invalid characters
		cookieValue = strings.ReplaceAll(cookieValue, "\n", "")
		cookieValue = strings.ReplaceAll(cookieValue, "\r", "")
		req.Header.Set("Cookie", cookieValue)
	}

	// Set headers to mimic browser (some servers require this)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	req.Header.Set("Cache-Control", "max-age=0")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")

	// Create HTTP client that follows redirects
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Preserve the session cookie through redirects
			if cfg.GitLabSessionCookie != "" {
				cookieValue := strings.TrimSpace(cfg.GitLabSessionCookie)
				// If it doesn't contain "=", assume it's just the session ID
				if !strings.Contains(cookieValue, "=") {
					cookieValue = fmt.Sprintf("_gitlab_session=%s", cookieValue)
				}
				// Remove any newlines or invalid characters
				cookieValue = strings.ReplaceAll(cookieValue, "\n", "")
				cookieValue = strings.ReplaceAll(cookieValue, "\r", "")
				req.Header.Set("Cookie", cookieValue)
			}
			return nil
		},
	}

	// Execute request
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("downloading image: %v", err)
	}
	defer resp.Body.Close()

	// Check for redirect to login page (permission issue)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("authentication failed: status %d (check GITLAB_TOKEN permissions)", resp.StatusCode)
	}

	// Check if we got HTML instead of an image (likely login page)
	contentType := resp.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "text/html") {
		return fmt.Errorf("received HTML instead of image (likely login page): check authentication")
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// Peek at first bytes to detect HTML even if Content-Type is wrong
	peekBytes := make([]byte, 512)
	n, err := resp.Body.Read(peekBytes)
	if err != nil && err != io.EOF {
		return fmt.Errorf("reading response: %v", err)
	}

	// Check if response starts with HTML tags
	peekStr := strings.ToLower(string(peekBytes[:n]))
	if strings.HasPrefix(peekStr, "<!doctype html") || strings.HasPrefix(peekStr, "<html") {
		return fmt.Errorf("received HTML login page instead of image: authentication required (check GITLAB_TOKEN)")
	}

	// Create output file
	outFile, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("creating output file: %v", err)
	}
	defer outFile.Close()

	// Write the peeked bytes first
	if n > 0 {
		if _, err := outFile.Write(peekBytes[:n]); err != nil {
			return fmt.Errorf("writing file: %v", err)
		}
	}

	// Copy remaining response body to file
	if _, err := io.Copy(outFile, resp.Body); err != nil {
		return fmt.Errorf("writing file: %v", err)
	}

	cfg.Logger.Debug("downloaded image", "url", imageURL, "path", outputPath)
	return nil
}

func pointer[T any](v T) *T {
	return &v
}
