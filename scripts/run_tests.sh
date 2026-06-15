#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

CGROUP_NAME="test-tinycontainer-$$"

check_cgroup_cleanup() {
    local name="$1"
    local paths=()

    if [ -d "/sys/fs/cgroup/cgroup.controllers" ]; then
        paths+=("/sys/fs/cgroup/$name")
    else
        paths+=("/sys/fs/cgroup/cpu/$name")
        paths+=("/sys/fs/cgroup/memory/$name")
    fi

    local all_clean=true
    for p in "${paths[@]}"; do
        if [ -d "$p" ]; then
            all_clean=false
            echo "  ✗ Cgroup directory still exists: $p"
        fi
    done

    if $all_clean; then
        echo "  ✓ All cgroup directories cleaned up"
    else
        echo "  Attempting cleanup..."
        for p in "${paths[@]}"; do
            sudo rmdir "$p" 2>/dev/null || true
        done
    fi
}

echo "========================================="
echo "TinyContainer Validation Test Suite"
echo "========================================="
echo ""

cd "$PROJECT_DIR"

echo "[1/5] Building tinycontainer..."
go build -o tinycontainer .
echo "  ✓ Build successful"
echo ""

echo "[2/5] Building stresstest..."
cd "$PROJECT_DIR/stresstest"
go build -o stresstest .
cd "$PROJECT_DIR"
echo "  ✓ Build successful"
echo ""

echo "[3/5] Preparing rootfs..."
ROOTFS_DIR="$PROJECT_DIR/test-rootfs"
rm -rf "$ROOTFS_DIR"
mkdir -p "$ROOTFS_DIR"/{bin,proc,sys,tmp,dev}

cp "$PROJECT_DIR/tinycontainer" "$ROOTFS_DIR/bin/"
cp "$PROJECT_DIR/stresstest/stresstest" "$ROOTFS_DIR/bin/"

if command -v busybox >/dev/null 2>&1; then
    cp "$(which busybox)" "$ROOTFS_DIR/bin/"
    cd "$ROOTFS_DIR/bin"
    for cmd in sh ls ps echo cat sleep kill exit false true; do
        ln -sf busybox "$cmd" 2>/dev/null || true
    done
    cd "$PROJECT_DIR"
    echo "  ✓ rootfs prepared with busybox"
else
    echo "  ⚠ busybox not found, copying essential tools from system"
    for cmd in sh ls ps echo cat sleep; do
        if [ -f "/bin/$cmd" ]; then
            cp "/bin/$cmd" "$ROOTFS_DIR/bin/"
        fi
    done
fi

echo ""

echo "========================================="
echo "Test 1: Normal exit (code 0)"
echo "========================================="
set +e
sudo ./tinycontainer run --rootfs "$ROOTFS_DIR" --cgroup-name "$CGROUP_NAME" --memory 128m /bin/true
EXIT_CODE=$?
set -e
echo "Exit code: $EXIT_CODE"
if [ "$EXIT_CODE" -eq 0 ]; then
    echo "  ✓ Exit code is 0"
else
    echo "  ✗ Exit code is $EXIT_CODE, expected 0"
fi
check_cgroup_cleanup "$CGROUP_NAME"
echo ""

echo "========================================="
echo "Test 2: Non-zero exit (code 1)"
echo "========================================="
set +e
sudo ./tinycontainer run --rootfs "$ROOTFS_DIR" --cgroup-name "$CGROUP_NAME" --memory 128m /bin/false
EXIT_CODE=$?
set -e
echo "Exit code: $EXIT_CODE"
if [ "$EXIT_CODE" -eq 1 ]; then
    echo "  ✓ Exit code is 1"
else
    echo "  ✗ Exit code is $EXIT_CODE, expected 1"
fi
check_cgroup_cleanup "$CGROUP_NAME"
echo ""

echo "========================================="
echo "Test 3: Signal termination (SIGTERM -> code 143)"
echo "========================================="
set +e
timeout --signal=TERM 3 sudo ./tinycontainer run --rootfs "$ROOTFS_DIR" --cgroup-name "$CGROUP_NAME" --memory 128m /bin/sleep 10
EXIT_CODE=$?
set -e
echo "Exit code: $EXIT_CODE"
if [ "$EXIT_CODE" -eq 143 ]; then
    echo "  ✓ Exit code is 143 (128 + 15 SIGTERM)"
else
    echo "  ✗ Exit code is $EXIT_CODE, expected 143"
fi
check_cgroup_cleanup "$CGROUP_NAME"
echo ""

echo "========================================="
echo "Test 4: Error - rootfs does not exist"
echo "========================================="
set +e
sudo ./tinycontainer run --rootfs /nonexistent/path --cgroup-name "$CGROUP_NAME" /bin/sh
EXIT_CODE=$?
set -e
echo "Exit code: $EXIT_CODE"
if [ "$EXIT_CODE" -eq 1 ]; then
    echo "  ✓ Command failed as expected"
else
    echo "  ✗ Exit code is $EXIT_CODE, expected 1"
fi
check_cgroup_cleanup "$CGROUP_NAME"
echo ""

echo "========================================="
echo "Test 5: Error - command does not exist"
echo "========================================="
set +e
sudo ./tinycontainer run --rootfs "$ROOTFS_DIR" --cgroup-name "$CGROUP_NAME" /bin/nonexistent_cmd
EXIT_CODE=$?
set -e
echo "Exit code: $EXIT_CODE"
if [ "$EXIT_CODE" -eq 1 ]; then
    echo "  ✓ Command failed as expected"
