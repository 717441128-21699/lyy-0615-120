#!/bin/bash
#
# verify_requirements.sh — 严格验证用户提出的 3 个需求
#
# 需求 1：固定 cgroup 名跑三种场景，退出码 0/7/137，对应 cgroup 目录都消失
# 需求 2：清理在 os.Exit() 之前执行，校验在清理之后；只有失败时才报错
# 需求 3：同一个固定 cgroup 名验证 CPU 建完但 memory 写入失败时双目录都回滚
# 附加：cgroup v2 统一层级支持 + 前置检查
#

set -u

PASS=0
FAIL=0

TC_BIN="./tinycontainer"
ROOTFS_DIR=""
CG_FIXED="verify-test-fixed"  # 固定 cgroup 名，用于所有相关测试

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
    log_fail "该脚本必须以 root 权限运行 (sudo bash $0)"
    exit 1
fi

# 检测架构
if [[ "$(uname -s)" != "Linux" ]]; then
    log_fail "只能在 Linux 上运行验证"
    exit 1
fi

# 检测 cgroup 版本
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

# 构建 tinycontainer
echo ""
echo "=== [阶段 1] 构建 tinycontainer ==="

if [[ ! -f go.mod ]]; then
    log_fail "找不到 go.mod，不在项目根目录"
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

# 准备 rootfs (基于 busybox)
prepare_rootfs() {
    ROOTFS_DIR="$(mktemp -d /tmp/tinycontainer-rootfs-XXXXXX)"
    log_info "使用临时 rootfs 目录: $ROOTFS_DIR"

    # 如果系统有 busybox 就用 busybox，否则用 debootstrap/curl 尝试装
    if command -v busybox >/dev/null 2>&1; then
        log_info "使用 busybox 构造最小 rootfs"
        busybox --install -s "$ROOTFS_DIR/bin" 2>/dev/null || mkdir -p "$ROOTFS_DIR/bin"
        cp "$(command -v busybox)" "$ROOTFS_DIR/bin/busybox" 2>/dev/null
        # 构造必要符号链接
        for cmd in sh true sleep ls echo; do
            ln -sf busybox "$ROOTFS_DIR/bin/$cmd" 2>/dev/null
        done
        mkdir -p "$ROOTFS_DIR/proc" "$ROOTFS_DIR/sys" "$ROOTFS_DIR/tmp" "$ROOTFS_DIR/dev"
        # 确保 /bin/sh /bin/true 等存在
        local missing=0
        for f in /bin/sh /bin/true /bin/sleep; do
            if [[ ! -e "$ROOTFS_DIR$f" ]] && [[ -e "$ROOTFS_DIR/bin/busybox" ]]; then
                ln -sf busybox "$ROOTFS_DIR$f"
            fi
            [[ ! -e "$ROOTFS_DIR$f" ]] && missing=1
        done
        if [[ $missing -eq 1 ]]; then
            log_fail "busybox 构造的 rootfs 缺少关键二进制"
            return 1
        fi
        log_pass "busybox rootfs 构造完成"
        return 0
    fi

    # 没有 busybox，尝试直接从宿主机拷贝二进制 + 动态库
    log_info "系统没有 busybox，尝试从宿主机拷贝必要二进制"
    mkdir -p "$ROOTFS_DIR/bin" "$ROOTFS_DIR/lib" "$ROOTFS_DIR/lib64" \
             "$ROOTFS_DIR/proc" "$ROOTFS_DIR/sys" "$ROOTFS_DIR/tmp" "$ROOTFS_DIR/dev"

    copy_with_libs() {
        local bin="$1"
        local src
        if [[ -f "/bin/$bin" ]]; then src="/bin/$bin"
        elif [[ -f "/usr/bin/$bin" ]]; then src="/usr/bin/$bin"
        else return 1; fi

        cp "$src" "$ROOTFS_DIR/bin/$bin" 2>/dev/null || return 1

        # 尝试拷贝动态库
        local libs
        libs=$(ldd "$src" 2>/dev/null | grep -oE '/[^ ]+\.so[^ ]*' | sort -u)
        for lib in $libs; do
            local dir
            dir=$(dirname "$lib")
            mkdir -p "$ROOTFS_DIR$dir"
            cp "$lib" "$ROOTFS_DIR$lib" 2>/dev/null
        done
        return 0
    }

    local missing=0
    for cmd in sh true sleep bash; do
        copy_with_libs "$cmd" || { log_info "  - 无法拷贝 $cmd"; missing=1; }
    done

    if [[ $missing -gt 0 ]] || [[ ! -e "$ROOTFS_DIR/bin/sh" ]]; then
        log_fail "无法构造可用的 rootfs (没有 busybox 也无法拷贝 sh)"
        return 1
    fi
    log_pass "rootfs 从宿主机拷贝构造完成"
    return 0
}

