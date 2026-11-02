# PoC 빌드 · 묶음.
#   make agent            OCB 로 에이전트(cmp-otelcol)를 정적 바이너리로 빌드 (agent/_build/cmp-otelcol, cmp-otelcol.exe)
#                         와 NVML GPM 보조 바이너리 cmp-gpm (Linux, glibc 동적. purego 가 libc 에 링크된다)
#   make agent-package    dist/cmp-otelcol-<버전>-linux-amd64.tar.gz · dist/cmp-otelcol-<버전>-windows-amd64.zip
#   make test             agent/ 아래 Go 모듈마다 go test
#   make central-bundle SRC=<Podman 원본 묶음 폴더>   중앙 노드 묶음 (dist/podman-el10, dist/images)
#   make clean            에이전트 빌드 결과와 묶음을 지운다 (중앙 묶음 dist/podman-el10 · dist/images 는 남긴다)
# 버전은 agent/builder-config.yaml 의 dist.version 이다.
#
# Go 버전: accelerator 모듈이 go 1.27.2 를 요구한다 (furiosa-smi-go v1.0.0). 설치된 go 가 더 낮으면 GOTOOLCHAIN=auto(기본)가
# go1.27.2 툴체인을 내려받아 쓴다. 인터넷이 없는 빌드 기계에는 Go 1.27.2 이상을 미리 깔거나, 툴체인 모듈을
# GOPROXY(사내 프록시 · 파일 file://...)로 준다. GOTOOLCHAIN=local 이면 낮은 go 로는 "requires go >= 1.27.2" 로 멈춘다.
SHELL := /bin/bash
.SHELLFLAGS := -euo pipefail -c

OCB_VERSION ?= v0.164.0
VERSION := $(shell awk '$$1 == "version:" {print $$2; exit}' agent/builder-config.yaml)
TOOLS := $(CURDIR)/agent/_tools
BUILD := agent/_build
LINUX_PKG := cmp-otelcol-$(VERSION)-linux-amd64
WINDOWS_PKG := cmp-otelcol-$(VERSION)-windows-amd64
STAGE := dist/.stage

.PHONY: agent agent-package test central-bundle clean

$(TOOLS)/builder:
	mkdir -p $(TOOLS)
	GOBIN=$(TOOLS) go install go.opentelemetry.io/collector/cmd/builder@$(OCB_VERSION)

# OCB 는 빌드하는 기계의 OS 용으로만 컴파일하므로, Linux 판을 만든 뒤 생성된 소스로 Windows 판을 따로 컴파일한다
# 본체는 정적이어야 한다 (glibc 판이 다른 서버에서도 돌게). 동적으로 링크되면 멈춘다
agent: $(TOOLS)/builder
	cd agent && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(TOOLS)/builder --config builder-config.yaml
	cd $(BUILD) && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o cmp-otelcol.exe .
	cd agent/accelerator && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o ../_build/cmp-gpm ./cmd/cmp-gpm
	file $(BUILD)/cmp-otelcol $(BUILD)/cmp-otelcol.exe $(BUILD)/cmp-gpm
	file $(BUILD)/cmp-otelcol | grep -q 'statically linked' || { echo "cmp-otelcol 이 정적 바이너리가 아니다" >&2; exit 1; }

agent-package: agent
	rm -rf $(STAGE) dist/$(LINUX_PKG).tar.gz dist/$(WINDOWS_PKG).zip
	mkdir -p $(STAGE)/$(LINUX_PKG)/bin $(STAGE)/$(LINUX_PKG)/systemd $(STAGE)/$(WINDOWS_PKG)
	install -m 0755 $(BUILD)/cmp-otelcol $(BUILD)/cmp-gpm $(STAGE)/$(LINUX_PKG)/bin/
	install -m 0644 agent/systemd/cmp-otelcol.service $(STAGE)/$(LINUX_PKG)/systemd/
	install -m 0755 agent/install.sh agent/uninstall.sh $(STAGE)/$(LINUX_PKG)/
	install -m 0644 agent/config.yaml.tmpl agent/THIRD_PARTY_NOTICES.md ../LICENSE $(STAGE)/$(LINUX_PKG)/
	install -m 0644 $(BUILD)/cmp-otelcol.exe agent/windows/install.ps1 agent/windows/uninstall.ps1 \
	  agent/windows/config.yaml.tmpl agent/THIRD_PARTY_NOTICES.md ../LICENSE $(STAGE)/$(WINDOWS_PKG)/
	tar czf dist/$(LINUX_PKG).tar.gz --owner=0 --group=0 -C $(STAGE) $(LINUX_PKG)
	cd $(STAGE) && zip -qr ../$(WINDOWS_PKG).zip $(WINDOWS_PKG)
	rm -rf $(STAGE)
	ls -l dist/$(LINUX_PKG).tar.gz dist/$(WINDOWS_PKG).zip

# 부품마다 자기 모듈이라 모듈 폴더마다 따로 돌린다. 실패한 모듈은 끝에 모아 알린다
test:
	failed=""; \
	for mod in $$(find agent -name go.mod -not -path 'agent/_build/*' -not -path 'agent/_tools/*' | sort); do \
	  dir=$${mod%/go.mod}; echo "=== $$dir"; \
	  (cd "$$dir" && go test ./...) || failed="$$failed $$dir"; \
	done; \
	if [ -n "$$failed" ]; then echo "실패:$$failed" >&2; exit 1; fi

central-bundle:
	@[ -n "$(SRC)" ] || { echo "SRC=<Podman 원본 묶음 폴더> 를 준다" >&2; exit 1; }
	bin/fetch-podman.sh "$(SRC)"
	bin/fetch-images.sh

clean:
	rm -rf $(BUILD) $(STAGE)
	rm -f dist/cmp-otelcol-*-linux-amd64.tar.gz dist/cmp-otelcol-*-windows-amd64.zip
