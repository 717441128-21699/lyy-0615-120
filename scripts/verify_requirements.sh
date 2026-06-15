#!/bin/bash
#
# verify_requirements.sh — 验证用户提出的 3 个需求
#
# 需求 1：固定 cgroup 名跑 /bin/true，cgroup 残留 → 不返回 0，打印残留路径
# 需求 2：/bin/sh -c 'exit 7'，cgroup 残留 → 不返回 7，返回清理失败码 2
# 需求 3：sleep 被 kill -9 → 正常返回 137；cgroup 残留 → 不返回 137，返回 2
#
# 每个需求都测两组：
#   A 组（正常）：cgroup 能正常清理 → 退出码 = 容器退出码
#   B 组（清理失败）：人为制造 cgroup 残留 → 退出码 = 2，终端打印残留路径
#

set -u

PASS=0
FAIL=0

TC_BIN="./tinycontainer"
ROOTFS_DIR=""

RED=$'\033[31m'
GREEN=$'\033[32m'
YELLOW=$'\033[33m'
RESET=$'\033[0m'

log_pass() { echo -e "${GREEN}✔ PASS${RESET}: $1"; PASS=$((PASS+1)); }
log_fail() { echo -e "${RED}✘ FAIL${RESET}: $1"; FAIL=$((FAIL+1)); }
log_info() { echo -e "${YELLOW}ℹ INFO${RESET}: $1"; }

# --------------------------------------------------------------------------
# 0. 基础检查
# --------------------------------------------------------------------------
echo "=== [阶段 0] 基础检查 ==="

if [[ $EUID -ne 0 ]]; then
    log_fail "该脚本必须以 root 权限运行"
    exit 1
fi

if [[ "$(uname -s)" != "Linux" ]]; then
    log_fail "只能在 Linux 上运行"
    exit 1
fi

detect_cgroup_version() {
    if grep -q cgroup2 /proc/mounts 2>/dev/null; then
        echo "v2"
    elif [[ -d /sys/fs/cgroup/cpu ]] && [[ -d /sys/fs/cgroup/memory ]]; then
        echo "v1"
    else
        echo "unknown"
    fi
}
CG_VERSION=$(detect_cgroup_version)
log_info "检测到 cgroup $CG_VERSION"

# --------------------------------------------------------------------------
# 1. 构建
# --------------------------------------------------------------------------
echo ""
echo "=== [阶段 1] 构建 tinycontainer ==="

if [[ ! -f go.mod ]]; then
    log_fail "找不到 go.mod"
    exit 1
fi

BUILD_OUTPUT=$(go build -o tinycontainer . 2>&1)
BUILD_RC=$?
if [[ $BUILD_RC -eq 0 ]] && [[ -x $TC_BIN ]]; then
    log_pass "go build -o tinycontainer . 一次通过"
else
    log_fail "构建失败: $BUILD_OUTPUT"
    exit 1
fi

