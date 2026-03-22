# Version format validation regex
VERSION_REGEX := '^v[0-9]+\.[0-9]+\.[0-9]+(-rc[0-9]+)?$'

# Default shell: powershell on Windows, sh on others
# Powershell must support simple commands
# Note: Windows shell should be an array with explicit args
set windows-shell := ["powershell.exe", "-NoLogo", "-Command"]
set shell := ["sh", "-c"]

# ========================
# Release / Version Tasks
# ========================

# Validate version format
[windows]
[doc("Validate version format")]
validate VERSION:
    if (! ("{{VERSION}}" -match "{{VERSION_REGEX}}")) { exit 1 }
    Write-Host "OK: Version format {{VERSION}}"

[unix]
[doc("Validate version format")]
validate VERSION:
    echo "{{VERSION}}" | grep -Eq {{VERSION_REGEX}}

# Create & push a tag
# Example: just release v1.0.0-rc1
[doc("Tag a release")]
release VERSION:
    just validate {{VERSION}}
    git tag {{VERSION}}
    git push origin {{VERSION}}

# ========================
# Build Tasks (CI + Local)
# ========================

[windows]
[group("build")]
[doc("Compile both binaries")]
build:
    $VERSION = (git describe --tags --always --dirty).Trim(); go build -ldflags "-X main.Version=$VERSION" -o bin/service.exe ./cmd/service; go build -ldflags "-X main.Version=$VERSION" -o bin/cli.exe ./cmd/cli

[unix]
[group("build")]
[doc("Compile both binaries")]
build:
    # Single shell invocation so VERSION is available to all commands
    VERSION=$$(git describe --tags --always --dirty)
    go build -ldflags "-X main.Version=$$VERSION" -o bin/service ./cmd/service
    go build -ldflags "-X main.Version=$$VERSION" -o bin/cli ./cmd/cli

[windows]
[group("build")]
[doc("Build Docker image for service")]
docker:
    $VERSION = git describe --tags --always --dirty; docker build --build-arg VERSION=$VERSION -t myproject:$VERSION .

[unix]
[group("build")]
[doc("Build Docker image for service")]
docker:
    VERSION=$$(git describe --tags --always --dirty); docker build --build-arg VERSION=$$VERSION -t myproject:$$VERSION .

# ========================
# Code Quality (Dev)
# ========================

[group("dev")]
[doc("Format code")]
fmt:
    go fmt ./...

[group("dev")]
[doc("Vet code")]
vet:
    go vet ./...

[unix]
[group("dev")]
[doc("Run unit tests (excludes e2e — no Docker required)")]
test:
    go test -cover $(go list ./... | grep -v 'hooklet/test/e2e')

[windows]
[group("dev")]
[doc("Run unit tests (excludes e2e — no Docker required)")]
test:
    go test -cover (go list ./... | Where-Object { $_ -notmatch 'hooklet/test/e2e' })

[group("dev")]
[doc("Run smoke e2e suite (fast, ~seconds, requires Docker)")]
test-smoke:
    go test -v -count=1 -timeout 3m ./test/e2e/smoke/...

[group("dev")]
[doc("Run heavy e2e suite (slow, requires Docker)")]
test-heavy:
    go test -v -count=1 -timeout 15m ./test/e2e/heavy/...

[group("dev")]
[doc("Run all quality checks (unit tests only)")]
check:
    just fmt
    just vet
    just test
