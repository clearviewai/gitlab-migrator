package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/google/go-github/v74/github"
	"github.com/xanzy/go-gitlab"
)

func newProject(slugs []string) (*project, error) {
	var err error
	p := &project{}

	p.gitlabPath, p.githubPath, err = parseProjectSlugs(slugs)
	if err != nil {
		return nil, fmt.Errorf("parsing project slugs: %v", err)
	}

	logger.Info("searching for GitLab project", "name", p.gitlabPath[1], "group", p.gitlabPath[0])
	p.project, _, err = gl.Projects.GetProject(slugs[0], nil)
	if err != nil {
		return nil, fmt.Errorf("retrieving project: %v", err)
	}

	if p.project == nil {
		return nil, fmt.Errorf("no matching GitLab project found: %s", slugs[0])
	}

	p.defaultBranch = "main"
	if renameTrunkBranch != "" {
		p.defaultBranch = renameTrunkBranch
	} else if !renameMasterToMain && p.project.DefaultBranch != "" {
		p.defaultBranch = p.project.DefaultBranch
	}

	return p, nil
}

type project struct {
	project       *gitlab.Project
	repo          *git.Repository
	defaultBranch string
	gitlabPath    []string
	githubPath    []string
}

func (p *project) createRepo(ctx context.Context, homepage string, repoDeleted bool) error {
	if repoDeleted {
		logger.Warn("recreating GitHub repository", "owner", p.githubPath[0], "repo", p.githubPath[1])
	} else {
		logger.Debug("repository not found on GitHub, proceeding to create", "owner", p.githubPath[0], "repo", p.githubPath[1])
	}
	newRepo := github.Repository{
		Name:          pointer(p.githubPath[1]),
		Description:   &p.project.Description,
		Homepage:      &homepage,
		DefaultBranch: &p.defaultBranch,
		Private:       pointer(true),
		HasIssues:     pointer(true),
		HasProjects:   pointer(true),
		HasWiki:       pointer(true),
	}
	if _, _, err := gh.Repositories.Create(ctx, p.githubPath[0], &newRepo); err != nil {
		return fmt.Errorf("creating github repo: %v", err)
	}

	return nil
}