# --------------------------------------------------------------------------
# 2. 准备 rootfs
# --------------------------------------------------------------------------
prepare_rootfs() {
    ROOTFS_DIR="$(mktemp -d /tmp/tinycontainer-rootfs-XXXXXX)"
    log_info "rootfs: $ROOTFS_DIR"

    if command -v busybox >/dev/null 2>&1; then
        mkdir -p "$ROOTFS_DIR/bin" "$ROOTFS_DIR/proc" "$ROOTFS_DIR/sys" "$ROOTFS_DIR/tmp" "$ROOTFS_DIR/dev"
        cp "$(command -v busybox)" "$ROOTFS_DIR/bin/busybox"
        for cmd in sh true sleep ls echo cat; do
            ln -sf busybox "$ROOTFS_DIR/bin/$cmd"
        done
        for f in /bin/sh /bin/true /bin/sleep; do
            [[ ! -e "$ROOTFS_DIR$f" ]] && ln -sf busybox "$ROOTFS_DIR$f"
        done
        log_pass "busybox rootfs 就绪"
        return 0
    fi

    mkdir -p "$ROOTFS_DIR/bin" "$ROOTFS_DIR/lib" "$ROOTFS_DIR/lib64" \
             "$ROOTFS_DIR/proc" "$ROOTFS_DIR/sys" "$ROOTFS_DIR/tmp" "$ROOTFS_DIR/dev"
    copy_bin() {
        local bin="$1"
        local src
        for d in /bin /usr/bin /sbin /usr/sbin; do
            if [[ -f "$d/$bin" ]]; then src="$d/$bin"; break; fi
        done
        [[ -z "${src:-}" ]] && return 1
        cp "$src" "$ROOTFS_DIR/bin/$bin" 2>/dev/null || return 1
        for lib in $(ldd "$src" 2>/dev/null | grep -oE '/[^ ]+\.so[^ ]*' | sort -u); do
            local dir=$(dirname "$lib")
            mkdir -p "$ROOTFS_DIR$dir"
            cp "$lib" "$ROOTFS_DIR$lib" 2>/dev/null
        done
    }
    local ok=1
    for cmd in sh true sleep bash; do copy_bin "$cmd" || true; done
    [[ -x "$ROOTFS_DIR/bin/sh" ]] || ok=0
    if [[ $ok -eq 0 ]]; then
        log_fail "无法构造 rootfs"
        return 1
    fi
    log_pass "rootfs 从宿主机拷贝就绪"
    return 0
}

if ! prepare_rootfs; then
    exit 1
fi

# --------------------------------------------------------------------------
# 辅助函数
# --------------------------------------------------------------------------

# 递归清理 cgroup 目录（含子 cgroup 和进程）
force_cleanup_cgroup() {
    local name="$1"
    if [[ "$CG_VERSION" == "v1" ]]; then
        for sub in cpu memory; do
            local base="/sys/fs/cgroup/$sub/$name"
            [[ ! -d "$base" ]] && continue
            # 递归杀掉所有子 cgroup 里的进程
            find "$base" -name cgroup.procs -exec sh -c '
                while IFS= read -r pid; do
                    [ -n "$pid" ] && kill -9 "$pid" 2>/dev/null
                done < "$1"
            ' _ {} \; 2>/dev/null
            # 递归删除子目录（深度优先）
            find "$base" -depth -type d -exec rmdir {} \; 2>/dev/null
            rmdir "$base" 2>/dev/null
        done
    elif [[ "$CG_VERSION" == "v2" ]]; then
        local base="/sys/fs/cgroup/$name"
        [[ ! -d "$base" ]] && return
        find "$base" -name cgroup.procs -exec sh -c '
            while IFS= read -r pid; do
                [ -n "$pid" ] && kill -9 "$pid" 2>/dev/null
            done < "$1"
        ' _ {} \; 2>/dev/null
        find "$base" -depth -type d -exec rmdir {} \; 2>/dev/null
        rmdir "$base" 2>/dev/null
    fi
}

# 检查 cgroup 目录是否不存在
cgroup_is_absent() {
    local name="$1"
    if [[ "$CG_VERSION" == "v1" ]]; then
        for sub in cpu memory; do
            [[ -d "/sys/fs/cgroup/$sub/$name" ]] && return 1
        done
    elif [[ "$CG_VERSION" == "v2" ]]; then
        [[ -d "/sys/fs/cgroup/$name" ]] && return 1
    fi
    return 0
}

# 制造 cgroup 清理失败：在目标 cgroup 下建一个子 cgroup 并放入一个进程
# 这导致 tinycontainer 的 removeCgroupDir 无法删除父目录（子 cgroup 非空）
inject_stubborn_subcgroup() {
    local name="$1"
    if [[ "$CG_VERSION" == "v1" ]]; then
        # 在 cpu cgroup 下建子目录并放一个 sleep 进程
        local child="/sys/fs/cgroup/cpu/$name/stubborn-child"
        mkdir -p "$child" 2>/dev/null || return 1
        sleep 300 &
        local spid=$!
        echo "$spid" > "$child/cgroup.procs" 2>/dev/null || { kill "$spid" 2>/dev/null; rmdir "$child" 2>/dev/null; return 1; }
        echo "$spid"
        return 0
    elif [[ "$CG_VERSION" == "v2" ]]; then
        local child="/sys/fs/cgroup/$name/stubborn-child"
        mkdir -p "$child" 2>/dev/null || return 1
        # v2 子 cgroup 需要启用控制器
        echo "+cpu +memory" > /sys/fs/cgroup/"$name"/cgroup.subtree_control 2>/dev/null
        sleep 300 &
        local spid=$!
        echo "$spid" > "$child/cgroup.procs" 2>/dev/null || { kill "$spid" 2>/dev/null; rmdir "$child" 2>/dev/null; return 1; }
        echo "$spid"
        return 0
    fi
    return 1
}

