# Cross-build keenetic-tools on a workstation (Linux, macOS, WSL) and deploy to
# a Keenetic router over SSH. Nothing is compiled on the router.
#
#   make build ARCH=mipsel            one architecture -> dist/$(CMD)-$(ARCH)
#   make all                          every architecture
#   make deploy ROUTER=root@192.168.1.1 [ARGS='...']
#                                     detect router arch, build, upload, run
#   make test                         go vet + go test
#   make verify TAG=v1.0.0            rebuild a release tag, compare with its SHA256SUMS
#   make clean
#
# Entware SSH is often on port 222 when the Keenetic CLI keeps 22: pass SSH_PORT=222.

CMD      ?= keenetic-tools
# Names follow Entware (opkg print-architecture) without the "sf" suffix.
ARCHES   := mipsel mips aarch64 armv7
ARCH     ?=
ROUTER   ?=
SSH_PORT ?= 22
ARGS     ?= version
DIST     ?= dist
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# The exact toolchain from go.mod (downloaded by the go command if needed):
# another Go version gives different bytes.
export GOTOOLCHAIN := $(shell sed -n 's/^toolchain //p' go.mod)

# Must match .goreleaser.yaml so release builds can be reproduced.
BUILD_FLAGS := -trimpath -buildvcs=false
LDFLAGS     := -s -w -buildid= -X main.version=$(VERSION)

# One SSH connection for all steps: a single password prompt without keys.
SSH := ssh -p $(SSH_PORT) -o ControlMaster=auto -o ControlPath=/tmp/kt-ssh-%C -o ControlPersist=60

REPO     ?= asiforis/keenetic-tools

.PHONY: build all deploy test verify clean

build:
	@test -n "$(ARCH)" || { echo "Укажите ARCH: $(ARCHES)" >&2; exit 1; }
	@set -e; \
	arch=$$(echo "$(ARCH)" | sed 's/sf$$//'); \
	case "$$arch" in \
	    mipsel) goenv="GOARCH=mipsle GOMIPS=softfloat" ;; \
	    mips) goenv="GOARCH=mips GOMIPS=softfloat" ;; \
	    aarch64) goenv="GOARCH=arm64" ;; \
	    armv7) goenv="GOARCH=arm GOARM=5" ;; \
	    *) echo "Неизвестная архитектура '$(ARCH)'. Поддерживаются: $(ARCHES)" >&2; exit 1 ;; \
	esac; \
	mkdir -p $(DIST); \
	env CGO_ENABLED=0 GOOS=linux $$goenv go build $(BUILD_FLAGS) -ldflags '$(LDFLAGS)' \
	    -o $(DIST)/$(CMD)-$$arch ./cmd/$(CMD); \
	echo "$(DIST)/$(CMD)-$$arch ($$(wc -c < $(DIST)/$(CMD)-$$arch) байт)"

all:
	@for a in $(ARCHES); do $(MAKE) --no-print-directory build ARCH=$$a || exit 1; done

deploy:
	@test -n "$(ROUTER)" || { echo 'Укажите ROUTER=root@АДРЕС_РОУТЕРА' >&2; exit 1; }
	@set -e; \
	out=$$($(SSH) $(ROUTER) /opt/bin/opkg print-architecture); \
	arch=$$(printf '%s\n' "$$out" | sed -n 's/^arch \([a-z0-9]*\)-k\{0,1\}[0-9].*/\1/p' | sed 's/sf$$//' | head -n 1); \
	test -n "$$arch" || { printf 'Не удалось определить архитектуру роутера:\n%s\n' "$$out" >&2; exit 1; }; \
	echo "Архитектура роутера: $$arch"; \
	$(MAKE) --no-print-directory build ARCH=$$arch; \
	$(SSH) $(ROUTER) 'cat > /opt/$(CMD).part && chmod 755 /opt/$(CMD).part' < $(DIST)/$(CMD)-$$arch; \
	echo "Запуск на роутере: /opt/$(CMD).part $(ARGS)"; \
	$(SSH) $(ROUTER) '/opt/$(CMD).part $(ARGS); s=$$?; rm -f /opt/$(CMD).part; exit $$s'

test:
	go vet ./...
	go test ./...

# Builds the tag in a temporary worktree (only committed files), then checks
# the hashes against the SHA256SUMS published with the release.
verify:
	@test -n "$(TAG)" || { echo 'Укажите TAG=vX.Y.Z' >&2; exit 1; }
	@set -e; \
	tmp=$$(mktemp -d); \
	trap 'git worktree remove --force "$$tmp/src" >/dev/null 2>&1 || :; rm -rf "$$tmp"' EXIT; \
	git worktree add --detach "$$tmp/src" "$(TAG)" >/dev/null 2>&1; \
	$(MAKE) --no-print-directory -C "$$tmp/src" all VERSION=$(TAG) DIST="$$tmp/dist"; \
	curl -fsSL "https://github.com/$(REPO)/releases/download/$(TAG)/SHA256SUMS" -o "$$tmp/SHA256SUMS"; \
	cd "$$tmp/dist"; \
	if command -v sha256sum >/dev/null 2>&1; then sha256sum -c "$$tmp/SHA256SUMS"; \
	else shasum -a 256 -c "$$tmp/SHA256SUMS"; fi; \
	echo "$(TAG): сборка из исходников совпадает с релизом"

clean:
	rm -rf $(DIST)
