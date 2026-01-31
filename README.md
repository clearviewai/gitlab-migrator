# GitLab to GitHub Repository Migration Tool

Originally forked from [manicminer/gitlab-migrator](https://github.com/manicminer/gitlab-migrator).

**This fork strives to maintain MR<>PR number equivalence, because practically, MR numbers are referenced in many places, e.g. merge commit descriptions, MR descriptions, and comments. Moreover, they are referenced across all communication channels in an organization, e.g. Slack, Linear, Notion, etc. Preserving MR<>PR equivalence is critical to preserving bi-directional links and institutional knowledge. This means every edge case needs to be handled, and if not, empty/placeholder PR is created to fill the PR number counter, (and counter only goes up). It also means we now fail fast in MR migration, instead of moving on, so the user can inspect the failure and resume accordingly.**

**We fetch comments through the GitLab [discussions](https://docs.gitlab.com/api/discussions/#list-project-merge-request-discussion-items) API in addition to the [notes](https://docs.gitlab.com/api/notes/#list-all-merge-request-notes) API to preserve the comment group structure to avoid interleaving of timelines. This also cuts down the display overhead. Notably, the diff is linked to GitHub's `/compare/`.**

**We detect and convert GitLab-uploaded images and rewrite them into links that correspond to a different GitHub repo that stores these images, either as raw objects or LFS objects. This is the most practical way to preserve these images in a private organization. You could also host an image server with some slight modification to the code.**

_Example migrated pull request_

![example migrated closed pull request](pr-example-closed.png)

**Notable points on top of the original repo:**

* create placeholder PRs for edge cases (empty MRs, orphaned commits, etc.) to maintain MR<>PR number equivalence
* fail-fast behavior by default - Now exits on first PR failure unless `-skip-invalid-merge-requests` is set
* fetch commits directly from remote by SHA - when commits are missing locally (e.g., squashed MRs), fetches them directly from GitLab remote
* smart head/tail commit detection - Algorithm to find head and tail commits even when MR has multiple heads or tails (picks latest head, earliest tail by author date)
* RFC3339 timestamp format - Changed from custom "Mon, 2 Jan 2006" to `time.RFC3339` for more precise timestamps
* fetch comments via both GitLab [discussions](https://docs.gitlab.com/api/discussions/#list-project-merge-request-discussion-items) API and [notes](https://docs.gitlab.com/api/notes/#list-all-merge-request-notes) API to preserve comment group structure and avoid interleaved timelines
* add GitHub `/compare/` links to diff notes (file + line number) for better navigation - uses `crypto/sha256` to generate the correct diff anchor hash that GitHub uses (`#diff-{hash}L{line}` or `R{line}`)
* separate the serial merge request body migration and concurrent merge request comments migration into two stages
* goroutine-powered concurrent comments migration with configurable concurrency (`-max-concurrency-for-comments`)
* transform GitLab-uploaded image links to point to a configurable GitHub image-hosting repo (`-images-repo-name`, `-images-repo-ref`)
* additional image downloader utility to concurrently download GitLab-uploaded images for self-hosting
* PR title includes author name - Format: "MR Title - Author Name" for easier scanning
* email links for authors/approvers/mentions - Users are displayed as [Name (@username)](mailto:email) when email is available
* find and display actual MR approvers from GitLab approval events
* show original merge/squash commit with GitHub link
* show MR commits in PR body with details and links because reconstructed commits carry midway-merged master/main commits
* backtick `@mention`s and `#issue-number`s to prevent accidental GitHub notifications/links
* links back to original GitLab MR and notes for cross-reference during the migration overlapping period
* additional CLI flags: `-only-migrate-pull-requests`, `-only-migrate-comments`, `-skip-migrating-comments`, `-skip-existing-closed-or-merged-merge-requests`, `-merge-requests-from-id`
* improved retry logic with better rate-limit handling (respects `X-Ratelimit-Reset`, `Retry-After` headers)

<br>

---

<br>

This tool can migrate projects from GitLab to repositories on GitHub. It currently supports:

* migrating the git repository with full history
* migrating merge requests and translating them into pull requests, including closed/merged ones
* renaming the `master` branch to `main` along the way

It does not support migrating issues, wikis or any other primitive at this time. PRs welcome! (Although please don't waste your time suggesting swathing changes by an LLM)

Both gitlab.com and GitLab self-hosted are supported, as well as github.com and GitHub Enterprise (latter untested).

## Building

This tool is best built from source, because you might have unique requirements with your own migration.

Migrator:
```bash
go build gitlab-migrator
./gitlab-migrator -h
```

Image downloader:
```bash
go build ./cmd/download-images
./download-images -h
```

Golang 1.23 was used, you may have luck with earlier releases.

## Usage

_Example Usage_

```bash
LOG_LEVEL=INFO ./gitlab-migrator -github-user=mytokenuser -gitlab-project=mygitlabuser/myproject -github-repo=mygithubuser/myrepo -migrate-pull-requests -skip-migrating-comments
LOG_LEVEL=INFO ./gitlab-migrator -github-user=mytokenuser -gitlab-project=mygitlabuser/myproject -github-repo=mygithubuser/myrepo -only-migrate-comments

# download images
LOG_LEVEL=INFO ./download-images -gitlab-project mygitlabuser/myproject
```

Written in Go, this is a cross-platform CLI utility that accepts the following runtime arguments:

```
Usage of ./gitlab-migrator:
  -delete-existing-repos
    	whether existing repositories should be deleted before migrating
  -github-domain string
    	specifies the GitHub domain to use (default "github.com")
  -github-repo string
    	the GitHub repository to migrate to
  -github-user string
    	specifies the GitHub user to use, who will author any migrated PRs (required)
  -gitlab-domain string
    	specifies the GitLab domain to use (default "gitlab.com")
  -gitlab-project string
    	the GitLab project to migrate
  -images-repo-name string
    	specifies the repository to use for GL images (default "gl-imgs")
  -images-repo-ref string
    	specifies the commit SHA to use for GL images (default "main")
  -loop
    	continue migrating until canceled
  -max-concurrency int
    	how many projects to migrate in parallel (default 4)
  -max-concurrency-for-comments int
    	how many merge request comments to migrate in parallel - used only when only-migrate-comments is set (default 2)
  -merge-requests-from-id int
    	optional merge request ID to start migrating from, inclusive
  -merge-requests-max-age int
    	optional maximum age in days of merge requests to migrate
  -migrate-pull-requests
    	whether pull requests should be migrated
  -only-migrate-comments
    	when true, will only migrate comments - this short-circuits much of the repo/MR migration logic, and uses goroutines for parallelization
  -only-migrate-pull-requests
    	when true, will only migrate pull requests - this short-circuits much of the repo migration logic - used only when only-migrate-comments is not set
  -projects-csv string
    	specifies the path to a CSV file describing projects to migrate (incompatible with -gitlab-project and -github-repo)
  -rename-master-to-main
    	rename master branch to main and update pull requests (incompatible with -rename-trunk-branch)
  -rename-trunk-branch string
    	specifies the new trunk branch name (incompatible with -rename-master-to-main)
  -report
    	report on primitives to be migrated instead of beginning migration
  -skip-existing-closed-or-merged-merge-requests
    	when true, will skip migrating closed/merged merge requests that already have corresponding closed pull requests on GitHub - used only when migrate-pull-requests or only-migrate-pull-requests is set, and only-migrate-comments is not set
  -skip-invalid-merge-requests
    	when true, will log and skip invalid merge requests instead of raising an error
  -skip-migrating-comments
    	when true, will skip migrating comments - used only when migrate-pull-requests or only-migrate-pull-requests is set, and only-migrate-comments is not set
  -trim-branches-on-github
    	when true, will delete any branches on GitHub that are no longer present in GitLab
  -version
    	output version information
```

## Authentication

For authentication, the `GITLAB_TOKEN` and `GITHUB_TOKEN` environment variables must be populated. You cannot specify tokens as command-line arguments.

Use the `-github-user` argument to specify the GitHub username for whom the authentication token was issued (mandatory). You can also specify this with the `GITHUB_USER` environment variable.

Specify the location of a self-hosted instance of GitLab with the `-gitlab-domain` argument, or a GitHub Enterprise instance with the `-github-domain` argument.

## Specify repositories

You can specify an individual GitLab project with the `-gitlab-project` argument, along with the target GitHub repository with the `-github-repo` argument.

Alternatively, you can supply the path to a CSV file with the `-projects-csv` argument, which should contain two columns:

```csv
gitlab-group/gitlab-project-name,github-org-or-user/github-repo-name
```

If the destination repository does not exist, this tool will attempt to create a private repository. If the destination repo already exists, it will be used unless you specify `-delete-existing-repos`

> [!WARNING]  
> To delete existing GitHub repos prior to migrating, pass the `-delete-existing-repos` argument. _This is potentially dangerous, you won't be asked for confirmation!_

## Pull requests

To enable migration of GitLab merge requests to GitHub pull requests (including closed/merged ones!), specify `-migrate-pull-requests`.

Whilst the git repository itself will be migrated verbatim, the pull requests are managed using the GitHub API and typically will be authored by the person supplying the authentication token. You are encouraged to create a bot account on GitHub for your organization, such as `org-bot` to perform the migration.

Each pull request, along with every comment, will be prepended with a Markdown table showing the original author and some other metadata that is useful to know.  This is also used to map pull requests and their comments to their counterparts in GitLab and enables the tool to be idempotent.

This tool also migrates merged/closed merge requests from your GitLab projects. It does this by reconstructing temporary branches in each repo, pushing them to GitHub, creating then closing the pull request, and lastly deleting the temporary branches. Once the tool has completed, you should not have any of these temporary branches in your repo - although GitHub will not garbage collect them immediately such that you can click the `Restore branch` button in any of these PRs.

Similarly, you can specify a maximum age for merge requests to migrate with the `-merge-requests-max-age` argument, which is useful for 'topping off' projects that are already migrated.

## Renaming the default/trunk branch

As a bonus, this tool can transparently rename the trunk branch on your GitHub repository - enable with the `-rename-trunk-branch` argument. This will also work for any open merge requests as they are translated to pull requests.

## Concurrency

By default, 4 workers will be spawned to migrate up to 4 projects in parallel. You can increase or decrease this with the `-max-concurrency` argument. Note that due to GitHub API rate-limiting, you may not experience any significant speed-up. See [GitHub API docs](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api) for details.

Specify `-loop` to continue migrating projects until canceled. This is useful for daemonizing the migration tool, or automatically restarting when migrating a large number of projects (or a small number of very large projects).

Within a single repo, migrating MRs is serial, due to the need to maintain MR<>PR number equivalence. However, one could skip migrating comments during the serial pass, and then use `-max-concurrency-for-comments` to migrate comments concurrently over different MRs.

## Logging

This tool is entirely noninteractive and outputs different levels of logs depending on your interest. You can set the `LOG_LEVEL` environment to one of `ERROR`, `WARN`, `INFO`, `DEBUG` or `TRACE` to get more or less verbosity. The default is `INFO`.

## Caching

The tool maintains a thread-safe in-memory cache for certain primitives, in order to help reduce the number of API requests being made. At this time, the following are cached the first time they are encountered, and thereafter retrieved from the cache until the tool is restarted:

- GitHub pull requests
- GitHub issue search results
- GitHub user profiles
- GitLab user profiles

## Idempotence

This tool tries to be idempotent. You can run it over and over and it will patch the GitHub repository, along with its pull requests, to match what you have in GitLab. This should help you migrate a number of projects without enacting a large maintenance window.

_Note that this tool performs a forced mirror push, so it's not recommended to run this tool after commencing work in the target repository._

For pull requests and their comments, the corresponding IDs from GitLab are added to the Markdown header, this is parsed to enable idempotence (see next section).

## Contributing, reporting bugs etc...

Please use GitHub issues & pull requests. This project is licensed under the MIT license.