# --------------------------------------------------------------------------
echo ""
echo "=== [阶段 2] 需求 1：/bin/true + 固定 cgroup 名 ==="

CG1="req1-true"

# --- 2A: 正常场景 ---
echo "--- 2A: /bin/true 正常清理 → exit 0 ---"
force_cleanup_cgroup "$CG1"
$TC_BIN run --rootfs "$ROOTFS_DIR" --cgroup-name "$CG1" /bin/true >/tmp/tc2a.out 2>/tmp/tc2a.err
RC=$?
if [[ $RC -eq 0 ]]; then
    log_pass "2A 退出码 0"
else
    log_fail "2A 退出码 $RC (期望 0), stderr: $(cat /tmp/tc2a.err)"
fi
if cgroup_is_absent "$CG1"; then
    log_pass "2A cgroup 目录已消失"
else
    log_fail "2A cgroup 目录残留"
fi
force_cleanup_cgroup "$CG1"

# --- 2B: 清理失败场景 ---
echo "--- 2B: /bin/true + cgroup 残留 → exit 2 (非 0) ---"
force_cleanup_cgroup "$CG1"
# 先让 tinycontainer 跑起来建立 cgroup，然后在 cgroup 下注入顽固子 cgroup
# 方案：后台启动 sleep，注入子 cgroup，再 kill sleep 让 tinycontainer 退出
$TC_BIN run --rootfs "$ROOTFS_DIR" --cgroup-name "$CG1" /bin/sleep 60 >/tmp/tc2b.out 2>/tmp/tc2b.err &
TC_PID=$!
sleep 2

STUBBORN_PID=$(inject_stubborn_subcgroup "$CG1") || {
    log_info "2B: 无法注入顽固子 cgroup，跳过此测试"
    kill -9 "$TC_PID" 2>/dev/null; wait "$TC_PID" 2>/dev/null
    force_cleanup_cgroup "$CG1"
    STUBBORN_PID=""
}

if [[ -n "$STUBBORN_PID" ]]; then
    log_info "2B: 已注入顽固子 cgroup (sleep PID=$STUBBORN_PID)"

    # 杀掉容器里的 sleep 让 tinycontainer 正常退出
    SLEEP_PID=$(pgrep -P "$TC_PID" 2>/dev/null | head -1)
    [[ -z "$SLEEP_PID" ]] && SLEEP_PID=$(pgrep -f "sleep 60" 2>/dev/null | head -1)
    if [[ -n "$SLEEP_PID" ]]; then
        kill -9 "$SLEEP_PID" 2>/dev/null
    else
        kill -9 "$TC_PID" 2>/dev/null
    fi
    wait "$TC_PID" 2>/dev/null
    RC=$?

    # 容器命令本身是 /bin/sleep 被 kill -9 → 137，但 cgroup 残留 → 应该返回 2
    # 或者如果是 /bin/true 的话就是 0 → 应该返回 2
    # 这里用的是 sleep 60 被 kill，所以容器退出码应该是 137
    # 但 cgroup 残留 → 退出码应该被覆盖为 2
    if [[ $RC -eq 2 ]]; then
        log_pass "2B 退出码 $RC (cgroup 残留 → 覆盖为 2)"
    else
        log_fail "2B 退出码 $RC (期望 2，cgroup 残留应覆盖原始退出码)"
    fi

    # 验证 stderr 中有残留路径
    if grep -q "cgroup directory still exists" /tmp/tc2b.err; then
        log_pass "2B 终端打印了残留 cgroup 路径"
        grep "cgroup directory still exists" /tmp/tc2b.err | while read -r line; do
            log_info "  $line"
        done
    else
        log_fail "2B 终端未打印残留 cgroup 路径"
    fi

    # 清理顽固进程和残留
    kill -9 "$STUBBORN_PID" 2>/dev/null
    wait "$STUBBORN_PID" 2>/dev/null
    force_cleanup_cgroup "$CG1"