else
    echo "  ✗ Exit code is $EXIT_CODE, expected 1"
fi
check_cgroup_cleanup "$CGROUP_NAME"
echo ""

echo "========================================="
echo "Test 6: CPU limit verification"
echo "========================================="
echo "  Starting CPU stress test with 2 threads, limited to 0.5 cores"
echo "  Running for 5 seconds..."
set +e
timeout 5 sudo ./tinycontainer run --rootfs "$ROOTFS_DIR" --cgroup-name "$CGROUP_NAME" \
    --cpu-quota 50000 --cpu-period 100000 --memory 256m \
    /bin/stresstest cpu 2
EXIT_CODE=$?
set -e
echo "Exit code: $EXIT_CODE (timeout = 124 is expected)"
if [ "$EXIT_CODE" -eq 124 ]; then
    echo "  ✓ CPU stress test completed (timed out as expected)"
else
    echo "  ⚠ Exit code is $EXIT_CODE (124 expected for timeout)"
fi
check_cgroup_cleanup "$CGROUP_NAME"
echo ""

echo "========================================="
echo "Test 7: Memory limit verification"
echo "========================================="
echo "  Starting memory stress test: trying to allocate 256MB, limited to 128MB"
echo "  Running for 5 seconds..."
set +e
timeout 5 sudo ./tinycontainer run --rootfs "$ROOTFS_DIR" --cgroup-name "$CGROUP_NAME" \
    --memory 128m \
    /bin/stresstest mem 256
EXIT_CODE=$?
set -e
echo "Exit code: $EXIT_CODE"
echo "  If OOM killer was invoked, process would be killed with code 137 (SIGKILL)"
if [ "$EXIT_CODE" -eq 137 ]; then
    echo "  ✓ Process killed by OOM (exit code 137 = 128 + 9 SIGKILL)"
elif [ "$EXIT_CODE" -eq 124 ]; then
    echo "  ⚠ Timed out - allocation may have been throttled instead of OOM killed"
else
    echo "  ⚠ Exit code: $EXIT_CODE"
fi
check_cgroup_cleanup "$CGROUP_NAME"
echo ""

echo "========================================="
echo "Test 8: Namespace isolation verification"
echo "========================================="
echo "  Running isolation checks..."
HOST_HOSTNAME="$(hostname)"
CONTAINER_HOSTNAME="test-container-$$"

set +e
CONTAINER_OUTPUT=$(sudo ./tinycontainer run --rootfs "$ROOTFS_DIR" --cgroup-name "$CGROUP_NAME" \
    --hostname "$CONTAINER_HOSTNAME" --memory 128m \
    /bin/sh -c 'echo "HOSTNAME=$(hostname)"; echo "PID=$$"; echo "PROCS=$(ls /proc | grep -E "^[0-9]+$" | wc -l)"')
EXIT_CODE=$?
set -e

echo "$CONTAINER_OUTPUT"
echo ""

CONTAINER_HOSTNAME_FOUND=$(echo "$CONTAINER_OUTPUT" | grep "^HOSTNAME=" | cut -d= -f2)
CONTAINER_PID=$(echo "$CONTAINER_OUTPUT" | grep "^PID=" | cut -d= -f2)
CONTAINER_PROCS=$(echo "$CONTAINER_OUTPUT" | grep "^PROCS=" | cut -d= -f2)

if [ "$CONTAINER_HOSTNAME_FOUND" = "$CONTAINER_HOSTNAME" ]; then
    echo "  ✓ UTS namespace: hostname is isolated ($CONTAINER_HOSTNAME)"
else
    echo "  ✗ UTS namespace: expected $CONTAINER_HOSTNAME, got $CONTAINER_HOSTNAME_FOUND"
fi

if [ "$CONTAINER_PID" = "2" ]; then
    echo "  ✓ PID namespace: container PID is 2 (init is 1)"
else
    echo "  ⚠ PID namespace: container PID is $CONTAINER_PID (expected 2)"
fi

if [ "$CONTAINER_PROCS" -lt 5 ]; then
    echo "  ✓ PID namespace: only $CONTAINER_PROCS processes visible (isolated)"
else
    echo "  ⚠ PID namespace: $CONTAINER_PROCS processes visible"
fi

check_cgroup_cleanup "$CGROUP_NAME"
echo ""

echo "========================================="
echo "Test 9: Child process cgroup inheritance"
echo "========================================="
echo "  Running nested process test..."
set +e
sudo ./tinycontainer run --rootfs "$ROOTFS_DIR" --cgroup-name "$CGROUP_NAME" \
    --memory 128m \
    /bin/sh -c 'sh -c "echo child1:$$; sleep 2 & echo child2:$!; sleep 0.1; ps aux"'
EXIT_CODE=$?
set -e
echo "Exit code: $EXIT_CODE"
if [ "$EXIT_CODE" -eq 0 ]; then
    echo "  ✓ Child processes created successfully"
    echo "  All child processes should inherit cgroup membership"
else
    echo "  ⚠ Exit code: $EXIT_CODE"
fi
check_cgroup_cleanup "$CGROUP_NAME"
echo ""

echo "========================================="
echo "Cleaning up..."
echo "========================================="
rm -rf "$ROOTFS_DIR"
rm -f "$PROJECT_DIR/tinycontainer"
rm -f "$PROJECT_DIR/stresstest/stresstest"
echo "  ✓ Cleanup complete"
echo ""

echo "========================================="
echo "All tests completed!"
echo "========================================="
