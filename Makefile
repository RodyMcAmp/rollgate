# Entry points for humans, agents and CI. See AGENTS.md and docs/testing.md.
# image and test-vm need an Arch Linux host and root; see docs/dev-setup.md.

SHELL := /bin/bash
BUILD_DIR ?= build
SNAPSHOT_DATE := $(shell tr -d '[:space:]' < image/snapshot-date)
IMAGE := $(BUILD_DIR)/rollgate-$(SNAPSHOT_DATE).img
SUDO := $(if $(filter 0,$(shell id -u)),,sudo)

SHELL_SCRIPTS := image/build.sh tests/vm/boot-test.sh tests/vm/rollback-test.sh \
	image/rootfs/usr/local/sbin/rollgate-snapshot-rollback \
	image/rootfs/usr/local/lib/rollgate/boot-report

.PHONY: check image test-vm test-rollback clean

## L0: shell lint and example config checks. Run before every commit.
check:
	shellcheck $(SHELL_SCRIPTS)
	python3 tests/check_examples.py

## Build the disk image at the date in image/snapshot-date.
image:
	$(SUDO) BUILD_DIR=$(BUILD_DIR) bash image/build.sh

## L3: boot the image in QEMU with both kernels.
test-vm:
	$(SUDO) LOG_DIR=$(BUILD_DIR)/test-logs bash tests/vm/boot-test.sh $(IMAGE)

## L3: rollback rehearsal following docs/runbooks/recovery.md.
test-rollback:
	$(SUDO) LOG_DIR=$(BUILD_DIR)/test-logs bash tests/vm/rollback-test.sh $(IMAGE)

clean:
	$(SUDO) rm -rf $(BUILD_DIR)