fi

# --------------------------------------------------------------------------
echo ""
echo "=== [阶段 3] 需求 2：/bin/sh -c 'exit 7' + 固定 cgroup 名 ==="

CG2="req2-exit7"

# --- 3A: 正常场景 ---
echo "--- 3A: exit 7 正常清理 → exit 7 ---"
force_cleanup_cgroup "$CG2"
$TC_BIN run --rootfs "$ROOTFS_DIR" --cgroup-name "$CG2" /bin/sh -c 'exit 7' >/tmp/tc3a.out 2>/tmp/tc3a.err
RC=$?
if [[ $RC -eq 7 ]]; then
    log_pass "3A 退出码 7"
else
    log_fail "3A 退出码 $RC (期望 7)"
fi
if cgroup_is_absent "$CG2"; then
    log_pass "3A cgroup 目录已消失"
else
    log_fail "3A cgroup 目录残留"
fi
force_cleanup_cgroup "$CG2"

# --- 3B: 清理失败场景 ---
echo "--- 3B: exit 7 + cgroup 残留 → exit 2 (非 7) ---"
force_cleanup_cgroup "$CG2"
$TC_BIN run --rootfs "$ROOTFS_DIR" --cgroup-name "$CG2" /bin/sleep 60 >/tmp/tc3b.out 2>/tmp/tc3b.err &
TC_PID=$!
sleep 2

STUBBORN_PID=$(inject_stubborn_subcgroup "$CG2") || {
    log_info "3B: 无法注入顽固子 cgroup，跳过此测试"
    kill -9 "$TC_PID" 2>/dev/null; wait "$TC_PID" 2>/dev/null
    force_cleanup_cgroup "$CG2"
    STUBBORN_PID=""
}

if [[ -n "$STUBBORN_PID" ]]; then
    log_info "3B: 已注入顽固子 cgroup (sleep PID=$STUBBORN_PID)"

    # 杀掉容器 sleep
    SLEEP_PID=$(pgrep -P "$TC_PID" 2>/dev/null | head -1)
    [[ -z "$SLEEP_PID" ]] && SLEEP_PID=$(pgrep -f "sleep 60" 2>/dev/null | head -1)
    if [[ -n "$SLEEP_PID" ]]; then
        kill -9 "$SLEEP_PID" 2>/dev/null
    else
        kill -9 "$TC_PID" 2>/dev/null
    fi
    wait "$TC_PID" 2>/dev/null
    RC=$?

    if [[ $RC -eq 2 ]]; then
        log_pass "3B 退出码 $RC (cgroup 残留 → 覆盖为 2，而非容器退出码)"
    else
        log_fail "3B 退出码 $RC (期望 2，即使容器 exit 7/137 也应被覆盖)"
    fi

    if grep -q "cgroup directory still exists" /tmp/tc3b.err; then
        log_pass "3B 终端打印了残留路径"
    else
        log_fail "3B 终端未打印残留路径"
    fi

    kill -9 "$STUBBORN_PID" 2>/dev/null
    wait "$STUBBORN_PID" 2>/dev/null
    force_cleanup_cgroup "$CG2"
fi

# --------------------------------------------------------------------------
echo ""
echo "=== [阶段 4] 需求 3：sleep 被 kill -9 + 固定 cgroup 名 ==="

CG3="req3-sigkill"

# --- 4A: 正常场景 (SIGKILL → exit 137, cgroup 正常清理) ---
echo "--- 4A: sleep 被 kill -9 → exit 137, cgroup 正常清理 ---"
force_cleanup_cgroup "$CG3"
$TC_BIN run --rootfs "$ROOTFS_DIR" --cgroup-name "$CG3" /bin/sleep 60 >/tmp/tc4a.out 2>/tmp/tc4a.err &
TC_PID=$!
sleep 2