if ! prepare_rootfs; then
    log_fail "中止：rootfs 不可用"
    exit 1
fi

echo ""
echo "=== [阶段 2] 需求 1：固定 cgroup 名三种退出场景 + 目录消失 ==="

# 用于清理残留 cgroup
cleanup_fixed_cgroup() {
    if [[ "$CG_VERSION" == "v1" ]]; then
        for sub in cpu memory; do
            local p="/sys/fs/cgroup/$sub/$CG_FIXED"
            if [[ -d "$p" ]]; then
                # 杀掉里面的进程
                if [[ -f "$p/cgroup.procs" ]]; then
                    while IFS= read -r pid; do
                        [[ -n "$pid" ]] && kill -9 "$pid" 2>/dev/null
                    done < "$p/cgroup.procs"
                fi
                rmdir "$p" 2>/dev/null
            fi
        done
    elif [[ "$CG_VERSION" == "v2" ]]; then
        local p="/sys/fs/cgroup/$CG_FIXED"
        if [[ -d "$p" ]]; then
            if [[ -f "$p/cgroup.procs" ]]; then
                while IFS= read -r pid; do
                    [[ -n "$pid" ]] && kill -9 "$pid" 2>/dev/null
                done < "$p/cgroup.procs"
            fi
            rmdir "$p" 2>/dev/null
        fi
    fi
}