func (p *project) migrate(ctx context.Context) error {
	if onlyMigrateComments {
		logger.Info("only migrating comments from GitLab to GitHub and skipping repository migration", "name", p.gitlabPath[1], "group", p.gitlabPath[0])
		p.migrateMergeRequests(ctx, nil)
		return nil
	}

	cloneUrl, err := url.Parse(p.project.HTTPURLToRepo)
	if err != nil {
		return fmt.Errorf("parsing clone URL: %v", err)
	}

	logger.Info("mirroring repository from GitLab to GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "github_org", p.githubPath[0], "github_repo", p.githubPath[1])

	logger.Debug("checking for existing repository on GitHub", "owner", p.githubPath[0], "repo", p.githubPath[1])
	_, _, err = gh.Repositories.Get(ctx, p.githubPath[0], p.githubPath[1])

	var githubError *github.ErrorResponse
	if err != nil && (!errors.As(err, &githubError) || githubError == nil || githubError.Response == nil || githubError.Response.StatusCode != http.StatusNotFound) {
		return fmt.Errorf("retrieving github repo: %v", err)
	}

	homepage := fmt.Sprintf("https://%s/%s/%s", gitlabDomain, p.gitlabPath[0], p.gitlabPath[1])

	if err != nil {
		// Repository not found
		if err = p.createRepo(ctx, homepage, false); err != nil {
			return err
		}
	} else if deleteExistingRepos {
		logger.Warn("existing repository was found on GitHub, proceeding to delete", "owner", p.githubPath[0], "repo", p.githubPath[1])
		if _, err = gh.Repositories.Delete(ctx, p.githubPath[0], p.githubPath[1]); err != nil {
			return fmt.Errorf("deleting existing github repo: %v", err)
		}

		if err = p.createRepo(ctx, homepage, true); err != nil {
			return err
		}
	}

	logger.Debug("updating repository settings", "owner", p.githubPath[0], "repo", p.githubPath[1])
	description := regexp.MustCompile("\r|\n").ReplaceAllString(p.project.Description, " ")
	updateRepo := github.Repository{
		Name:              pointer(p.githubPath[1]),
		Description:       &description,
		Homepage:          &homepage,
		AllowAutoMerge:    pointer(true),
		AllowMergeCommit:  pointer(true),
		AllowRebaseMerge:  pointer(true),
		AllowSquashMerge:  pointer(true),
		AllowUpdateBranch: pointer(true),
	}
	if _, _, err = gh.Repositories.Edit(ctx, p.githubPath[0], p.githubPath[1], &updateRepo); err != nil {
		return fmt.Errorf("updating github repo: %v", err)
	}

	cloneUrl.User = url.UserPassword("oauth2", gitlabToken)
	cloneUrlWithCredentials := cloneUrl.String()

	// In-memory filesystem for worktree operations
	fs := memfs.New()

	logger.Debug("cloning repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", p.project.HTTPURLToRepo)
	p.repo, err = git.CloneContext(ctx, memory.NewStorage(), fs, &git.CloneOptions{
		URL:        cloneUrlWithCredentials,
		Auth:       nil,
		RemoteName: "gitlab",
		Mirror:     true,
	})
	if err != nil {
		return fmt.Errorf("cloning gitlab repo: %v", err)
	}

	if p.defaultBranch != p.project.DefaultBranch {
		if gitlabTrunk, err := p.repo.Reference(plumbing.NewBranchReferenceName(p.project.DefaultBranch), false); err == nil {
			logger.Info("renaming trunk branch prior to push", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "gitlab_trunk", p.project.DefaultBranch, "github_trunk", p.defaultBranch, "sha", gitlabTrunk.Hash())

			logger.Debug("creating new trunk branch", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "github_trunk", p.defaultBranch, "sha", gitlabTrunk.Hash())
			githubTrunk := plumbing.NewHashReference(plumbing.NewBranchReferenceName(p.defaultBranch), gitlabTrunk.Hash())
			if err = p.repo.Storer.SetReference(githubTrunk); err != nil {
				return fmt.Errorf("creating trunk branch: %v", err)
			}

			logger.Debug("deleting old trunk branch", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "gitlab_trunk", p.project.DefaultBranch, "sha", gitlabTrunk.Hash())
			if err = p.repo.Storer.RemoveReference(gitlabTrunk.Name()); err != nil {
				return fmt.Errorf("deleting old trunk branch: %v", err)
			}
		}
	}

	githubUrl := fmt.Sprintf("https://%s/%s/%s", githubDomain, p.githubPath[0], p.githubPath[1])
	githubUrlWithCredentials := fmt.Sprintf("https://%s:%s@%s/%s/%s", githubUser, githubToken, githubDomain, p.githubPath[0], p.githubPath[1])

	logger.Debug("adding remote for GitHub repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
	if _, err = p.repo.CreateRemote(&config.RemoteConfig{
		Name:   "github",
		URLs:   []string{githubUrlWithCredentials},
		Mirror: true,
	}); err != nil {
		return fmt.Errorf("adding github remote: %v", err)
	}

	logger.Debug("determining branches to push", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
	branches, err := p.repo.Branches()
	if err != nil {
		return fmt.Errorf("retrieving branches: %v", err)
	}

	gitlabBranches := make(map[string]bool, 0)
	refSpecs := make([]config.RefSpec, 0)
	if err = branches.ForEach(func(ref *plumbing.Reference) error {
		gitlabBranches[ref.Name().Short()] = true
		refSpecs = append(refSpecs, config.RefSpec(fmt.Sprintf("%[1]s:%[1]s", ref.Name())))
		return nil
	}); err != nil {
		return fmt.Errorf("parsing branches: %v", err)
	}

	logger.Debug("force-pushing branches to GitHub repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl, "count", len(refSpecs))
	if err = p.repo.PushContext(ctx, &git.PushOptions{
		RemoteName: "github",
		Force:      true,
		RefSpecs:   refSpecs,
		//Prune:      true, // causes error, attempts to delete main branch
	}); err != nil {
		if errors.Is(err, git.NoErrAlreadyUpToDate) {
			logger.Debug("repository already up-to-date on GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
		} else {
			return fmt.Errorf("pushing to github repo: %v", err)
		}
	}

	if trimGithubBranches {
		logger.Debug("determining old branches to trim on GitHub repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
		refSpecsToDelete := make([]config.RefSpec, 0)
		githubBranches, err := getGithubBranches(ctx, p.githubPath[0], p.githubPath[1])
		if err != nil {
			return fmt.Errorf("listing branches from GitHub: %v", err)
		}
		for _, githubBranch := range githubBranches {
			if githubBranch.Name != nil && !gitlabBranches[*githubBranch.Name] {
				refSpecsToDelete = append(refSpecsToDelete, config.RefSpec(fmt.Sprintf(":refs/heads/%s", *githubBranch.Name)))
			}
		}

		logger.Debug("trimming old branches on GitHub repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl, "count", len(refSpecsToDelete))
		if err = p.repo.PushContext(ctx, &git.PushOptions{
			RemoteName: "github",
			Force:      true,
			RefSpecs:   refSpecsToDelete,
			//Prune:      true, // causes error, attempts to delete main branch
		}); err != nil {
			if errors.Is(err, git.NoErrAlreadyUpToDate) {
				logger.Debug("repository already up-to-date on GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
			} else {
				return fmt.Errorf("pushing to github repo: %v", err)
			}
		}
	}

	logger.Debug("force-pushing tags to GitHub repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
	if err = p.repo.PushContext(ctx, &git.PushOptions{
		RemoteName: "github",
		Force:      true,
		RefSpecs:   []config.RefSpec{"refs/tags/*:refs/tags/*"},
	}); err != nil {
		if errors.Is(err, git.NoErrAlreadyUpToDate) {
			logger.Debug("repository already up-to-date on GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
		} else {
			return fmt.Errorf("pushing to github repo: %v", err)
		}
	}

	logger.Debug("setting default repository branch", "owner", p.githubPath[0], "repo", p.githubPath[1], "branch_name", p.defaultBranch)
	updateRepo = github.Repository{
		DefaultBranch: &p.defaultBranch,
	}
	if _, _, err = gh.Repositories.Edit(ctx, p.githubPath[0], p.githubPath[1], &updateRepo); err != nil {
		return fmt.Errorf("setting default branch: %v", err)
	}

	if enablePullRequests {
		p.migrateMergeRequests(ctx, nil)
	}

	return nil
}

func (p *project) migrateMergeRequests(ctx context.Context, mergeRequestIDs *[]int) {
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

	if mergeRequestsAge > 0 {
		opts.CreatedAfter = pointer(time.Now().AddDate(0, 0, -mergeRequestsAge))
	}

	maxMergeRequestNumber := 0
	logger.Info("retrieving GitLab merge requests", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID)
	nPages := 0
	for {
		result, resp, err := gl.MergeRequests.ListProjectMergeRequests(p.project.ID, opts)
		if err != nil {
			sendErr(fmt.Errorf("retrieving gitlab merge requests: %v", err))
			return
		}

		for _, mergeRequest := range result {
			mergeRequests[mergeRequest.IID] = mergeRequest
			if mergeRequest.IID > maxMergeRequestNumber {
				maxMergeRequestNumber = mergeRequest.IID
			}
		}

		nPages++
		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}
	logger.Info("retrieved merge requests", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "count", len(mergeRequests), "n_pages", nPages)

	if mergeRequestIDs == nil {
		ids := make([]int, maxMergeRequestNumber)
		for i := range ids {
			ids[i] = i + 1
		}
		mergeRequestIDs = &ids
		logger.Info(fmt.Sprintf("we will fill in the gaps with empty pull requests from 1 to %d", maxMergeRequestNumber))
	} else {
		logger.Info("we will fill in missing merge requests with empty pull requests")
	}

	var successCount, failureCount int
	totalCount := len(*mergeRequestIDs)

	if onlyMigrateComments {
		logger.Info("migrating merge request comments from GitLab to GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "count", totalCount, "max_concurrency", maxConcurrencyForComments)

		type migrationResult struct {
			ok  bool
			err error
		}

		resultChan := make(chan migrationResult, totalCount)

		go func() {
			// Launch all jobs with semaphore limiting concurrency
			semaphore := make(chan struct{}, maxConcurrencyForComments)
			wg := sync.WaitGroup{}
			for _, mergeRequestID := range *mergeRequestIDs {
				wg.Add(1)
				go func(mergeRequestID int) {
					defer wg.Done()
					// Acquire semaphore
					semaphore <- struct{}{}
					defer func() { <-semaphore }() // Release semaphore

					mergeRequest := mergeRequests[mergeRequestID]
					if mergeRequest == nil {
						// this is fine - the empty PR doesn't need comments
						resultChan <- migrationResult{ok: true, err: nil}
						return
					}
					pullRequest, err := p.fetchPullRequest(ctx, mergeRequest)
					if err != nil {
						resultChan <- migrationResult{ok: false, err: err}
						return
					}
					ok, err := p.migrateMergeRequestComments(ctx, mergeRequest, pullRequest)
					resultChan <- migrationResult{ok: ok, err: err}
				}(mergeRequestID)
			}

			// Wait for all jobs to complete and close result channel
			wg.Wait()
			close(resultChan)
		}()

		// Aggregate results
		for result := range resultChan {
			if result.err != nil {
				sendErr(result.err)
				failureCount++
			} else if result.ok {
				successCount++
			}
		}

	} else {
		logger.Info("migrating merge requests from GitLab to GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "count", totalCount)
		for _, mergeRequestID := range *mergeRequestIDs {
			mergeRequest := mergeRequests[mergeRequestID]
			var ok bool
			var err error
			if mergeRequest == nil {
				// this happened to one MR that does not exist on GitLab
				ok, err = p.createEmptyPullRequest(ctx, mergeRequestID)
			} else {
				ok, err = func() (bool, error) {
					pullRequest, err := p.fetchPullRequest(ctx, mergeRequest)
					if err != nil {
						return false, err
					}
					ok, err = p.migrateMergeRequestWithoutComments(ctx, mergeRequest, pullRequest)
					if err != nil {
						return false, err
					}
					if !skipMigratingComments {
						ok, err = p.migrateMergeRequestComments(ctx, mergeRequest, pullRequest)
						if err != nil {
							return false, err
						}
					}
					return ok, nil
				}()
			}
			if err != nil {
				sendErr(err)
				failureCount++
				if !skipInvalidMergeRequests {
					// explicitly terminate to avoid MR <> PR number mismatch
					// user should manually fix the mismatch and re-run
					break
				}
			} else if ok {
				successCount++
			}
			logger.Info("--------------------------------")
		}
	}

	skippedCount := totalCount - successCount - failureCount

	logger.Info("migrated merge requests from GitLab to GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "successful", successCount, "failed", failureCount, "skipped", skippedCount)
}

func (p *project) createEmptyPullRequest(ctx context.Context, mergeRequestIID int) (bool, error) {
	// Check for context cancellation
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("preparing to list pull requests: %v", err)
	}

	logger.Trace("creating empty pull request", "merge_request_id", mergeRequestIID)

	sourceBranchForEmptyMergeRequest := fmt.Sprintf("migration-source-%d/%s", mergeRequestIID, "empty")
	targetBranchForEmptyMergeRequest := fmt.Sprintf("migration-target-%d/%s", mergeRequestIID, "empty")

	var pullRequest *github.PullRequest

	logger.Debug("searching for any existing pull request", "owner", p.githubPath[0], "repo", p.githubPath[1], "merge_request_id", mergeRequestIID)
	sourceBranches := []string{sourceBranchForEmptyMergeRequest}
	branchQuery := fmt.Sprintf("head:%s", strings.Join(sourceBranches, " OR head:"))
	query := fmt.Sprintf("repo:%s/%s AND is:pr AND (%s)", p.githubPath[0], p.githubPath[1], branchQuery)
	searchResult, err := getGithubSearchResults(ctx, query)
	if err != nil {
		return false, fmt.Errorf("listing pull requests: %v", err)
	}
	logger.Debug("search results for pull requests", "owner", p.githubPath[0], "repo", p.githubPath[1], "merge_request_id", mergeRequestIID, "count", len(searchResult.Issues))

	// Look for an existing GitHub pull request
	for _, issue := range searchResult.Issues {
		if issue == nil {
			continue
		}

		// Check for context cancellation
		if err := ctx.Err(); err != nil {
			return false, fmt.Errorf("preparing to retrieve pull request: %v", err)
		}

		if issue.IsPullRequest() {
			// Use the issue number directly (PRs are issues in GitHub)
			prNumber := issue.GetNumber()
			if prNumber == 0 {
				continue
			}

			pr, err := getGithubPullRequest(ctx, p.githubPath[0], p.githubPath[1], prNumber)
			if err != nil {
				return false, fmt.Errorf("retrieving pull request: %v", err)
			}

			logger.Info("found existing pull request", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pr.GetNumber())
			pullRequest = pr
			break
		}
	}

	if pullRequest != nil && pullRequest.State != nil && strings.EqualFold(*pullRequest.State, "closed") {
		logger.Info("found existing pull request with original merged request both closed/merged. Skipping", "merge_request_id", mergeRequestIID, "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())
		return false, nil
	}

	commit, err := p.createEmptyCommit()
	if err != nil {
		return false, err
	}
	startCommit := commit
	endCommit := commit

	// Generate temporary branch names
	sourceBranch := sourceBranchForEmptyMergeRequest
	targetBranch := targetBranchForEmptyMergeRequest

	// Proceed to create temporary branches when migrating the empty MR doesn't yet have a counterpart PR in GitHub (can't create one without a branch)
	if pullRequest == nil {
		// look for the parent commit on the target branch to branch out from
		if startCommit.NumParents() > 1 {
			logger.Warn("start commit has multiple parents", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequestIID, "sha", startCommit.Hash, "num_parents", startCommit.NumParents())
		}
		logger.Trace("inspecting start commit parent", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequestIID, "sha", startCommit.Hash)
		startCommitParent, err := startCommit.Parent(0)
		if err != nil || startCommitParent == nil {
			return false, fmt.Errorf("identifying suitable parent of start commit %s for merge request %d", startCommit.Hash, mergeRequestIID)
		}

		logger.Trace("creating target branch for merged/closed merge request", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequestIID, "branch", targetBranch, "sha", startCommitParent.Hash)
		targetBranchRef := plumbing.NewBranchReferenceName(targetBranch)
		newTargetBranchRef := plumbing.NewHashReference(targetBranchRef, startCommitParent.Hash)
		if err = p.repo.Storer.SetReference(newTargetBranchRef); err != nil {
			return false, fmt.Errorf("creating target branch reference: %v", err)
		}

		logger.Trace("creating source branch for merged/closed merge request", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequestIID, "branch", sourceBranch, "sha", endCommit.Hash)
		sourceBranchRef := plumbing.NewBranchReferenceName(sourceBranch)
		newSourceBranchRef := plumbing.NewHashReference(sourceBranchRef, endCommit.Hash)
		if err = p.repo.Storer.SetReference(newSourceBranchRef); err != nil {
			return false, fmt.Errorf("creating source branch reference: %v", err)
		}

		logger.Debug("pushing branches for merged/closed merge request", "merge_request_id", mergeRequestIID, "owner", p.githubPath[0], "repo", p.githubPath[1], "source_branch", sourceBranch, "target_branch", targetBranch)
		if err = p.repo.PushContext(ctx, &git.PushOptions{
			RemoteName: "github",
			RefSpecs: []config.RefSpec{
				config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", sourceBranch)),
				config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", targetBranch)),
			},
			Force: true,
		}); err != nil {
			if errors.Is(err, git.NoErrAlreadyUpToDate) {
				logger.Trace("branch already exists and is up-to-date on GitHub", "merge_request_id", mergeRequestIID, "owner", p.githubPath[0], "repo", p.githubPath[1], "source_branch", sourceBranch, "target_branch", targetBranch)
			} else {
				return false, fmt.Errorf("pushing temporary branches to github: %v", err)
			}
		}

		// We will clean up these temporary branches after configuring and closing the pull request
		defer func() {
			logger.Debug("deleting temporary branches for closed pull request", "merge_request_id", mergeRequestIID, "owner", p.githubPath[0], "repo", p.githubPath[1], "source_branch", sourceBranch, "target_branch", targetBranch)
			if err := p.repo.PushContext(ctx, &git.PushOptions{
				RemoteName: "github",
				RefSpecs: []config.RefSpec{
					config.RefSpec(fmt.Sprintf(":refs/heads/%s", sourceBranch)),
					config.RefSpec(fmt.Sprintf(":refs/heads/%s", targetBranch)),
				},
				Force: true,
			}); err != nil {
				if errors.Is(err, git.NoErrAlreadyUpToDate) {
					logger.Trace("branches already deleted on GitHub", "merge_request_id", mergeRequestIID, "owner", p.githubPath[0], "repo", p.githubPath[1], "source_branch", sourceBranch, "target_branch", targetBranch)
				} else {
					sendErr(fmt.Errorf("pushing branch deletions to github: %v", err))
				}
			}

		}()
	}

	/*********************************************************
	* Create or Edit Pull Request
	**********************************************************/

	pullRequestTitle := fmt.Sprintf("Migration: Merge Request %d", mergeRequestIID)
	pullRequestBody := fmt.Sprintf("Merge Request %d was originally empty.", mergeRequestIID)

	if pullRequest == nil {
		newPullRequest := github.NewPullRequest{
			Title:               pointer(pullRequestTitle),
			Head:                &sourceBranch,
			Base:                &targetBranch,
			Body:                pointer(pullRequestBody),
			MaintainerCanModify: pointer(true),
			Draft:               pointer(false),
		}
		if pullRequest, _, err = gh.PullRequests.Create(ctx, p.githubPath[0], p.githubPath[1], &newPullRequest); err != nil {
			return false, fmt.Errorf("creating pull request: %v", err)
		}
		logger.Info("created pull request", "merge_request_id", mergeRequestIID, "owner", p.githubPath[0], "repo", p.githubPath[1], "source_branch", sourceBranch, "target_branch", targetBranch)

		pullRequest.State = pointer("closed")
		if pullRequest, _, err = gh.PullRequests.Edit(ctx, p.githubPath[0], p.githubPath[1], pullRequest.GetNumber(), pullRequest); err != nil {
			return false, fmt.Errorf("updating pull request: %v", err)
		}
		logger.Info("closed pull request", "merge_request_id", mergeRequestIID, "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())

	} else {
		if (pullRequest.Title == nil || *pullRequest.Title != pullRequestTitle) ||
			(pullRequest.Body == nil || *pullRequest.Body != pullRequestBody) {
			logger.Info("updating pull request", "merge_request_id", mergeRequestIID, "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())

			pullRequest.Title = &pullRequestTitle
			pullRequest.Body = &pullRequestBody
			pullRequest.MaintainerCanModify = nil
			if pullRequest, _, err = gh.PullRequests.Edit(ctx, p.githubPath[0], p.githubPath[1], pullRequest.GetNumber(), pullRequest); err != nil {
				return false, fmt.Errorf("updating pull request: %v", err)
			}
			logger.Info("updated pull request", "merge_request_id", mergeRequestIID, "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())
		} else {
			logger.Trace("existing pull request is up-to-date", "merge_request_id", mergeRequestIID, "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())
		}
	}

	return true, nil
}

func (p *project) fetchPullRequest(ctx context.Context, mergeRequest *gitlab.MergeRequest) (*github.PullRequest, error) {
	sourceBranchForClosedMergeRequest := fmt.Sprintf("migration-source-%d/%s", mergeRequest.IID, mergeRequest.SourceBranch)

	var pullRequest *github.PullRequest

	logger.Debug("searching for any existing pull request", "owner", p.githubPath[0], "repo", p.githubPath[1], "merge_request_id", mergeRequest.IID)
	sourceBranches := []string{mergeRequest.SourceBranch, sourceBranchForClosedMergeRequest}
	branchQuery := fmt.Sprintf("head:%s", strings.Join(sourceBranches, " OR head:"))
	query := fmt.Sprintf("repo:%s/%s AND is:pr AND (%s)", p.githubPath[0], p.githubPath[1], branchQuery)
	searchResult, err := getGithubSearchResults(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("listing pull requests: %v", err)
	}

	// Look for an existing GitHub pull request
	for _, issue := range searchResult.Issues {
		if issue == nil {
			continue
		}

		// Check for context cancellation
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("preparing to retrieve pull request: %v", err)
		}

		if issue.IsPullRequest() {
			// Use the issue number directly (PRs are issues in GitHub)
			prNumber := issue.GetNumber()
			if prNumber == 0 {
				continue
			}

			pr, err := getGithubPullRequest(ctx, p.githubPath[0], p.githubPath[1], prNumber)
			if err != nil {
				return nil, fmt.Errorf("retrieving pull request: %v", err)
			}

			if strings.Contains(pr.GetBody(), fmt.Sprintf("**GitLab MR Number** | %d", mergeRequest.IID)) ||
				strings.Contains(pr.GetBody(), fmt.Sprintf("**GitLab MR Number** | [%d]", mergeRequest.IID)) {
				logger.Info("found existing pull request", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pr.GetNumber())
				pullRequest = pr
				break
			}
		}
	}
	return pullRequest, nil
}

func (p *project) migrateMergeRequestWithoutComments(ctx context.Context, mergeRequest *gitlab.MergeRequest, pullRequest *github.PullRequest) (bool, error) {
	// Check for context cancellation
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("preparing to list pull requests: %v", err)
	}

	logger.Trace("merge request", "merge_request", mergeRequest)

	sourceBranchForClosedMergeRequest := fmt.Sprintf("migration-source-%d/%s", mergeRequest.IID, mergeRequest.SourceBranch)
	targetBranchForClosedMergeRequest := fmt.Sprintf("migration-target-%d/%s", mergeRequest.IID, mergeRequest.TargetBranch)

	if pullRequest != nil && pullRequest.State != nil && strings.EqualFold(*pullRequest.State, "closed") && !strings.EqualFold(mergeRequest.State, "opened") {
		logger.Info("found existing pull request with original merged request both closed/merged. Skipping", "merge_request_id", mergeRequest.IID, "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())
		return false, nil
	}

	mergeRequestCommits, startCommit, endCommit, err := p.getMergeRequestCommits(ctx, mergeRequest)
	if err != nil {
		return false, fmt.Errorf("getting merge request commits: %v", err)
	}

	mergeRequestCommitsSectionStr := fmt.Sprintf("## Original Commits (%d in total):\n", len(mergeRequestCommits))
	for _, commit := range mergeRequestCommits {
		mergeRequestCommitsSectionStr += fmt.Sprintf(
			"[`%[1]s`](https://%[5]s/%[6]s/%[7]s/commit/%[8]s) `%[2]s`: %[4]s - [%[3]s](mailto:%[9]s)\n",
			commit.ShortID[:7],
			commit.CommittedDate.Format(dateFormat),
			// commit.AuthoredDate.Format(dateFormat),
			commit.AuthorName,
			commit.Title,
			githubDomain,
			p.githubPath[0],
			p.githubPath[1],
			commit.ID,
			commit.AuthorEmail,
		)
	}
	// logger.Debug("merge request commits", "merge_request_id", mergeRequest.IID, "commits", mergeRequestCommitsSectionStr)

	// // early return for debugging
	// return true, nil

	if strings.EqualFold(mergeRequest.State, "opened") {
		if _, err = p.repo.Branch(mergeRequest.SourceBranch); err != nil {
			if errors.Is(err, git.ErrBranchNotFound) {
				// there is a problem that some branches can't be seen locally despite they should have been mirrored
				// but creating the GitHub PR later works fine because the branch is already created on GitHub
				// so we will skip the verification for now
				// if _, err = p.repo.Branch(mergeRequest.SourceBranch); err != nil {
				// 	return false, fmt.Errorf("source branch %s does not exist for merge request %d after creation: %v", mergeRequest.SourceBranch, mergeRequest.IID, err)
				// }
			} else {
				return false, fmt.Errorf("checking source branch for merge request: %v", err)
			}
		}
	}

	// Proceed to create temporary branches when migrating a merged/closed merge request that doesn't yet have a counterpart PR in GitHub (can't create one without a branch)
	if pullRequest == nil && !strings.EqualFold(mergeRequest.State, "opened") {
		// Generate temporary branch names
		mergeRequest.SourceBranch = sourceBranchForClosedMergeRequest
		mergeRequest.TargetBranch = targetBranchForClosedMergeRequest

		// look for the parent commit on the target branch to branch out from
		if startCommit.NumParents() > 1 {
			logger.Warn("start commit has multiple parents", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "sha", startCommit.Hash, "num_parents", startCommit.NumParents())
		}
		logger.Trace("inspecting start commit parent", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "sha", startCommit.Hash)
		startCommitParent, err := startCommit.Parent(0)
		if err != nil || startCommitParent == nil {
			return false, fmt.Errorf("identifying suitable parent of start commit %s for merge request %d", startCommit.Hash, mergeRequest.IID)
		}

		logger.Trace("creating target branch for merged/closed merge request", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "branch", mergeRequest.TargetBranch, "sha", startCommitParent.Hash)
		targetBranchRef := plumbing.NewBranchReferenceName(mergeRequest.TargetBranch)
		newTargetBranchRef := plumbing.NewHashReference(targetBranchRef, startCommitParent.Hash)
		if err = p.repo.Storer.SetReference(newTargetBranchRef); err != nil {
			return false, fmt.Errorf("creating target branch reference: %v", err)
		}

		logger.Trace("creating source branch for merged/closed merge request", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "branch", mergeRequest.SourceBranch, "sha", endCommit.Hash)
		sourceBranchRef := plumbing.NewBranchReferenceName(mergeRequest.SourceBranch)
		newSourceBranchRef := plumbing.NewHashReference(sourceBranchRef, endCommit.Hash)
		if err = p.repo.Storer.SetReference(newSourceBranchRef); err != nil {
			return false, fmt.Errorf("creating source branch reference: %v", err)
		}

		logger.Debug("pushing branches for merged/closed merge request", "merge_request_id", mergeRequest.IID, "owner", p.githubPath[0], "repo", p.githubPath[1], "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
		if err = p.repo.PushContext(ctx, &git.PushOptions{
			RemoteName: "github",
			RefSpecs: []config.RefSpec{
				config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", mergeRequest.SourceBranch)),
				config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", mergeRequest.TargetBranch)),
			},
			Force: true,
		}); err != nil {
			if errors.Is(err, git.NoErrAlreadyUpToDate) {
				logger.Trace("branch already exists and is up-to-date on GitHub", "merge_request_id", mergeRequest.IID, "owner", p.githubPath[0], "repo", p.githubPath[1], "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
			} else {
				return false, fmt.Errorf("pushing temporary branches to github: %v", err)
			}
		}

		// We will clean up these temporary branches after configuring and closing the pull request
		defer func() {
			logger.Debug("deleting temporary branches for closed pull request", "merge_request_id", mergeRequest.IID, "owner", p.githubPath[0], "repo", p.githubPath[1], "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
			if err := p.repo.PushContext(ctx, &git.PushOptions{
				RemoteName: "github",
				RefSpecs: []config.RefSpec{
					config.RefSpec(fmt.Sprintf(":refs/heads/%s", mergeRequest.SourceBranch)),
					config.RefSpec(fmt.Sprintf(":refs/heads/%s", mergeRequest.TargetBranch)),
				},
				Force: true,
			}); err != nil {
				if errors.Is(err, git.NoErrAlreadyUpToDate) {
					logger.Trace("branches already deleted on GitHub", "merge_request_id", mergeRequest.IID, "owner", p.githubPath[0], "repo", p.githubPath[1], "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
				} else {
					sendErr(fmt.Errorf("pushing branch deletions to github: %v", err))
				}
			}

		}()
	}

	if p.defaultBranch != p.project.DefaultBranch && mergeRequest.TargetBranch == p.project.DefaultBranch {
		logger.Trace("changing target trunk branch", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "old_trunk", p.project.DefaultBranch, "new_trunk", p.defaultBranch)
		mergeRequest.TargetBranch = p.defaultBranch
	}

	/*********************************************************
	* Assemble Pull Request Body
	**********************************************************/

	mergeRequestAuthorStr := ""
	author, err := getGitlabUser(mergeRequest.Author.Username)
	if err == nil {
		// logger.Trace("GitLab full user for author", "author", author)
		mergeRequestAuthorStr = fmt.Sprintf("[%s (`@%s`)](mailto:%s)", mergeRequest.Author.Name, mergeRequest.Author.Username, author.Email)
	} else {
		mergeRequestAuthorStr = fmt.Sprintf("%s (`@%s`)", mergeRequest.Author.Name, mergeRequest.Author.Username)
	}

	logger.Debug("determining merge request approvers", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID)

	approvers := make([]string, 0)
	// First try real approvals
	approvalState, _, err := gl.MergeRequestApprovals.GetApprovalState(
		p.project.ID,
		mergeRequest.IID,
	)
	if err != nil || approvalState == nil {
		sendErr(fmt.Errorf("getting MR approval state: %v", err))
	} else {
		// Use a set to avoid duplicates across rules
		seen := make(map[int]bool) // or map[string]bool keyed by username

		for _, rule := range approvalState.Rules {
			// rule.ApprovedBy is []*gitlab.BasicUser
			for _, u := range rule.ApprovedBy {
				if u == nil {
					continue
				}

				// De-dupe by user ID (or username if you prefer)
				if seen[u.ID] {
					continue
				}
				seen[u.ID] = true

				approverStr := ""
				// Re-use your existing GitLab->GitHub mapping logic
				approverUser, err := getGitlabUser(u.Username)
				if err == nil {
					// logger.Trace("GitLab full user for approver", "approver", approverUser)
					approverStr = fmt.Sprintf("[%s (`@%s`)](mailto:%s)", u.Name, u.Username, approverUser.Email)
				} else {
					approverStr = fmt.Sprintf("%s (`@%s`)", u.Name, u.Username)
				}

				approvers = append(approvers, approverStr)
			}
		}
	}

	description := p.migrateTextBody(mergeRequest.Description, mergeRequest.IID)
	if strings.TrimSpace(description) == "" {
		description = "_No description_"
	}

	slices.Sort(approvers)
	approval := strings.Join(approvers, ", ")
	if approval == "" {
		approval = "_No approvers_"
	}

	closeDateRow := ""
	if mergeRequest.State == "closed" && mergeRequest.ClosedAt != nil {
		closeDateRow = fmt.Sprintf("\n> | **Time Originally Closed** | %s |", mergeRequest.ClosedAt.Format(dateFormat))
	} else if mergeRequest.State == "merged" && mergeRequest.MergedAt != nil {
		closeDateRow = fmt.Sprintf("\n> | **Time Originally Merged** | %s |", mergeRequest.MergedAt.Format(dateFormat))
	}

	title := fmt.Sprintf("%s - %s", mergeRequest.Title, mergeRequest.Author.Name)

	mergeOrCloseNote := ""
	if strings.EqualFold(mergeRequest.State, "merged") {
		if mergeRequest.MergeCommitSHA != "" {
			shortSHA := mergeRequest.MergeCommitSHA
			if len(shortSHA) > 7 {
				shortSHA = shortSHA[:7]
			}
			commitStr := fmt.Sprintf("[`%s`](https://%s/%s/%s/commit/%s)", shortSHA, githubDomain, p.githubPath[0], p.githubPath[1], mergeRequest.MergeCommitSHA)
			mergeOrCloseNote = fmt.Sprintf("> This merge request was originally **merged** on GitLab (merge commit %s).", commitStr)
		} else if mergeRequest.SquashCommitSHA != "" {
			shortSHA := mergeRequest.SquashCommitSHA
			if len(shortSHA) > 7 {
				shortSHA = shortSHA[:7]
			}
			commitStr := fmt.Sprintf("[`%s`](https://%s/%s/%s/commit/%s)", shortSHA, githubDomain, p.githubPath[0], p.githubPath[1], mergeRequest.SquashCommitSHA)
			mergeOrCloseNote = fmt.Sprintf("> This merge request was originally **merged** on GitLab (squash commit %s).", commitStr)
		}
	} else {
		mergeOrCloseNote = fmt.Sprintf("> This merge request was originally **%s** on GitLab.", mergeRequest.State)
	}

	body := fmt.Sprintf(`> [!NOTE]
> This pull request was migrated from GitLab.
>
> |      |      |
> | ---- | ---- |
> | **Original Author** | %[1]s |
> | **GitLab MR Number** | [%[2]d](https://%[10]s/%[4]s/%[5]s/merge_requests/%[2]d) |
> | **Time Originally Opened** | %[6]s |%[7]s
> | **Approved on GitLab by** | %[8]s |
> |      |      |
>
%[9]s

%[11]s
## Original Description

%[3]s`, mergeRequestAuthorStr, mergeRequest.IID, description, p.gitlabPath[0], p.gitlabPath[1], mergeRequest.CreatedAt.Format(dateFormat), closeDateRow, approval, mergeOrCloseNote, gitlabDomain, mergeRequestCommitsSectionStr)

	/*********************************************************
	* Create or Edit Pull Request
	**********************************************************/

	if pullRequest == nil {
		newPullRequest := github.NewPullRequest{
			Title:               &title,
			Head:                &mergeRequest.SourceBranch,
			Base:                &mergeRequest.TargetBranch,
			Body:                &body,
			MaintainerCanModify: pointer(true),
			Draft:               &mergeRequest.Draft,
		}
		if pullRequest, _, err = gh.PullRequests.Create(ctx, p.githubPath[0], p.githubPath[1], &newPullRequest); err != nil {
			if strings.Contains(err.Error(), "body is too long (maximum is 65536 characters)") {
				logger.Warn("the body is too long", "owner", p.githubPath[0], "repo", p.githubPath[1], "merge_request_id", mergeRequest.IID)
				newPullRequest.Body = pointer(body[:65536])
				if pullRequest, _, err = gh.PullRequests.Create(ctx, p.githubPath[0], p.githubPath[1], &newPullRequest); err != nil {
					return false, fmt.Errorf("creating pull request: %v", err)
				}
			} else if strings.Contains(err.Error(), "No commits between") {
				return false, fmt.Errorf("skipping merge request as the change is already present in trunk branch: %v", err)
			} else {
				return false, fmt.Errorf("creating pull request: %v", err)
			}
		}
		logger.Info("created pull request", "merge_request_id", mergeRequest.IID, "owner", p.githubPath[0], "repo", p.githubPath[1], "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)

		if mergeRequest.State == "closed" || mergeRequest.State == "merged" {
			pullRequest.State = pointer("closed")
			if pullRequest, _, err = gh.PullRequests.Edit(ctx, p.githubPath[0], p.githubPath[1], pullRequest.GetNumber(), pullRequest); err != nil {
				return false, fmt.Errorf("updating pull request: %v", err)
			}
			logger.Info("closed pull request", "merge_request_id", mergeRequest.IID, "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())
		}

	} else {
		var newState *string
		switch mergeRequest.State {
		case "opened":
			newState = pointer("open")
		case "closed", "merged":
			newState = pointer("closed")
		}

		if pullRequest.State != nil && newState != nil && *pullRequest.State != *newState {
			pullRequestState := &github.PullRequest{
				Number: pullRequest.Number,
				State:  newState,
			}

			if pullRequest, _, err = gh.PullRequests.Edit(ctx, p.githubPath[0], p.githubPath[1], pullRequestState.GetNumber(), pullRequestState); err != nil {
				return false, fmt.Errorf("updating pull request state: %v", err)
			}
			logger.Info("updated pull request state", "merge_request_id", mergeRequest.IID, "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())
		}

		if (newState != nil && (pullRequest.State == nil || *pullRequest.State != *newState)) ||
			(pullRequest.Title == nil || *pullRequest.Title != mergeRequest.Title) ||
			(pullRequest.Body == nil || *pullRequest.Body != body) ||
			(pullRequest.Draft == nil || *pullRequest.Draft != mergeRequest.Draft) {
			logger.Info("updating pull request", "merge_request_id", mergeRequest.IID, "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())

			pullRequest.Title = &title
			pullRequest.Body = &body
			pullRequest.Draft = &mergeRequest.Draft
			pullRequest.MaintainerCanModify = nil
			if pullRequest, _, err = gh.PullRequests.Edit(ctx, p.githubPath[0], p.githubPath[1], pullRequest.GetNumber(), pullRequest); err != nil {
				return false, fmt.Errorf("updating pull request: %v", err)
			}
			logger.Info("updated pull request", "merge_request_id", mergeRequest.IID, "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())
		} else {
			logger.Trace("existing pull request is up-to-date", "merge_request_id", mergeRequest.IID, "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())
		}
	}

	return true, nil
}

func (p *project) migrateMergeRequestComments(ctx context.Context, mergeRequest *gitlab.MergeRequest, pullRequest *github.PullRequest) (bool, error) {
	// get all gitlab discussions
	var discussions []*gitlab.Discussion
	discussionsOpts := &gitlab.ListMergeRequestDiscussionsOptions{
		OrderBy: "created_at",
		Sort:    "asc",
		PerPage: 100,
	}
	nPages := 0
	for {
		result, resp, err := gl.Discussions.ListMergeRequestDiscussions(p.project.ID, mergeRequest.IID, discussionsOpts)
		if err != nil {
			return false, fmt.Errorf("listing merge request discussions: %v", err)
		}
		discussions = append(discussions, result...)

		nPages++
		if resp.NextPage == 0 {
			break
		}

		discussionsOpts.Page = resp.NextPage
	}
	logger.Debug("retrieved GitLab merge request discussions", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "count", len(discussions), "n_pages", nPages)

	// get all gitlab merge request comments
	var comments []*gitlab.Note
	commentsOpts := &gitlab.ListMergeRequestNotesOptions{
		ListOptions: gitlab.ListOptions{
			PerPage: 100,
		},
		OrderBy: pointer("created_at"),
		Sort:    pointer("asc"),
	}
	nPages = 0
	for {
		result, resp, err := gl.Notes.ListMergeRequestNotes(p.project.ID, mergeRequest.IID, commentsOpts)
		if err != nil {
			return false, fmt.Errorf("listing merge request notes: %v", err)
		}

		comments = append(comments, result...)

		nPages++
		if resp.NextPage == 0 {
			break
		}

		commentsOpts.Page = resp.NextPage
	}
	logger.Debug("retrieved GitLab merge request comments", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "count", len(comments), "n_pages", nPages)

	var prComments []*github.IssueComment
	ghCommentsOpts := &github.IssueListCommentsOptions{Sort: pointer("created"), Direction: pointer("asc"), ListOptions: github.ListOptions{PerPage: 100}}
	nPages = 0
	for {
		result, resp, err := gh.Issues.ListComments(ctx, p.githubPath[0], p.githubPath[1], pullRequest.GetNumber(), ghCommentsOpts)
		if err != nil {
			return false, fmt.Errorf("listing pull request comments: %v", err)
		}
		prComments = append(prComments, result...)

		nPages++
		if resp.NextPage == 0 {
			break
		}

		ghCommentsOpts.Page = resp.NextPage
	}
	logger.Debug("retrieved GitHub pull request comments", "merge_request_id", mergeRequest.IID, "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "count", len(prComments), "n_pages", nPages)

	/*********************************************************
	* Migrate Discussions
	**********************************************************/

	// trim discussion notes
	var trimmedDiscussions []*gitlab.Discussion
	for _, discussion := range discussions {
		if discussion == nil {
			continue
		}

		// filter out system comments
		var discussionNotes []*gitlab.Note
		for _, comment := range discussion.Notes {
			if comment == nil || comment.System {
				continue
			}

			if strings.HasPrefix(comment.Author.Username, "GitLab-") || strings.HasPrefix(comment.Author.Username, "gitlab-") {
				logger.Trace("skipping system comment", "merge_request_id", mergeRequest.IID, "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "gl_comment_id", comment.ID)
				continue
			}

			discussionNotes = append(discussionNotes, comment)
		}

		if len(discussionNotes) == 0 {
			continue
		}

		// sort by created at ascending
		sort.Slice(discussionNotes, func(i, j int) bool {
			return discussionNotes[i].CreatedAt.Before(*discussionNotes[j].CreatedAt) // ascending
		})

		trimmedDiscussions = append(trimmedDiscussions, &gitlab.Discussion{
			ID:    discussion.ID,
			Notes: discussionNotes,
		})
	}

	// sort by first note created at ascending
	sort.Slice(trimmedDiscussions, func(i, j int) bool {
		return trimmedDiscussions[i].Notes[0].CreatedAt.Before(*trimmedDiscussions[j].Notes[0].CreatedAt) // ascending
	})

	logger.Info("migrating merge request discussions from GitLab to GitHub", "merge_request_id", mergeRequest.IID, "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "discussions", len(discussions), "trimmed_discussions", len(trimmedDiscussions))

	var discussionCommentIds []int
	for _, discussion := range trimmedDiscussions {
		positionIsTheSame := true
		// we know the Notes has at least one note, so we can safely access the first note's position
		discussionPosition := discussion.Notes[0].Position
		for _, comment := range discussion.Notes {
			if comment.Position != nil && (comment.Position.OldPath != discussionPosition.OldPath || comment.Position.NewPath != discussionPosition.NewPath || comment.Position.OldLine != discussionPosition.OldLine || comment.Position.NewLine != discussionPosition.NewLine) {
				positionIsTheSame = false
				break
			}
		}
		logger.Trace("discussion position", "merge_request_id", mergeRequest.IID, "discussion_id", discussion.ID, "position_is_the_same", positionIsTheSame)

		discussionDiffNoteRow := ""
		if positionIsTheSame && discussionPosition != nil {
			lineNumberStr := ""
			if discussionPosition.NewLine != 0 && discussionPosition.OldLine != 0 {
				lineNumberStr = fmt.Sprintf("%d->%d", discussionPosition.OldLine, discussionPosition.NewLine)
			} else if discussionPosition.NewLine != 0 {
				lineNumberStr = fmt.Sprintf("%d", discussionPosition.NewLine)
			} else if discussionPosition.OldLine != 0 {
				lineNumberStr = fmt.Sprintf("%d", discussionPosition.OldLine)
			}

			ghLink := generateGithubDiffLink(githubDomain, p.githubPath[0], p.githubPath[1], discussionPosition)

			if discussionPosition.OldPath == discussionPosition.NewPath {
				discussionDiffNoteRow = fmt.Sprintf("\n> | **Diff Note** | [`%s` (lineno `%s`)](%s) |", discussionPosition.OldPath, lineNumberStr, ghLink)
			} else {
				discussionDiffNoteRow = fmt.Sprintf("\n> | **Diff Note** | `%s` -> `%s` (lineno `%s`)](%s) |", discussionPosition.OldPath, discussionPosition.NewPath, lineNumberStr, ghLink)
			}
		}

		var commentsPerDiscussion []string
		for _, comment := range discussion.Notes {
			commentAuthorStr := ""
			if comment.Author.Email != "" {
				commentAuthorStr = fmt.Sprintf("[%s (`@%s`)](mailto:%s)", comment.Author.Name, comment.Author.Username, comment.Author.Email)
			} else {
				commentAuthor, err := getGitlabUser(comment.Author.Username)
				if err == nil {
					commentAuthorStr = fmt.Sprintf("[%s (`@%s`)](mailto:%s)", comment.Author.Name, comment.Author.Username, commentAuthor.Email)
				} else {
					commentAuthorStr = fmt.Sprintf("%s (`@%s`)", comment.Author.Name, comment.Author.Username)
				}
			}

			glCommentBodyStr := p.migrateTextBody(comment.Body, mergeRequest.IID)

			diffNoteRow := ""
			if !positionIsTheSame && comment.Position != nil {
				lineNumberStr := ""
				if comment.Position.NewLine != 0 && comment.Position.OldLine != 0 {
					lineNumberStr = fmt.Sprintf("%d->%d", comment.Position.OldLine, comment.Position.NewLine)
				} else if comment.Position.NewLine != 0 {
					lineNumberStr = fmt.Sprintf("%d", comment.Position.NewLine)
				} else if comment.Position.OldLine != 0 {
					lineNumberStr = fmt.Sprintf("%d", comment.Position.OldLine)
				}

				ghLink := generateGithubDiffLink(githubDomain, p.githubPath[0], p.githubPath[1], comment.Position)

				if comment.Position.OldPath == comment.Position.NewPath {
					diffNoteRow = fmt.Sprintf("\n[`%s` (lineno `%s`)](%s)", comment.Position.OldPath, lineNumberStr, ghLink)
				} else {
					diffNoteRow = fmt.Sprintf("\n[`%s` -> `%s` (lineno `%s`)](%s)", comment.Position.OldPath, comment.Position.NewPath, lineNumberStr, ghLink)
				}
			}

			commentBody := fmt.Sprintf("`%[3]s` %[1]s (Note [%[2]d](https://%[5]s/%[6]s/%[7]s/merge_requests/%[8]d#note_%[2]d))%[9]s\n\n%[4]s", commentAuthorStr, comment.ID, comment.CreatedAt.Format(dateFormat), glCommentBodyStr, gitlabDomain, p.gitlabPath[0], p.gitlabPath[1], mergeRequest.IID, diffNoteRow)

			commentsPerDiscussion = append(commentsPerDiscussion, commentBody)
			discussionCommentIds = append(discussionCommentIds, comment.ID)
		}

		ghCommentBody := fmt.Sprintf(`> [!NOTE]
> This discussion was migrated from GitLab.
>
> |      |      |
> | ---- | ---- |
> | **Discussion ID** | %[1]s |%[2]s
> |      |      |
>

`, fmt.Sprintf("`%s`", discussion.ID), discussionDiffNoteRow)
		ghCommentBody += strings.Join(commentsPerDiscussion, "\n\n---\n\n")

		foundExistingComment := false
		for _, prComment := range prComments {
			if prComment == nil {
				continue
			}
			// this has to match a portion of the comment body
			if strings.Contains(prComment.GetBody(), fmt.Sprintf("**Discussion ID** | `%s`", discussion.ID)) {
				foundExistingComment = true

				if prComment.Body == nil || *prComment.Body != ghCommentBody {
					logger.Debug("updating pull request comment", "merge_request_id", mergeRequest.IID, "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "gh_comment_id", prComment.GetID())
					prComment.Body = &ghCommentBody
					if _, _, err := gh.Issues.EditComment(ctx, p.githubPath[0], p.githubPath[1], prComment.GetID(), prComment); err != nil {
						return false, fmt.Errorf("updating pull request comments: %v", err)
					}
				} else {
					logger.Trace("existing pull request comment is up-to-date", "merge_request_id", mergeRequest.IID, "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "gh_comment_id", prComment.GetID())
				}
			}
		}

		if !foundExistingComment {
			logger.Debug("creating pull request comment", "merge_request_id", mergeRequest.IID, "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())
			newComment := github.IssueComment{
				Body: &ghCommentBody,
			}
			if _, _, err := gh.Issues.CreateComment(ctx, p.githubPath[0], p.githubPath[1], pullRequest.GetNumber(), &newComment); err != nil {
				return false, fmt.Errorf("creating pull request comment: %v", err)
			}
		}
	}

	/*********************************************************
	* Migrate Stray Comments
	**********************************************************/

	// stray comments that are not part of a discussion
	var strayComments []*gitlab.Note
	for _, comment := range comments {
		if comment == nil || comment.System || slices.Contains(discussionCommentIds, comment.ID) {
			continue
		}

		if strings.HasPrefix(comment.Author.Username, "GitLab-") || strings.HasPrefix(comment.Author.Username, "gitlab-") {
			logger.Trace("skipping system comment", "merge_request_id", mergeRequest.IID, "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "gl_comment_id", comment.ID)
			continue
		}

		strayComments = append(strayComments, comment)
	}

	// sort by created at ascending
	sort.Slice(strayComments, func(i, j int) bool {
		return strayComments[i].CreatedAt.Before(*strayComments[j].CreatedAt) // ascending
	})

	logger.Info("migrating merge request stray comments from GitLab to GitHub", "merge_request_id", mergeRequest.IID, "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "comments", len(comments), "stray_comments", len(strayComments))

	for _, comment := range strayComments {
		commentAuthorStr := ""
		if comment.Author.Email != "" {
			commentAuthorStr = fmt.Sprintf("[%s (`@%s`)](mailto:%s)", comment.Author.Name, comment.Author.Username, comment.Author.Email)
		} else {
			commentAuthor, err := getGitlabUser(comment.Author.Username)
			if err == nil {
				commentAuthorStr = fmt.Sprintf("[%s (`@%s`)](mailto:%s)", comment.Author.Name, comment.Author.Username, commentAuthor.Email)
			} else {
				commentAuthorStr = fmt.Sprintf("%s (`@%s`)", comment.Author.Name, comment.Author.Username)
			}
		}

		glCommentBodyStr := p.migrateTextBody(comment.Body, mergeRequest.IID)

		diffNoteRow := ""
		if comment.Position != nil {
			lineNumberStr := ""
			if comment.Position.NewLine != 0 && comment.Position.OldLine != 0 {
				lineNumberStr = fmt.Sprintf("%d->%d", comment.Position.OldLine, comment.Position.NewLine)
			} else if comment.Position.NewLine != 0 {
				lineNumberStr = fmt.Sprintf("%d", comment.Position.NewLine)
			} else if comment.Position.OldLine != 0 {
				lineNumberStr = fmt.Sprintf("%d", comment.Position.OldLine)
			}

			ghLink := generateGithubDiffLink(githubDomain, p.githubPath[0], p.githubPath[1], comment.Position)

			if comment.Position.OldPath == comment.Position.NewPath {
				diffNoteRow = fmt.Sprintf("\n> | **Diff Note** | [`%s` (lineno `%s`)](%s) |", comment.Position.OldPath, lineNumberStr, ghLink)
			} else {
				diffNoteRow = fmt.Sprintf("\n> | **Diff Note** | [`%s` -> `%s` (lineno `%s`)](%s) |", comment.Position.OldPath, comment.Position.NewPath, lineNumberStr, ghLink)
			}
		}

		commentBody := fmt.Sprintf(`> [!NOTE]
> This comment was migrated from GitLab.
>
> |      |      |
> | ---- | ---- |
> | **Original Author** | %[1]s |
> | **Note ID** | [%[2]d](https://%[5]s/%[6]s/%[7]s/merge_requests/%[8]d#note_%[2]d) |%[9]s
> | **Time Originally Created** | %[3]s |
> |      |      |
>

%[4]s`, commentAuthorStr, comment.ID, comment.CreatedAt.Format(dateFormat), glCommentBodyStr, gitlabDomain, p.gitlabPath[0], p.gitlabPath[1], mergeRequest.IID, diffNoteRow)

		foundExistingComment := false
		for _, prComment := range prComments {
			if prComment == nil {
				continue
			}

			if strings.Contains(prComment.GetBody(), fmt.Sprintf("**Note ID** | %d", comment.ID)) || strings.Contains(prComment.GetBody(), fmt.Sprintf("**Note ID** | [%d]", comment.ID)) {
				foundExistingComment = true

				if prComment.Body == nil || *prComment.Body != commentBody {
					logger.Debug("updating pull request comment", "merge_request_id", mergeRequest.IID, "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "gh_comment_id", prComment.GetID())
					prComment.Body = &commentBody
					if _, _, err := gh.Issues.EditComment(ctx, p.githubPath[0], p.githubPath[1], prComment.GetID(), prComment); err != nil {
						return false, fmt.Errorf("updating pull request comments: %v", err)
					}
				} else {
					logger.Trace("existing pull request comment is up-to-date", "merge_request_id", mergeRequest.IID, "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "gh_comment_id", prComment.GetID())
				}
			}
		}

		if !foundExistingComment {
			logger.Debug("creating pull request comment", "merge_request_id", mergeRequest.IID, "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())
			newComment := github.IssueComment{
				Body: &commentBody,
			}
			if _, _, err := gh.Issues.CreateComment(ctx, p.githubPath[0], p.githubPath[1], pullRequest.GetNumber(), &newComment); err != nil {
				return false, fmt.Errorf("creating pull request comment: %v", err)
			}
		}
	}

	return true, nil
}

func (p *project) getMergeRequestCommits(ctx context.Context, mergeRequest *gitlab.MergeRequest) ([]*gitlab.Commit, *object.Commit, *object.Commit, error) {
	logger.Trace("retrieving commits for merge request", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID)
	var mergeRequestCommits []*gitlab.Commit
	opts := &gitlab.GetMergeRequestCommitsOptions{
		OrderBy: "created_at",
		Sort:    "asc",
		PerPage: 100,
	}
	nPages := 0
	for {
		result, resp, err := gl.MergeRequests.GetMergeRequestCommits(p.project.ID, mergeRequest.IID, opts)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("retrieving merge request commits: %v", err)
		}

		mergeRequestCommits = append(mergeRequestCommits, result...)

		nPages++
		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}
	logger.Info("retrieved merge request commits", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "count", len(mergeRequestCommits), "n_pages", nPages)

	var startCommit, endCommit *object.Commit
	// Some merge requests have no commits, create an empty commit instead
	if len(mergeRequestCommits) == 0 {
		logger.Debug("merge request has no commits, creating empty commit", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID)
		commit, err := p.createEmptyCommit()
		if err != nil {
			return nil, nil, nil, err
		}
		startCommit = commit
		endCommit = commit
		mergeRequestCommits = []*gitlab.Commit{commit2GitlabCommit(commit)}
	} else {
		// API is buggy, ordering is not respected, so we'll reorder by commit datestamp
		sort.Slice(mergeRequestCommits, func(i, j int) bool {
			// return mergeRequestCommits[i].CommittedDate.Before(*mergeRequestCommits[j].CommittedDate) // sometimes the commits have the same commited timestamp
			return mergeRequestCommits[i].AuthoredDate.Before(*mergeRequestCommits[j].AuthoredDate)
		})

		if mergeRequestCommits[0] == nil {
			return nil, nil, nil, fmt.Errorf("start commit for merge request %d is nil", mergeRequest.IID)
		}
		if mergeRequestCommits[len(mergeRequestCommits)-1] == nil {
			return nil, nil, nil, fmt.Errorf("end commit for merge request %d is nil", mergeRequest.IID)
		}

		var fetchedCommits []*object.Commit
		// reverse commits during range iteration to start from the last commit
		// this saves many fetches because fetching the last commit will fetch all the parents
		for i := len(mergeRequestCommits) - 1; i >= 0; i-- {
			commit := mergeRequestCommits[i]
			logger.Trace("commit", "id", commit.ID, "shortId", commit.ShortID, "authoredDate", commit.AuthoredDate, "committedDate", commit.CommittedDate)
			// logger.Trace("  parentIDs", "parentIDs", commit.ParentIDs) // this ends up to be an empty array for squashed commits

			fetchedCommit, fetchErr := p.fetchCommitFromRemote(ctx, commit.ID)
			if fetchErr != nil || fetchedCommit == nil {
				return nil, nil, nil, fmt.Errorf("fetching commit %s: %v", commit.ID, fetchErr)
			}
			logger.Trace("fetchedCommit", "hash", fetchedCommit.Hash, "parentHashes", fetchedCommit.ParentHashes)
			fetchedCommits = append(fetchedCommits, fetchedCommit)
		}

		// endCommit is the head commit, startCommit is the tail commit, but parent of the startCommit is the root on the target branch
		endCommit, startCommit = findHeadAndTailCommits(fetchedCommits)
		if endCommit == nil || startCommit == nil {
			// fallback to using the first and last commits from the merge request
			logger.Warn("no head or tail commit found in fetched commits", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID)
			var err error
			startCommit, err = object.GetCommit(p.repo.Storer, plumbing.NewHash(mergeRequestCommits[0].ID))
			if err != nil {
				return nil, nil, nil, fmt.Errorf("loading start commit %s (merge request %d): commit exists in GitLab but not in cloned repository: %v", startCommit.Hash, mergeRequest.IID, err)
			}
			endCommit, err = object.GetCommit(p.repo.Storer, plumbing.NewHash(mergeRequestCommits[len(mergeRequestCommits)-1].ID))
			if err != nil {
				return nil, nil, nil, fmt.Errorf("loading end commit %s (merge request %d): commit exists in GitLab but not in cloned repository: %v", endCommit.Hash, mergeRequest.IID, err)
			}
		}
	}

	if startCommit.NumParents() == 0 {
		// Orphaned commit, we cannot open a pull request as GitHub rejects it
		logger.Debug("merge request has orphaned commits, creating empty commit instead", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID)
		commit, err := p.createEmptyCommit()
		if err != nil {
			return nil, nil, nil, err
		}
		startCommit = commit
		endCommit = commit
		mergeRequestCommits = []*gitlab.Commit{commit2GitlabCommit(commit)}
	}

	return mergeRequestCommits, startCommit, endCommit, nil
}

// fetchCommitFromRemote fetches a commit directly by SHA from the GitLab remote
// Equivalent to: git fetch origin <commit-sha>
func (p *project) fetchCommitFromRemote(ctx context.Context, commitID string) (*object.Commit, error) {
	commitHash := plumbing.NewHash(commitID)

	// First check if commit already exists
	commit, err := object.GetCommit(p.repo.Storer, commitHash)
	if err == nil {
		logger.Trace("  commit already exists in local repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "sha", commitID)
		return commit, nil
	}

	gitlabRemote, err := p.repo.Remote("gitlab")
	if err != nil {
		return nil, fmt.Errorf("getting gitlab remote: %v", err)
	}

	logger.Trace("  fetching commit directly by SHA from remote", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "sha", commitID)

	// Fetch the commit directly by SHA (GitLab supports this)
	refSpec := config.RefSpec(fmt.Sprintf("%s:refs/remotes/gitlab/%s", commitID, commitID))
	if err := gitlabRemote.FetchContext(ctx, &git.FetchOptions{
		RefSpecs: []config.RefSpec{refSpec},
		Depth:    0,
	}); err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil, fmt.Errorf("fetching commit %s directly from remote: %v", commitID, err)
	}

	// Verify the commit is now available
	commit, err = object.GetCommit(p.repo.Storer, commitHash)
	if err != nil {
		return nil, fmt.Errorf("commit %s still not found after fetch: %v", commitID, err)
	}

	logger.Trace("  successfully fetched commit from remote", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "sha", commitID)
	return commit, nil
}

func (p *project) createEmptyCommit() (*object.Commit, error) {
	// Resolve the default branch to get the parent commit
	targetHash, err := p.repo.ResolveRevision(plumbing.Revision(p.defaultBranch))
	if err != nil {
		return nil, fmt.Errorf("resolving default branch %s: %v", p.defaultBranch, err)
	}

	// Get the parent commit directly from the storer
	parentCommit, err := object.GetCommit(p.repo.Storer, *targetHash)
	if err != nil {
		return nil, fmt.Errorf("getting parent commit: %v", err)
	}

	// Create an empty commit by using the same tree as the parent
	now := time.Now()
	commitObj := &object.Commit{
		Author: object.Signature{
			Name:  "GitLab Migrator",
			Email: "gitlab-migrator",
			When:  now,
		},
		Committer: object.Signature{
			Name:  "GitLab Migrator",
			Email: "gitlab-migrator",
			When:  now,
		},
		Message:      "Empty commit for empty merge request",
		TreeHash:     parentCommit.TreeHash,
		ParentHashes: []plumbing.Hash{parentCommit.Hash},
	}

	// Encode and store the commit using the storer
	obj := p.repo.Storer.NewEncodedObject()
	if err := commitObj.Encode(obj); err != nil {
		return nil, fmt.Errorf("encoding commit: %v", err)
	}
	commitHash, err := p.repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return nil, fmt.Errorf("saving empty commit: %v", err)
	}

	// Get the commit object to extract metadata
	createdCommit, err := object.GetCommit(p.repo.Storer, commitHash)
	if err != nil {
		return nil, fmt.Errorf("retrieving created commit: %v", err)
	}

	logger.Debug("created empty commit", "sha", createdCommit.Hash)
	return createdCommit, nil
}

func commit2GitlabCommit(commit *object.Commit) *gitlab.Commit {
	return &gitlab.Commit{
		ID:            commit.Hash.String(),
		ShortID:       commit.Hash.String()[:8], // gitlab wants 8 as short hash
		Title:         commit.Message,
		AuthorName:    commit.Author.Name,
		AuthorEmail:   commit.Author.Email,
		AuthoredDate:  &commit.Author.When,
		CommittedDate: &commit.Author.When,
	}
}

// findHeadAndTailCommits analyzes a set of commits and identifies the head commit
// (not a parent of any other commit) and tail commit (parent not in the set).
// in a merge request, there should be only one head commit and one tail commit.
func findHeadAndTailCommits(commits []*object.Commit) (headCommit, tailCommit *object.Commit) {
	if len(commits) == 0 {
		return nil, nil
	}

	// Build maps and sets in a single pass
	commitHashes := make(map[plumbing.Hash]bool)
	parentHashes := make(map[plumbing.Hash]bool)

	for _, commit := range commits {
		commitHashes[commit.Hash] = true
		for _, parentHash := range commit.ParentHashes {
			parentHashes[parentHash] = true
		}
	}

	var headCommits []*object.Commit
	var tailCommits []*object.Commit
	// Find head and tail commits in a single pass
	for _, commit := range commits {
		// Check if this is a head commit (not a parent of any other commit)
		if !parentHashes[commit.Hash] {
			logger.Trace("found head commit", "hash", commit.Hash)
			headCommits = append(headCommits, commit)
		}

		// Check if this is a tail commit (parent(s) not in this set)
		isTail := true
		// if any parent is in the set, it's not a tail commit
		// this should be largely true - can reexamine later
		for _, parentHash := range commit.ParentHashes {
			if commitHashes[parentHash] {
				isTail = false
				break
			}
		}
		if isTail {
			tailCommits = append(tailCommits, commit)
			logger.Trace("found tail commit", "hash", commit.Hash)
		}
	}

	// Select head commit from collected head commits
	if len(headCommits) == 0 {
		headCommit = nil
	} else if len(headCommits) == 1 {
		headCommit = headCommits[0]
	} else {
		// Multiple head commits found, pick the one with the latest creation date
		logger.Warn("multiple head commits found", "count", len(headCommits), "commits", headCommits)

		// Find the commit with the latest Author.When (creation date)
		headCommit = headCommits[0]
		for _, c := range headCommits[1:] {
			if c.Author.When.After(headCommit.Author.When) {
				headCommit = c
			}
		}
		logger.Trace("selected latest head commit", "hash", headCommit.Hash, "author_date", headCommit.Author.When)
	}

	// Select tail commit from collected tail commits
	if len(tailCommits) == 0 {
		tailCommit = nil
	} else if len(tailCommits) == 1 {
		tailCommit = tailCommits[0]
	} else {
		// Multiple tail commits found, pick the one with the earliest creation date
		logger.Warn("multiple tail commits found", "count", len(tailCommits), "commits", tailCommits)

		// Find the commit with the earliest Author.When (creation date)
		tailCommit = tailCommits[0]
		for _, c := range tailCommits[1:] {
			if c.Author.When.Before(tailCommit.Author.When) {
				tailCommit = c
			}
		}
		logger.Trace("selected earliest tail commit", "hash", tailCommit.Hash, "author_date", tailCommit.Author.When)
	}

	return headCommit, tailCommit
}

func (p *project) migrateTextBody(textBody string, mergeRequestIID int) string {
	// Extract @mentions and backtick them.
	// This prevents GitLab @mentions being confused as GitHub @mentions.
	mentionRegex := regexp.MustCompile(`@([a-zA-Z0-9_-]+)`)
	textBody = mentionRegex.ReplaceAllStringFunc(textBody, func(match string) string {
		// extract the @mention
		submatches := mentionRegex.FindStringSubmatch(match)
		if len(submatches) == 2 {
			mention := submatches[1]
			backtickedMention := fmt.Sprintf("`@%s`", mention)
			logger.Debug("backticking @mention", "merge_request_id", mergeRequestIID, "mention", mention, "backticked_mention", backtickedMention)
			return backtickedMention
		}
		return match
	})

	// Extract #issue-number and backtick them.
	// This prevents GitLab issue references being confused as GitHub issue/pr references.
	issueNumberRegex := regexp.MustCompile(`#(\d+)`)
	textBody = issueNumberRegex.ReplaceAllStringFunc(textBody, func(match string) string {
		submatches := issueNumberRegex.FindStringSubmatch(match)
		if len(submatches) == 2 {
			issueNumber := submatches[1]
			backtickedIssueNumber := fmt.Sprintf("`#%s`", issueNumber)
			logger.Debug("backticking issue number", "merge_request_id", mergeRequestIID, "issue_number", issueNumber, "backticked_issue_number", backtickedIssueNumber)
			return backtickedIssueNumber
		}
		return match
	})

	// Convert markdown image links from /uploads/... to domain/uploads/...
	// Pattern: ![alt text](/uploads/path/to/file.png)
	imageLinkRegex := regexp.MustCompile(`!\[([^\]]*)\]\((/uploads/[^)]+)\)`)
	textBody = imageLinkRegex.ReplaceAllStringFunc(textBody, func(match string) string {
		// Extract the alt text and path
		submatches := imageLinkRegex.FindStringSubmatch(match)
		if len(submatches) == 3 {
			altText := submatches[1]
			uploadPath := submatches[2]
			// we don't display as image but just a link because the imageHostingDomain is private
			newLink := fmt.Sprintf("[%s](%s%s)", altText, imageHostingDomain, uploadPath)
			logger.Debug("converting image link", "merge_request_id", mergeRequestIID, "old", match, "new", newLink)
			return newLink
		}
		return match
	})

	return textBody
}

func generateGithubDiffLink(githubDomain, repoOwner, repoName string, position *gitlab.NotePosition) string {
	var filepath string
	var lineNumber int
	var lineType string // "L" for old line, "R" for new line

	if position.NewLine != 0 {
		filepath = position.NewPath
		lineNumber = position.NewLine
		lineType = "R"
	} else {
		filepath = position.OldPath
		lineNumber = position.OldLine
		lineType = "L"
	}

	hash := sha256.Sum256([]byte(filepath))
	diffHash := fmt.Sprintf("%x", hash)

	return fmt.Sprintf("https://%s/%s/%s/compare/%s...%s#diff-%s%s%d", githubDomain, repoOwner, repoName, position.BaseSHA, position.HeadSHA, diffHash, lineType, lineNumber)
}