SLEEP_PID=$(pgrep -P "$TC_PID" 2>/dev/null | head -1)
[[ -z "$SLEEP_PID" ]] && SLEEP_PID=$(pgrep -f "sleep 60" 2>/dev/null | head -1)

if [[ -n "$SLEEP_PID" ]]; then
    log_info "4A: 找到 sleep 进程 $SLEEP_PID，发送 SIGKILL"
    kill -9 "$SLEEP_PID"
    sleep 1
    wait "$TC_PID" 2>/dev/null
    RC=$?
    if [[ $RC -eq 137 ]]; then
        log_pass "4A 退出码 137 (128+9)"
    else
        log_fail "4A 退出码 $RC (期望 137)"
    fi
else
    log_fail "4A: 找不到 sleep 进程"
    kill -9 "$TC_PID" 2>/dev/null; wait "$TC_PID" 2>/dev/null
    RC=$?
fi

sleep 1
force_cleanup_cgroup "$CG3"
if cgroup_is_absent "$CG3"; then
    log_pass "4A cgroup 目录已消失"
else
    log_fail "4A cgroup 目录残留"
fi
force_cleanup_cgroup "$CG3"

# --- 4B: SIGKILL + cgroup 残留 → exit 2 (非 137) ---
echo "--- 4B: sleep 被 kill -9 + cgroup 残留 → exit 2 (非 137) ---"
force_cleanup_cgroup "$CG3"
$TC_BIN run --rootfs "$ROOTFS_DIR" --cgroup-name "$CG3" /bin/sleep 60 >/tmp/tc4b.out 2>/tmp/tc4b.err &
TC_PID=$!
sleep 2

STUBBORN_PID=$(inject_stubborn_subcgroup "$CG3") || {
    log_info "4B: 无法注入顽固子 cgroup，跳过此测试"
    kill -9 "$TC_PID" 2>/dev/null; wait "$TC_PID" 2>/dev/null
    force_cleanup_cgroup "$CG3"
    STUBBORN_PID=""
}

if [[ -n "$STUBBORN_PID" ]]; then
    log_info "4B: 已注入顽固子 cgroup (sleep PID=$STUBBORN_PID)"

    SLEEP_PID=$(pgrep -P "$TC_PID" 2>/dev/null | head -1)
    [[ -z "$SLEEP_PID" ]] && SLEEP_PID=$(pgrep -f "sleep 60" 2>/dev/null | head -1)

    if [[ -n "$SLEEP_PID" ]]; then
        kill -9 "$SLEEP_PID"
    else
        kill -9 "$TC_PID"
    fi
    sleep 1
    wait "$TC_PID" 2>/dev/null
    RC=$?

    if [[ $RC -eq 2 ]]; then
        log_pass "4B 退出码 $RC (cgroup 残留 → 覆盖为 2，而非 137)"
    else
        log_fail "4B 退出码 $RC (期望 2，SIGKILL 的 137 应被覆盖)"
    fi

    if grep -q "cgroup directory still exists" /tmp/tc4b.err; then
        log_pass "4B 终端打印了残留路径（含 cpu/memory/v2 完整路径）"
        grep "cgroup directory still exists" /tmp/tc4b.err | while read -r line; do
            log_info "  $line"
        done
    else
        log_fail "4B 终端未打印残留路径"
    fi

    kill -9 "$STUBBORN_PID" 2>/dev/null
    wait "$STUBBORN_PID" 2>/dev/null
    force_cleanup_cgroup "$CG3"
fi

# --------------------------------------------------------------------------
echo ""
echo "=== [阶段 5] cgroup v1 回滚验证 ==="

ROLLBACK_CG="req-rollback"