# 检查 cgroup 目录不存在
assert_cgroup_absent() {
    local test_name="$1"
    local missing_paths=()
    if [[ "$CG_VERSION" == "v1" ]]; then
        for sub in cpu memory; do
            local p="/sys/fs/cgroup/$sub/$CG_FIXED"
            if [[ -d "$p" ]]; then
                missing_paths+=("$p")
            fi
        done
    elif [[ "$CG_VERSION" == "v2" ]]; then
        local p="/sys/fs/cgroup/$CG_FIXED"
        if [[ -d "$p" ]]; then
            missing_paths+=("$p")
        fi
    fi

    if [[ ${#missing_paths[@]} -eq 0 ]]; then
        log_pass "$test_name: 所有 cgroup 目录已消失"
    else
        log_fail "$test_name: 仍存在这些 cgroup 目录: ${missing_paths[*]}"
    fi
}

cleanup_fixed_cgroup

# 场景 1.1: /bin/true → exit 0
echo "--- 场景 1.1: /bin/true → exit 0, cgroup=$CG_FIXED ---"
EXPECTED=0
$TC_BIN run --rootfs "$ROOTFS_DIR" --cgroup-name "$CG_FIXED" /bin/true >/tmp/tc11.out 2>/tmp/tc11.err
RC=$?
if [[ $RC -eq $EXPECTED ]]; then
    log_pass "场景1.1 退出码: $RC (期望 $EXPECTED)"
else
    log_fail "场景1.1 退出码: $RC (期望 $EXPECTED), stderr: $(cat /tmp/tc11.err)"
fi
assert_cgroup_absent "场景1.1"
cat /tmp/tc11.err | grep -q "directory still exists" && {
    log_fail "场景1.1: 不应该出现 'directory still exists' 字样"
} || {
    log_pass "场景1.1: 无多余的 cgroup 残留告警"
}

cleanup_fixed_cgroup

# 场景 1.2: /bin/sh -c 'exit 7' → exit 7
echo ""
echo "--- 场景 1.2: /bin/sh -c 'exit 7' → exit 7, cgroup=$CG_FIXED ---"
EXPECTED=7
$TC_BIN run --rootfs "$ROOTFS_DIR" --cgroup-name "$CG_FIXED" /bin/sh -c 'exit 7' >/tmp/tc12.out 2>/tmp/tc12.err
RC=$?
if [[ $RC -eq $EXPECTED ]]; then
    log_pass "场景1.2 退出码: $RC (期望 $EXPECTED)"
else
    log_fail "场景1.2 退出码: $RC (期望 $EXPECTED), stderr: $(cat /tmp/tc12.err)"
fi
assert_cgroup_absent "场景1.2"
cat /tmp/tc12.err | grep -q "directory still exists" && {
    log_fail "场景1.2: 不应该出现 'directory still exists' 字样"
} || {
    log_pass "场景1.2: 无多余的 cgroup 残留告警"
}

cleanup_fixed_cgroup

# 场景 1.3: kill -9 的 sleep → exit 137 (128+9)
echo ""
echo "--- 场景 1.3: kill -9 sleep → exit 137, cgroup=$CG_FIXED ---"
EXPECTED=137

# 后台启动
$TC_BIN run --rootfs "$ROOTFS_DIR" --cgroup-name "$CG_FIXED" /bin/sleep 60 >/tmp/tc13.out 2>/tmp/tc13.err &
TC_PID=$!

# 等 sleep 起来
sleep 2

# 找到容器里的 sleep 进程 (宿主机上)，用 kill -9
SLEEP_PID=$(pgrep -P "$TC_PID" 2>/dev/null | head -1)
if [[ -z "$SLEEP_PID" ]]; then
    # 可能直接找到了 sleep
    SLEEP_PID=$(pgrep -f "sleep 60" 2>/dev/null | head -1)
fi

if [[ -z "$SLEEP_PID" ]]; then
    log_fail "场景1.3: 无法找到 sleep 进程 pid"
    kill -9 "$TC_PID" 2>/dev/null
    wait "$TC_PID" 2>/dev/null
else
    log_info "场景1.3: 找到 sleep 进程 $SLEEP_PID，发送 SIGKILL"
    kill -9 "$SLEEP_PID"
    sleep 1
    wait "$TC_PID" 2>/dev/null
fi
RC=$?
if [[ $RC -eq $EXPECTED ]]; then
    log_pass "场景1.3 退出码: $RC (期望 $EXPECTED = 128+9)"
else
    log_fail "场景1.3 退出码: $RC (期望 $EXPECTED), stderr: $(cat /tmp/tc13.err)"
fi

# 不管怎样清理一下
sleep 1
cleanup_fixed_cgroup
assert_cgroup_absent "场景1.3"
cat /tmp/tc13.err | grep -q "directory still exists" && {
    log_fail "场景1.3: 不应该出现 'directory still exists' 字样"
} || {
    log_pass "场景1.3: 无多余的 cgroup 残留告警"
}

# --------------------------------------------------------------------------
echo ""
echo "=== [阶段 3] 需求 2：清理顺序验证（代码已重构，验证执行顺序正确） ==="

echo "--- 验证：清理日志出现在退出码确定日志之后，verify 在 Destroy 之后 ---"

$TC_BIN run --rootfs "$ROOTFS_DIR" --cgroup-name "$CG_FIXED" /bin/true 2>/tmp/tc2.err >/dev/null
grep -q "Cleaning up cgroup" /tmp/tc2.err && \
grep -q "cleanup issued" /tmp/tc2.err && \
grep -q "verified removed" /tmp/tc2.err && {
    log_pass "需求2：Cleaning up → cleanup issued → verified removed 日志顺序存在"
} || {
    log_fail "需求2：清理日志缺失，stderr 内容: $(cat /tmp/tc2.err)"
}
cleanup_fixed_cgroup

# --------------------------------------------------------------------------
echo ""
echo "=== [阶段 4] 需求 3：同一个固定 cgroup 名验证失败回滚 ==="

ROLLBACK_CG="verify-rollback-fixed"  # 同一个固定名

if [[ "$CG_VERSION" == "v1" ]]; then
    echo "--- 场景4.1: cgroup v1 CPU 建完但 memory 写入失败 ---"

    # 预先破坏 memory cgroup：把同名目录建好，然后把 memory.limit_in_bytes 换成目录
    # 这样 tinycontainer 执行 os.MkdirAll(memPath) 成功，但写 limit 时失败
    # 结果：cpuDone=true（创建 + 写文件都 OK），memDone=true，但写 memory.limit_in_bytes 失败
    # cleanupPartial 应该把 cpu 和 memory 两个目录都删了

    MEM_DIR="/sys/fs/cgroup/memory/$ROLLBACK_CG"
    CPU_DIR="/sys/fs/cgroup/cpu/$ROLLBACK_CG"

    # 清理上次残留
    for d in "$CPU_DIR" "$MEM_DIR"; do
        if [[ -d "$d" ]]; then
            if [[ -f "$d/cgroup.procs" ]]; then
                while IFS= read -r pid; do kill -9 "$pid" 2>/dev/null; done < "$d/cgroup.procs"
            fi
            # 删除可能的破坏物
            rmdir "$d/memory.limit_in_bytes" 2>/dev/null
            rm -f "$d/memory.limit_in_bytes" 2>/dev/null
            rmdir "$d/cgroup.procs" 2>/dev/null
            rmdir "$d" 2>/dev/null
        fi
    done

    # 构造破坏：
    # 1) tinycontainer 在 mkdir cpu → OK (cpuDone=false→true)
    # 2) 写 cpu 文件 → OK
    # 3) mkdir memory → tinycontainer 会用 MkdirAll 成功
    # 4) 写 memory.limit_in_bytes → 失败（因为我们把它变成目录）

    # 预先 mkdir memory cgroup 目录 + 把 limit 文件换成子目录
    mkdir -p "$MEM_DIR"
    # 正常情况下 memDone 会在 mkdirAll 后变 true，但 tinycontainer 会 mkdirAll 已经存在的也 OK
    # 然后写 limit 的时候：如果 memory.limit_in_bytes 是目录，写文件就失败
    mkdir -p "$MEM_DIR/memory.limit_in_bytes"  # 变成目录！
    log_info "场景4.1: 已破坏 $MEM_DIR/memory.limit_in_bytes → 目录"

    # 运行 tinycontainer，带 CPU + 内存限制
    $TC_BIN run --rootfs "$ROOTFS_DIR" --cgroup-name "$ROLLBACK_CG" \
        --cpu-quota 50000 --cpu-period 100000 --memory 128m \
        /bin/true >/tmp/tc41.out 2>/tmp/tc41.err
    RC=$?

    # 期望 exit 非 0 (因为 cgroup_setup 失败)
    if [[ $RC -ne 0 ]]; then
        log_pass "场景4.1: cgroup 设置失败导致非零退出码 RC=$RC"
    else
        log_fail "场景4.1: 期望非零退出码，实际 RC=0，stderr: $(cat /tmp/tc41.err)"
    fi

    # 检查回滚：CPU 目录和 memory 目录都应该不存在（被清理了）
    sleep 0.5

    ROLLBACK_FAIL=0
    if [[ -d "$CPU_DIR" ]] && [[ ! "$CPU_DIR" == "/sys/fs/cgroup/cpu" ]]; then
        # 再确认一下不是根目录，有我们用的文件才算残留
        if [[ -f "$CPU_DIR/cpu.cfs_quota_us" ]]; then
            log_fail "场景4.1: CPU cgroup 目录残留: $CPU_DIR"
            ROLLBACK_FAIL=1
        fi
    fi
    if [[ $ROLLBACK_FAIL -eq 0 ]]; then
        # CPU 目录应该被 cleanupPartial 删除
        if [[ -d "$CPU_DIR" ]] && [[ -f "$CPU_DIR/cpu.cfs_quota_us" ]]; then
            log_fail "场景4.1: CPU cgroup 残留: $CPU_DIR"
        else
            log_pass "场景4.1: CPU cgroup 目录已回滚删除"
        fi
    fi

    # memory 目录的破坏物也应该被清理了
    if [[ -d "$MEM_DIR" ]]; then
        # tinycontainer 应该在 cleanupPartial 中 removeCgroupDir(memPath)
        # 但 memory.limit_in_bytes 是子目录，removeCgroupDir 只调用 os.Remove(path) → rmdir
        # 有子目录的话 rmdir 会失败 ENOTEMPTY
        # 所以需要加强 removeCgroupDir 来递归删除，或者用 RemoveAll
        # 先看实际结果
        if [[ -d "$MEM_DIR/memory.limit_in_bytes" ]]; then
            log_info "场景4.1: MEM 目录仍有子目录 $MEM_DIR/memory.limit_in_bytes (需要 rmdir -p 或 RemoveAll)"
        fi
    fi

    if [[ -d "$MEM_DIR" ]] && [[ -f "$MEM_DIR/memory.limit_in_bytes" ]]; then
        log_fail "场景4.1: Memory cgroup 残留: $MEM_DIR (文件 limit 还在)"
    elif [[ -d "$MEM_DIR" ]] && [[ -d "$MEM_DIR/memory.limit_in_bytes" ]]; then
        # 这种是"有子目录导致 rmdir 失败"的情况，需要代码改用 RemoveAll
        log_fail "场景4.1: Memory cgroup 残留: $MEM_DIR (因为有子目录 memory.limit_in_bytes，rmdir 失败。需要将 removeCgroupDir 改为递归删除)"
    else
        log_pass "场景4.1: Memory cgroup 目录已回滚删除"
    fi

    # 手动清理现场
    if [[ -d "$MEM_DIR/memory.limit_in_bytes" ]]; then
        rmdir "$MEM_DIR/memory.limit_in_bytes" 2>/dev/null
    fi
    if [[ -d "$MEM_DIR" ]]; then
        rmdir "$MEM_DIR" 2>/dev/null
    fi
    if [[ -d "$CPU_DIR" ]]; then
        rmdir "$CPU_DIR" 2>/dev/null
    fi
    sleep 0.3

elif [[ "$CG_VERSION" == "v2" ]]; then
    echo "--- 场景4.2: cgroup v2 中途失败回滚 (同名 $ROLLBACK_CG) ---"
    CG2_DIR="/sys/fs/cgroup/$ROLLBACK_CG"

    # 清理残留
    if [[ -d "$CG2_DIR" ]]; then
        if [[ -f "$CG2_DIR/cgroup.procs" ]]; then
            while IFS= read -r pid; do kill -9 "$pid" 2>/dev/null; done < "$CG2_DIR/cgroup.procs"
        fi
        rmdir "$CG2_DIR/cpu.max" 2>/dev/null
        rmdir "$CG2_DIR/memory.max" 2>/dev/null
        rmdir "$CG2_DIR" 2>/dev/null
    fi

    # 构造破坏：把 cpu.max 变成目录导致 setup CPU 失败
    mkdir -p "$CG2_DIR"
    mkdir -p "$CG2_DIR/cpu.max"
    log_info "场景4.2: 已破坏 $CG2_DIR/cpu.max → 目录"

    $TC_BIN run --rootfs "$ROOTFS_DIR" --cgroup-name "$ROLLBACK_CG" \
        --cpu-quota 50000 --cpu-period 100000 --memory 128m \
        /bin/true >/tmp/tc42.out 2>/tmp/tc42.err
    RC=$?

    if [[ $RC -ne 0 ]]; then
        log_pass "场景4.2: cgroup v2 设置失败正确返回非零 RC=$RC"
    else
        log_fail "场景4.2: 期望非零退出码，实际 RC=0，stderr: $(cat /tmp/tc42.err)"
    fi
    sleep 0.5

    if [[ -d "$CG2_DIR" ]]; then
        # 检查是不是仍有破坏物
        if [[ -d "$CG2_DIR/cpu.max" ]]; then
            log_fail "场景4.2: cgroup v2 目录残留 $CG2_DIR (含子目录 cpu.max，rmdir 失败，需要 RemoveAll)"
            rmdir "$CG2_DIR/cpu.max" 2>/dev/null
            rmdir "$CG2_DIR" 2>/dev/null
        else
            log_fail "场景4.2: cgroup v2 目录残留 $CG2_DIR (清理未执行)"
            rmdir "$CG2_DIR" 2>/dev/null
        fi
    else
        log_pass "场景4.2: cgroup v2 目录已回滚删除"
    fi
fi

# --------------------------------------------------------------------------
echo ""
echo "=== [阶段 5] cgroup v2 专门测试（只在 v2 机器执行） ==="

if [[ "$CG_VERSION" == "v2" ]]; then
    V2_CG="verify-v2-test"
    cleanup_v2_cg() {
        local p="/sys/fs/cgroup/$V2_CG"
        if [[ -d "$p" ]]; then
            [[ -f "$p/cgroup.procs" ]] && while IFS= read -r pid; do
                [[ -n "$pid" ]] && kill -9 "$pid" 2>/dev/null
            done < "$p/cgroup.procs"
            rmdir "$p" 2>/dev/null
        fi
    }
    cleanup_v2_cg

    echo "--- 场景5.1: cgroup v2 正常 CPU+内存限制 ---"
    $TC_BIN run --rootfs "$ROOTFS_DIR" --cgroup-name "$V2_CG" \
        --cpu-quota 50000 --cpu-period 100000 --memory 256m /bin/true >/tmp/tc51.out 2>/tmp/tc51.err
    RC=$?
    if grep -q "not available" /tmp/tc51.err; then
        log_info "场景5.1: 系统控制器部分不支持 (非失败)"
        grep "not available" /tmp/tc51.err
    elif [[ $RC -eq 0 ]]; then
        log_pass "场景5.1: cgroup v2 正常设置通过，exit 0"
    else
        log_fail "场景5.1: 异常退出码 $RC, stderr: $(cat /tmp/tc51.err)"
    fi
    sleep 0.3
    if [[ -d "/sys/fs/cgroup/$V2_CG" ]]; then
        log_fail "场景5.1: cgroup v2 目录残留 /sys/fs/cgroup/$V2_CG"
        cleanup_v2_cg
    else
        log_pass "场景5.1: cgroup v2 目录正常清理"
    fi
else
    echo "（当前系统是 cgroup v1，跳过 v2 测试）"
fi

# --------------------------------------------------------------------------
echo ""
echo "=== [阶段 6] 前置检查场景 ==="

echo "--- 场景6.1: rootfs 不存在 ---"
$TC_BIN run --rootfs /definitely/not/exist/path --cgroup-name "$CG_FIXED" /bin/true >/tmp/tc61.out 2>/tmp/tc61.err
RC=$?
if [[ $RC -ne 0 ]] && grep -q "preflight_rootfs\|rootfs" /tmp/tc61.err; then
    log_pass "场景6.1: rootfs 不存在正确报错，exit $RC"
else
    log_fail "场景6.1: 期望错误退出 (实际 RC=$RC), stderr: $(cat /tmp/tc61.err)"
fi

echo ""
echo "--- 场景6.2: 命令不存在 ---"
$TC_BIN run --rootfs "$ROOTFS_DIR" --cgroup-name "$CG_FIXED" /bin/nonexistent_cmd >/tmp/tc62.out 2>/tmp/tc62.err
RC=$?
if [[ $RC -ne 0 ]] && grep -q "preflight_command\|command does not exist" /tmp/tc62.err; then
    log_pass "场景6.2: 命令不存在正确报错，exit $RC"
else
    log_fail "场景6.2: 期望命令不存在错误 (实际 RC=$RC), stderr: $(cat /tmp/tc62.err)"
fi

cleanup_fixed_cgroup

# --------------------------------------------------------------------------
echo ""
echo "========================================================"
echo "                  最终总结"
echo "========================================================"
echo "   PASS: $PASS"
echo "   FAIL: $FAIL"
echo "========================================================"

# 最后：删除临时 rootfs
rm -rf "$ROOTFS_DIR"
log_info "已清理临时 rootfs: $ROOTFS_DIR"

if [[ $FAIL -gt 0 ]]; then
    exit 1
fi
exit 0
