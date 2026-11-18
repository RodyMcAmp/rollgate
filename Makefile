# Entry points for humans, agents and CI. See AGENTS.md and docs/testing.md.
# image and test-vm need an Arch Linux host and root; see docs/dev-setup.md.

SHELL := /bin/bash
BUILD_DIR ?= build
SNAPSHOT_DATE := $(shell tr -d '[:space:]' < image/snapshot-date)
IMAGE := $(BUILD_DIR)/rollgate-$(SNAPSHOT_DATE).img
SUDO := $(if $(filter 0,$(shell id -u)),,sudo)

SHELL_SCRIPTS := image/build.sh tests/vm/boot-test.sh tests/vm/rollback-test.sh \
	tests/vm/apply-test.sh tests/vm/ipv6-test.sh tests/netns/run.sh \
	image/rootfs/usr/local/sbin/rollgate-snapshot-rollback \
	image/rootfs/usr/local/lib/rollgate/boot-report

.PHONY: check golden-update rollgate test-netns image test-vm test-apply test-ipv6 test-rollback clean

## L0 + L1: shell lint, Go format/vet/lint/tests, and the example config validates.
## Run before every commit.
check:
	shellcheck $(SHELL_SCRIPTS)
	@unformatted=$$(cd compiler && gofmt -l .); \
	  if [ -n "$$unformatted" ]; then echo "gofmt needed: $$unformatted"; exit 1; fi
	cd compiler && go vet ./... && staticcheck ./... && go test ./...
	cd compiler && go run ./cmd/rollgate validate ../examples/router.toml
	cd compiler && for f in ../tests/vm/apply/*.toml ../tests/vm/ipv6/*.toml; do go run ./cmd/rollgate validate "$$f" || exit 1; done
	cd compiler && for f in ../tests/netns/*.go ../tests/vm/*.go; do go vet "$$f" || exit 1; done

## Rewrite tests/golden/*/expected and schema/router.schema.json from the compiler.
## Review the diff before committing; never run this just to make a failing test pass.
golden-update:
	cd compiler && go test ./compile -run TestGolden -update && go test ./config -run TestSchemaFile -update

## Build the rollgate CLI as a static binary (ADR 0005).
rollgate:
	cd compiler && CGO_ENABLED=0 go build -trimpath -o ../$(BUILD_DIR)/rollgate ./cmd/rollgate

## L2: check the compiled example with the daemons' own checkers (as rollgate apply
## will), then load the generated firewall and sysctl settings into network
## namespaces and check real traffic against the policy (tests/netns/run.sh). Needs root.
test-netns: rollgate
	cd compiler && $(SUDO) env ROLLGATE_REAL_CHECKS=1 go test -count=1 -run 'TestPrecheckReal' -v ./apply/
	$(SUDO) ROLLGATE=$(BUILD_DIR)/rollgate bash tests/netns/run.sh

## Build the disk image at the date in image/snapshot-date.
image: rollgate
	$(SUDO) BUILD_DIR=$(BUILD_DIR) ROLLGATE=$(abspath $(BUILD_DIR)/rollgate) bash image/build.sh

## L3: boot the image in QEMU with both kernels.
test-vm:
	$(SUDO) LOG_DIR=$(BUILD_DIR)/test-logs bash tests/vm/boot-test.sh $(IMAGE)

## L3: rollgate apply in a VM. Confirm works only from a new connection, and a
## config that locks out SSH reverts by itself, by the timer and across a power cut.
test-apply:
	$(SUDO) LOG_DIR=$(BUILD_DIR)/test-logs bash tests/vm/apply-test.sh $(IMAGE)

## L3: ipv6 = "auto" in a VM, with a fake ISP and a LAN client: local IPv6 when the
## ISP has none, public IPv6 by prefix delegation once it does (ADR 0008).
test-ipv6:
	$(SUDO) LOG_DIR=$(BUILD_DIR)/test-logs bash tests/vm/ipv6-test.sh $(IMAGE)

## L3: rollback rehearsal following docs/runbooks/recovery.md.
test-rollback:
	$(SUDO) LOG_DIR=$(BUILD_DIR)/test-logs bash tests/vm/rollback-test.sh $(IMAGE)

clean:
	$(SUDO) rm -rf $(BUILD_DIR)