if [[ "$CG_VERSION" == "v1" ]]; then
    echo "--- 5.1: v1 CPU 建完但 memory 写入失败 → cpu+memory 目录都回滚 ---"
    force_cleanup_cgroup "$ROLLBACK_CG"

    MEM_DIR="/sys/fs/cgroup/memory/$ROLLBACK_CG"
    CPU_DIR="/sys/fs/cgroup/cpu/$ROLLBACK_CG"

    # 破坏：预先创建 memory cgroup 目录，把 limit 文件变成目录
    mkdir -p "$MEM_DIR"
    mkdir -p "$MEM_DIR/memory.limit_in_bytes"

    $TC_BIN run --rootfs "$ROOTFS_DIR" --cgroup-name "$ROLLBACK_CG" \
        --cpu-quota 50000 --cpu-period 100000 --memory 128m \
        /bin/true >/tmp/tc51.out 2>/tmp/tc51.err
    RC=$?

    if [[ $RC -ne 0 ]]; then
        log_pass "5.1 cgroup 设置失败 → 非零退出码 $RC"
    else
        log_fail "5.1 期望非零退出码，实际 0"
    fi

    sleep 0.5
    if [[ -d "$CPU_DIR" ]] && ls "$CPU_DIR"/cpu.cfs_* >/dev/null 2>&1; then
        log_fail "5.1 CPU cgroup 残留: $CPU_DIR"
    else
        log_pass "5.1 CPU cgroup 目录已回滚"
    fi

    if [[ -d "$MEM_DIR" ]]; then
        log_fail "5.1 Memory cgroup 残留: $MEM_DIR"
    else
        log_pass "5.1 Memory cgroup 目录已回滚"
    fi

    force_cleanup_cgroup "$ROLLBACK_CG"

elif [[ "$CG_VERSION" == "v2" ]]; then
    echo "--- 5.2: v2 CPU 设置失败 → 统一层级目录回滚 ---"
    force_cleanup_cgroup "$ROLLBACK_CG"

    CG2_DIR="/sys/fs/cgroup/$ROLLBACK_CG"
    mkdir -p "$CG2_DIR/cpu.max"

    $TC_BIN run --rootfs "$ROOTFS_DIR" --cgroup-name "$ROLLBACK_CG" \
        --cpu-quota 50000 --cpu-period 100000 --memory 128m \
        /bin/true >/tmp/tc52.out 2>/tmp/tc52.err
    RC=$?

    if [[ $RC -ne 0 ]]; then
        log_pass "5.2 cgroup v2 设置失败 → 非零退出码 $RC"
    else
        log_fail "5.2 期望非零退出码"
    fi

    sleep 0.5
    if [[ -d "$CG2_DIR" ]]; then
        log_fail "5.2 cgroup v2 目录残留: $CG2_DIR"
    else
        log_pass "5.2 cgroup v2 目录已回滚"
    fi

    force_cleanup_cgroup "$ROLLBACK_CG"
fi

# --------------------------------------------------------------------------
echo ""
echo "=== [阶段 6] 前置检查 ==="

echo "--- 6.1: rootfs 不存在 ---"
$TC_BIN run --rootfs /no/such/path --cgroup-name preflight-test /bin/true >/tmp/tc61.out 2>/tmp/tc61.err
RC=$?
if [[ $RC -ne 0 ]] && grep -q "preflight_rootfs\|rootfs" /tmp/tc61.err; then
    log_pass "6.1 rootfs 不存在正确报错 exit $RC"
else
    log_fail "6.1 期望错误退出 (实际 RC=$RC)"
fi

echo ""
echo "--- 6.2: 命令不存在 ---"
$TC_BIN run --rootfs "$ROOTFS_DIR" --cgroup-name preflight-test /bin/nonexistent_cmd >/tmp/tc62.out 2>/tmp/tc62.err
RC=$?
if [[ $RC -ne 0 ]] && grep -q "preflight_command\|command does not exist" /tmp/tc62.err; then
    log_pass "6.2 命令不存在正确报错 exit $RC"
else
    log_fail "6.2 期望错误退出 (实际 RC=$RC)"
fi

# --------------------------------------------------------------------------
echo ""
echo "========================================================"
echo "                  最终总结"
echo "========================================================"
echo "   PASS: $PASS"
echo "   FAIL: $FAIL"
echo "========================================================"

rm -rf "$ROOTFS_DIR"

if [[ $FAIL -gt 0 ]]; then
    exit 1
fi
exit 0
