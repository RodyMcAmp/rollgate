# Entry points for humans, agents and CI. See AGENTS.md and docs/testing.md.
# image and test-vm need an Arch Linux host and root; see docs/dev-setup.md.

SHELL := /bin/bash
BUILD_DIR ?= build
SNAPSHOT_DATE := $(shell tr -d '[:space:]' < image/snapshot-date)
IMAGE := $(BUILD_DIR)/rollgate-$(SNAPSHOT_DATE).img
# The L4 update test's "previous release": a week before the candidate, until the
# release train publishes real releases (M4 step 7).
PREVIOUS_DATE := $(shell date -u -d "$(SNAPSHOT_DATE) -7 days" +%F)
PREVIOUS_IMAGE := $(BUILD_DIR)/rollgate-$(PREVIOUS_DATE).img
SUDO := $(if $(filter 0,$(shell id -u)),,sudo)

SHELL_SCRIPTS := image/build.sh tests/vm/boot-test.sh tests/vm/rollback-test.sh \
	tests/vm/apply-test.sh tests/vm/ipv6-test.sh tests/vm/trial-test.sh \
	tests/vm/update-test.sh tests/netns/run.sh release/publish.sh release/dry-run.sh \
	release/bump.sh packaging/build.sh image/vmware.sh tests/vm/vmware-test.sh tests/vm/api-test.sh tests/vm/web-test.sh \
	tests/vm/clock-test.sh \
	image/rootfs/usr/local/sbin/rollgate-snapshot-rollback \
	image/rootfs/usr/local/lib/rollgate/boot-report

.PHONY: check golden-update rollgate rollgate-release release-dry-run package test-netns \
	image image-previous vmware test-vmware test-api test-web test-clock test-vm test-apply test-ipv6 test-trial test-rollback test-update clean

## L0 + L1: shell lint, Go format/vet/lint/tests, and the example config validates.
## Run before every commit.
check: rollgate rollgate-release
	shellcheck $(SHELL_SCRIPTS)
	@unformatted=$$(cd compiler && gofmt -l .); \
	  if [ -n "$$unformatted" ]; then echo "gofmt needed: $$unformatted"; exit 1; fi
	cd compiler && go vet ./... && staticcheck ./... && go test ./...
	cd compiler && go run ./cmd/rollgate validate ../examples/router.toml
	cd compiler && for f in ../tests/vm/apply/*.toml ../tests/vm/ipv6/*.toml ../image/vmware/router.toml; do go run ./cmd/rollgate validate "$$f" || exit 1; done
	cd compiler && for f in ../tests/netns/*.go ../tests/vm/*.go; do go vet "$$f" || exit 1; done
	bash release/dry-run.sh

## Rewrite tests/golden/*/expected and schema/router.schema.json from the compiler.
## Review the diff before committing; never run this just to make a failing test pass.
golden-update:
	cd compiler && go test ./compile -run TestGolden -update && go test ./config -run TestSchemaFile -update

## The release train's releases.json tool (ADR 0012); CI only, never on a router.
rollgate-release:
	cd compiler && CGO_ENABLED=0 go build -trimpath -o ../$(BUILD_DIR)/rollgate-release ./cmd/rollgate-release

## Rehearse publishing a release with a throwaway key (also part of check).
release-dry-run: rollgate rollgate-release
	bash release/dry-run.sh

## Build the rollgate package (packaging/rollgate) for image/snapshot-date into build/pkg.
package: rollgate
	bash packaging/build.sh $(SNAPSHOT_DATE) $(BUILD_DIR)/rollgate $(BUILD_DIR)/pkg

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

## Turn the image into a VMware Workstation VM (docs/vmware.md). Needs SSH_KEY, your
## SSH public key file; VMWARE_DIR is where the VM's folder goes.
VMWARE_DIR ?= $(BUILD_DIR)/vmware
vmware:
	$(SUDO) SSH_KEY="$(SSH_KEY)" bash image/vmware.sh $(IMAGE) "$(VMWARE_DIR)"

## Build the L4 update test's "previous release" image, a week before image/snapshot-date.
image-previous: rollgate
	$(SUDO) BUILD_DIR=$(BUILD_DIR) IMAGE_DATE=$(PREVIOUS_DATE) ROLLGATE=$(abspath $(BUILD_DIR)/rollgate) bash image/build.sh

## L4: install the previous release, apply a config, and update it to this release:
## a trial that fails its checks and one that panics both roll back by themselves; one
## that passes becomes the system, with the config intact and both kernels booting.
## Needs make image-previous and internet access (the Arch Linux Archive).
test-update: rollgate rollgate-release
	$(SUDO) LOG_DIR=$(BUILD_DIR)/test-logs bash tests/vm/update-test.sh $(PREVIOUS_IMAGE) $(SNAPSHOT_DATE)

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

## L3: rollgate update prepares @next and arms exactly one trial boot of it (ADR 0011).
## Needs internet access (the Arch Linux Archive).
test-trial:
	$(SUDO) LOG_DIR=$(BUILD_DIR)/test-logs bash tests/vm/trial-test.sh $(IMAGE)

## L3: make vmware's VM boots on VMware's virtual hardware (NVMe, vmxnet3, emulated by
## QEMU) and its router.toml applies and passes the health checks (docs/vmware.md).
test-vmware:
	$(SUDO) LOG_DIR=$(BUILD_DIR)/test-logs bash tests/vm/vmware-test.sh $(IMAGE)

## L3: the local API the web UI uses (ADR 0014), over its socket in a VM: off by
## default, only root and rollgate-api may connect, apply and confirm work, and a
## confirm from the applying connection is refused.
test-api:
	$(SUDO) LOG_DIR=$(BUILD_DIR)/test-logs bash tests/vm/api-test.sh $(IMAGE)

## L3: the web UI (ADR 0014) in a VM, as a browser sees it: off until a password is
## set, TLS with the printed fingerprint, login and session cookie, the status
## page, HTTP redirects, unreachable from WAN, and a new password ends sessions.
test-web:
	$(SUDO) LOG_DIR=$(BUILD_DIR)/test-logs bash tests/vm/web-test.sh $(IMAGE)

## L3: the clock synchronizes, and without DNS, with the clock 9 hours back, from
## Cloudflare's time service by IP address (ADR 0013, 0015). Needs internet.
test-clock:
	$(SUDO) LOG_DIR=$(BUILD_DIR)/test-logs bash tests/vm/clock-test.sh $(IMAGE)

## L3: rollback rehearsal following docs/runbooks/recovery.md.
test-rollback:
	$(SUDO) LOG_DIR=$(BUILD_DIR)/test-logs bash tests/vm/rollback-test.sh $(IMAGE)

clean:
	$(SUDO) rm -rf $(BUILD_DIR)
