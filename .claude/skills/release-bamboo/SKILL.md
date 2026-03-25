---
name: release-bamboo
description: Build, push Docker images and create GitHub release for Bamboo Tunnel. Use when the user says "release", "publish", "new version", or wants to push a new version of Bamboo Tunnel.
argument-hint: "[version, e.g. 0.0.2]"
---

# Bamboo Tunnel Release Process

Release Bamboo Tunnel: build Docker images, push to Docker Hub, create GitHub release.

## Step 1: Determine version

If version is provided as argument, use it. Otherwise ask the user.

Version format: `MAJOR.MINOR.PATCH` (semver, no `v` prefix in argument, add `v` for git tag).
- `0.x.x` -- pre-release / alpha / testing
- `1.0.0+` -- stable

## Step 2: Pre-flight checks

Run in parallel:
- `git status` -- must be clean (no uncommitted changes). If dirty, STOP and tell the user to commit first.
- `git log --oneline -5` -- show recent commits
- `docker info 2>&1 | grep Username` -- must be logged into Docker Hub
- `gh auth status` -- must be logged into GitHub CLI

If any check fails, STOP and tell the user what's wrong.

## Step 3: Build Docker images

Build both images from the repo root (`/home/user/Bamboo-Tunnel`):

```bash
docker build -t eduard256/bamboo-server:VERSION -t eduard256/bamboo-server:latest -f server/Dockerfile .
docker build -t eduard256/bamboo-client:VERSION -t eduard256/bamboo-client:latest -f client/Dockerfile .
```

If build fails, STOP and show the error.

## Step 4: Push Docker images

Push all 4 tags in parallel:

```bash
docker push eduard256/bamboo-server:VERSION &
docker push eduard256/bamboo-server:latest &
docker push eduard256/bamboo-client:VERSION &
docker push eduard256/bamboo-client:latest &
wait
```

## Step 5: Create git tag

```bash
git tag vVERSION
git push origin vVERSION
```

## Step 6: Create GitHub release

Use `gh release create` with the following template. Fill in the sections based on `git log` since the previous tag.

```bash
PREV_TAG=$(git tag --sort=-v:refname | head -2 | tail -1)
# If no previous tag, use initial commit
```

Release notes template (ALWAYS use this exact structure):

```
## Bamboo Tunnel vVERSION

### What's Changed
- bullet point for each meaningful change since last release
- group by: Added / Changed / Fixed / Removed

### Docker Images

` `` bash
# Server (home, behind censored internet)
docker pull eduard256/bamboo-server:VERSION

# Client (foreign VPS, unrestricted internet)
docker pull eduard256/bamboo-client:VERSION
` ``

### Quick Start

See [README](https://github.com/eduard256/Bamboo-Tunnel) for full setup instructions.

**Server (.env):**
` ``
BAMBOO_TOKEN=your_secret_token
BAMBOO_PORT=8080
` ``

**Client (.env):**
` ``
BAMBOO_SERVER_URL=https://your-domain.com
BAMBOO_TOKEN=your_secret_token
` ``

Both require ` --cap-add=NET_ADMIN --device=/dev/net/tun`.
```

IMPORTANT: Use `--notes` with HEREDOC, NOT `--notes-file`. Example:
```bash
gh release create vVERSION --title "vVERSION - Title" --notes "$(cat <<'NOTES'
release notes here
NOTES
)"
```

## Step 7: Verify

Run in parallel:
- `gh release view vVERSION` -- confirm release exists
- `docker manifest inspect eduard256/bamboo-server:VERSION` -- confirm image exists
- `docker manifest inspect eduard256/bamboo-client:VERSION` -- confirm image exists

## Step 8: Report

Tell the user:
- Release URL
- Docker image tags
- What was included

## Rules

- NEVER skip pre-flight checks
- NEVER create release if git is dirty
- NEVER use emoji in release notes
- Commit messages in release notes -- in English
- Chat with user -- in Russian
- If version 0.x.x, add note at the top: "**This is a pre-release. Not production-ready.**"
- Release title format: `vVERSION - Short Description` (ask user for description or derive from changes)
