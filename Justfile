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

[group("dev")]
[doc("Run tests")]
test:
    go test ./...  -cover

[group("dev")]
[doc("Run all quality checks")]
check:
    just fmt
    just vet
    just test
