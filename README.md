# TinyContainer - 简化容器运行时技术说明

## 项目结构

```
.
├── main.go                    # 程序入口，命令行参数解析
├── go.mod                     # Go 模块定义
├── container/
│   ├── types.go              # 类型定义
│   ├── run.go                # 容器启动主逻辑 (父进程)
│   ├── init.go               # 容器内 init 进程逻辑 (子进程)
│   ├── cgroup.go             # cgroup 管理器基类 + v1/v2 自动检测
│   ├── cgroup_v1.go          # cgroup v1 具体实现
│   └── cgroup_v2.go          # cgroup v2 具体实现
├── stresstest/
│   └── main.go               # CPU/内存压力测试程序
└── scripts/
    └── run_tests.sh          # 完整验证测试脚本
```

## 编译与使用

```bash
go build -o tinycontainer .

# 准备 rootfs (例如使用 busybox)
mkdir -p rootfs/bin
cp /bin/busybox rootfs/bin/
cd rootfs/bin && ln -s busybox sh && ln -s busybox ls && ln -s busybox ps

# 运行容器
sudo ./tinycontainer run --rootfs /path/to/rootfs --hostname mycontainer /bin/sh

# 带资源限制
sudo ./tinycontainer run --rootfs /path/to/rootfs \
    --cpu-quota 50000 --cpu-period 100000 \
    --memory 128m \
    --cgroup-name my-container \
    /bin/sh
```

---

## 一、各隔离步骤的系统调用详解

### 1. Namespace 隔离 (clone 系统调用)

**代码位置**: [run.go](file:///d:/trae-bz/TraeProjects/120/container/run.go#L84-L89)

使用 `clone()` 系统调用创建新进程时，通过传入以下 flag 同时创建多个新的 namespace：

| Flag | 系统调用 | 作用 |
|------|----------|------|
| `CLONE_NEWPID` | clone | 创建新的 PID namespace，容器内 PID 从 1 开始 |
| `CLONE_NEWNS` | clone | 创建新的 Mount namespace，隔离挂载点视图 |
| `CLONE_NEWNET` | clone | 创建新的 Network namespace，隔离网络栈 |
| `CLONE_NEWUTS` | clone | 创建新的 UTS namespace，隔离 hostname 和 NIS domain |

**为什么用 clone 而不是 unshare？**
- `clone` 在创建新进程的同时创建新 namespace
- `unshare` 是让当前进程离开现有 namespace
- 我们需要子进程在新 namespace 中运行，所以用 clone 更直接

---

### 2. UTS Namespace - 主机名隔离

**代码位置**: [init.go](file:///d:/trae-bz/TraeProjects/120/container/init.go#L163-L165)

**系统调用**: `sethostname()`

```go
syscall.Sethostname([]byte(hostname))
```

由于进程在新的 UTS namespace 中，修改 hostname 不会影响宿主机。

---

### 3. Mount Namespace - 挂载点隔离

**代码位置**: [init.go](file:///d:/trae-bz/TraeProjects/120/container/init.go#L167-L179)

**关键系统调用**:
- `mount("", "/", "", MS_REC|MS_PRIVATE, "")` - 将根目录挂载改为 private
- `mount(rootfs, rootfs, "", MS_BIND|MS_REC, "")` - bind 挂载 rootfs

**为什么要先 remount root 为 private？**
- 默认情况下，mount namespace 的挂载事件可能会传播到父 namespace
- 设置 `MS_PRIVATE` 确保挂载事件不会传播
- 这是 pivot_root 正常工作的前提

---

### 4. 根文件系统切换 (pivot_root)

**代码位置**: [init.go](file:///d:/trae-bz/TraeProjects/120/container/init.go#L181-L201)

**系统调用**: `pivot_root()` + `umount()`

```
pivot_root(new_root, put_old)
```

**工作原理**:
1. 在新 rootfs 内创建 `.pivot_root` 目录作为旧根文件系统的挂载点
2. 调用 `pivot_root` 将当前根文件系统切换到新的 rootfs
3. 卸载旧的根文件系统（使用 `MNT_DETACH` 延迟卸载）
4. 删除旧的挂载点目录

**为什么用 pivot_root 而不是 chroot？**
- `pivot_root` 完全替换根文件系统，旧根可以被卸载
- `chroot` 只是改变进程的根目录视图，旧根仍然可以通过各种方式访问
- pivot_root 配合 mount namespace 才能实现真正的文件系统隔离

---

### 5. 挂载 proc 文件系统

**代码位置**: [init.go](file:///d:/trae-bz/TraeProjects/120/container/init.go#L203-L215)

**系统调用**: `mount()`

```go
syscall.Mount("proc", "/proc", "proc", 0, "")
```

在新的 PID namespace 中，必须重新挂载 proc 文件系统才能看到正确的进程列表。否则 `/proc` 仍然显示宿主机的进程。

---

### 6. Cgroup 资源限制

**代码位置**: [cgroup.go](file:///d:/trae-bz/TraeProjects/120/container/cgroup.go)、[cgroup_v1.go](file:///d:/trae-bz/TraeProjects/120/container/cgroup_v1.go)、[cgroup_v2.go](file:///d:/trae-bz/TraeProjects/120/container/cgroup_v2.go)

**自动版本检测**: 通过读取 `/proc/mounts` 和检查 `/sys/fs/cgroup/cgroup.controllers` 文件自动判断 cgroup v1 或 v2。

**cgroup v1**:
- `cpu.cfs_period_us` - CFS 调度周期（默认 100ms）
- `cpu.cfs_quota_us` - 周期内可用 CPU 时间
- `memory.limit_in_bytes` - 内存使用上限

**cgroup v2**:
- `cpu.max` - CPU 限制（格式：`quota period` 或 `max period`）
- `memory.max` - 内存使用上限

---

## 二、核心改进说明

### 1. cgroup v1/v2 双版本支持

**代码位置**: [cgroup.go](file:///d:/trae-bz/TraeProjects/120/container/cgroup.go#L46-L73)

```go
func DetectCgroupVersion() (CgroupVersion, error) {
    // 1. 检查 /proc/mounts 中是否有 cgroup2 挂载
    // 2. 检查 /sys/fs/cgroup/cgroup.controllers (v2 特有)
    // 3. 检查 /sys/fs/cgroup/cpu/cgroup.procs (v1 特有)
}
```

**接口设计**: 使用 `CgroupManager` 接口抽象，v1 和 v2 分别实现：
- `Set()` - 创建并配置 cgroup 限制
- `AddProcess()` - 将进程加入 cgroup
- `Destroy()` - 删除 cgroup

**不支持时的错误处理**: 每一步设置失败时，自动回滚已创建的资源，给出明确的错误信息。

---

### 2. 进程一开始就受 cgroup 限制，子进程自动继承

**双重保障机制**:

**第一层 - 父进程加入**: [run.go](file:///d:/trae-bz/TraeProjects/120/container/run.go#L101-L106)
```go
if err := cmd.Start(); err != nil { ... }
if err := cgroup.AddProcess(cmd.Process.Pid); err != nil {
    cmd.Process.Kill()
    cmd.Wait()
    return err
}
```

**第二层 - 子进程自加入**: [init.go](file:///d:/trae-bz/TraeProjects/120/container/init.go#L24-L26)
```go
if err := selfJoinCgroup(); err != nil {
    return stageErr("init_cgroup_join", err)
}
```

**子进程继承**: Linux cgroup 机制保证，进程加入 cgroup 后，所有 fork 出的子进程会**自动继承** cgroup 成员关系，无需额外操作。

**验证方法**: 使用压力测试程序创建多个子进程，观察所有进程都受 CPU/内存限制。

---

### 3. 退出码一致性

| 场景 | 容器内退出码 | 宿主机退出码 | 说明 |
|------|------------|------------|------|
| 正常退出 (exit 0) | 0 | 0 | `/bin/true` |
| 非零退出 (exit 1) | 1 | 1 | `/bin/false` |
| 被 SIGTERM 杀死 | - | 143 | `128 + 15` |
| 被 SIGKILL 杀死 | - | 137 | `128 + 9` (OOM kill) |
| 被 SIGINT 杀死 | - | 130 | `128 + 2` |

**代码位置**: [run.go](file:///d:/trae-bz/TraeProjects/120/container/run.go#L126-L149)

```go
exitCode := 0
if err := cmd.Wait(); err != nil {
    if exitErr, ok := err.(*exec.ExitError); ok {
        status := exitErr.Sys().(syscall.WaitStatus)
        if status.Exited() {
            exitCode = status.ExitStatus()
        } else if status.Signaled() {
            exitCode = 128 + int(status.Signal())
        }
    }
}
os.Exit(exitCode)
```

---

### 4. 分阶段错误处理与资源回滚

**阶段划分**:

| 阶段 | 检查点 | 失败时回滚 |
|------|--------|----------|
| `preflight_rootfs` | rootfs 路径存在且可访问 | 无需回滚 |
| `preflight_command` | 命令在 rootfs 中存在 | 无需回滚 |
| `cgroup_version_detect` | cgroup 可检测 | 无需回滚 |
| `cgroup_setup` | cgroup 创建并配置 | 自动删除已创建的 cgroup 目录 |
| `process_start` | clone 成功 | 无需回滚 |
| `cgroup_add_process` | 进程加入 cgroup | 杀死进程 + 删除 cgroup |
| `init_cgroup_join` | init 自加入 cgroup | 退出进程 |
| `init_hostname` | sethostname 成功 | 清理挂载 |
| `init_mount` | root remount private + bind mount | `cleanupMounts()` |
| `init_pivot_root` | pivot_root 成功 | `cleanupMounts()` |
| `init_mount_proc` | /proc 挂载成功 | `cleanupMounts()` |
| `init_run_command` | 用户命令启动成功 | 退出 init |

**代码位置**: [run.go](file:///d:/trae-bz/TraeProjects/120/container/run.go#L44-L50)

```go
var cleanupFuncs []func()
cleanup := func() {
    for i := len(cleanupFuncs) - 1; i >= 0; i-- {
        cleanupFuncs[i]()
    }
}
defer cleanup()

// 成功后加入清理函数
cleanupFuncs = append(cleanupFuncs, func() {
    cgroup.Destroy()
})
```

**挂载点清理**: [init.go](file:///d:/trae-bz/TraeProjects/120/container/init.go#L217-L226)
```go
func cleanupMounts() {
    for i := len(mountedPoints) - 1; i >= 0; i-- {
        mp := mountedPoints[i]
        syscall.Unmount(mp.path, syscall.MNT_DETACH)
    }
}
```

---

### 5. cgroup 清理验证

每次容器退出后，自动验证 cgroup 目录是否已清理：

**代码位置**: [run.go](file:///d:/trae-bz/TraeProjects/120/container/run.go#L177-L201)

```go
func verifyCgroupCleanup(cgroupName string, version CgroupVersion) {
    // 检查所有相关 cgroup 路径
    // 如果发现残留目录，输出 ERROR 日志
}
```

---

## 三、重点问题解答

### 1. 为什么仅仅 chroot 不足以构成安全隔离？

**chroot 只是路径前缀替换**，存在以下根本缺陷：

- **可以逃出 chroot**：通过 `chdir("..")` + `chroot(".")` 等手段，如果进程有足够权限，可以逃出 chroot 环境
- **不隔离进程视图**：能看到宿主机所有进程（缺少 PID namespace），可以发信号杀死宿主进程
- **不隔离网络**：可以访问宿主机所有网络接口、套接字
- **不隔离挂载**：可以看到宿主机所有挂载点
- **没有资源限制**：进程可以无限制使用 CPU、内存等资源
- **不隔离 IPC**：可以通过共享内存、消息队列与宿主机进程通信

**正确做法**：`pivot_root + mount namespace` 才能实现真正的文件系统隔离，再配合其他 namespace 才构成完整的容器隔离。

---

### 2. PID namespace 里 1 号进程的特殊职责（回收孤儿进程）是否处理了？

**已经处理了。**

在 Linux 中，父进程先退出的子进程会成为**孤儿进程**，内核会将其过继给 PID 1（init 进程）。如果 PID 1 不调用 `wait()` 回收，这些进程会变成**僵尸进程**，持续占用系统资源。

本实现在 [init.go](file:///d:/trae-bz/TraeProjects/120/container/init.go#L97-L151) 的 `runAsInit()` 函数中完整实现了 init 进程职责：

```go
for {
    var status syscall.WaitStatus
    pid, err := syscall.Wait4(-1, &status, 0, nil)
    if err == syscall.ECHILD {
        // 没有更多子进程了，退出
        os.Exit(firstExitStatus)
    }
    // 记录主进程的退出状态
    if pid == proc.Pid {
        firstExitStatus = status.ExitStatus()
    }
    // 其他 pid 是孤儿进程，自动被回收
}
```

同时还实现了：
- **信号转发**：将收到的信号转发给用户命令进程（忽略 SIGCHLD）
- **正确退出码**：以主进程的退出码作为容器退出码

---

### 3. 容器内进程退出后各种 namespace 和 cgroup 资源如何确保被彻底清理不泄漏？

#### Namespace 资源：内核自动清理

Namespace 是内核级对象，**当 namespace 中的最后一个进程退出时，内核自动释放该 namespace**。包括：
- PID namespace
- Mount namespace
- Network namespace
- UTS namespace

无需手动清理。

#### Cgroup 资源：多层清理机制

Cgroup 是持久化的，必须手动删除。本实现提供四重保障：

**第一层：cgroup 内部回滚** ([cgroup_v1.go](file:///d:/trae-bz/TraeProjects/120/container/cgroup_v1.go#L37-L40), [cgroup_v2.go](file:///d:/trae-bz/TraeProjects/120/container/cgroup_v2.go#L47-L50))
```go
if err := c.setupMemory(); err != nil {
    c.Destroy()  // CPU 设置成功了但内存失败，清理 CPU
    return err
}
```

**第二层：defer 兜底清理** ([run.go](file:///d:/trae-bz/TraeProjects/120/container/run.go#L44-L50))
```go
defer cleanup()  // 函数返回时自动删除 cgroup 目录
```

**第三层：信号处理** ([run.go](file:///d:/trae-bz/TraeProjects/120/container/run.go#L108-L124))
捕获 `SIGINT`、`SIGTERM`、`SIGQUIT`、`SIGHUP`，转发给容器进程，进程退出后通过 defer 清理 cgroup。

**第四层：退出后验证** ([run.go](file:///d:/trae-bz/TraeProjects/120/container/run.go#L177-L201))
```go
verifyCgroupCleanup(cgroupName, cgroup.Version())
```

#### 极端情况（SIGKILL）

如果父进程被 `SIGKILL`（信号 9）强制杀死，defer 和信号处理都来不及执行，cgroup 会残留。生产环境中的解决方案：
- 由上层管理进程（containerd、systemd）监控并清理
- 使用 cgroup v2 的 release_agent 机制
- 启动时检测并清理残留的 cgroup

---

## 四、验证测试

### 运行完整测试套件

```bash
bash scripts/run_tests.sh
```

### 手动验证示例

**1. 正常退出码**
```bash
sudo ./tinycontainer run --rootfs ./rootfs --cgroup-name test1 /bin/true
echo $?  # 应该输出 0
ls /sys/fs/cgroup/cpu/test1  # 应该不存在
```

**2. 非零退出码**
```bash
sudo ./tinycontainer run --rootfs ./rootfs --cgroup-name test2 /bin/false
echo $?  # 应该输出 1
```

**3. 信号杀死**
```bash
sudo timeout --signal=TERM 3 ./tinycontainer run --rootfs ./rootfs --cgroup-name test3 /bin/sleep 10
echo $?  # 应该输出 143 (128 + 15)
```

**4. CPU 限制测试**
```bash
# 限制为 0.5 核，启动 2 个 CPU 密集线程
# 观察 top 中 CPU 使用率应该在 50% 左右
sudo timeout 10 ./tinycontainer run --rootfs ./rootfs \
    --cpu-quota 50000 --cpu-period 100000 --memory 256m --cgroup-name test4 \
    /bin/stresstest cpu 2
```

**5. 内存限制测试**
```bash
# 限制 128MB，尝试申请 256MB
# 应该被 OOM killer 杀死，退出码 137
sudo ./tinycontainer run --rootfs ./rootfs --memory 128m --cgroup-name test5 \
    /bin/stresstest mem 256
echo $?  # 应该输出 137
```

**6. 前置检查失败**
```bash
# rootfs 不存在
sudo ./tinycontainer run --rootfs /nonexistent /bin/sh
# 错误信息: stage [preflight_rootfs] failed: rootfs path does not exist

# 命令不存在
sudo ./tinycontainer run --rootfs ./rootfs /bin/nonexistent
# 错误信息: stage [preflight_command] failed: command does not exist in rootfs
```

---

## 五、整体执行流程图

```
父进程 (宿主机)
  │
  ├─ 1. preflightChecks()
  │   ├─ 检查 rootfs 存在
  │   └─ 检查命令存在
  │
  ├─ 2. 检测 cgroup 版本 (v1/v2)
  ├─ 3. 创建 cgroup 目录并设置限制
  │   ├─ CPU 限制
  │   └─ 内存限制
  │   (失败时自动回滚已创建的 cgroup)
  │
  ├─ 4. clone() 系统调用 ──── 同时创建 4 个新 namespace
  │     (CLONE_NEWPID | CLONE_NEWNS | CLONE_NEWNET | CLONE_NEWUTS)
  │
  │                    子进程 (新 namespace 中，PID 1)
  │                      │
  │                      ├─ 5. selfJoinCgroup() - 自加入 cgroup
  │                      ├─ 6. sethostname() - 设置主机名
  │                      ├─ 7. mount() - 重新挂载 root 为 private
  │                      ├─ 8. mount() - bind 挂载 rootfs
  │                      ├─ 9. pivot_root() - 切换根文件系统
  │                      ├─ 10. mount() - 挂载 proc 文件系统
  │                      │
  │                      ├─ 11. fork() 启动用户命令 (PID 2)
  │                      │
  │                      ├─ 12. 循环 Wait4() 回收所有子进程
  │                      │     (充当 init 进程，处理孤儿进程)
  │                      │
  │                      └─ 13. 用户命令退出，所有子进程回收完
  │                           PID 1 退出
  │
  ├─ 5. 将子进程 PID 写入 cgroup.procs (双重保险)
  ├─ 6. wait() 等待子进程退出
  ├─ 7. 记录正确的退出码 (正常/非零/信号)
  ├─ 8. defer 删除 cgroup 目录
  └─ 9. 验证 cgroup 目录已清理
```

---

## 六、局限性与改进方向

1. **网络 namespace 为空**: 新网络 namespace 中只有 loopback，没有配置网络设备
2. **缺少 user namespace**: 容器内 root 即宿主机 root，有安全风险
3. **缺少 IPC namespace**: System V IPC 和 POSIX 消息队列未隔离
4. **没有 seccomp**: 缺少系统调用过滤
5. **没有 capability 限制**: 进程拥有所有 root 权限
6. **没有 overlayfs**: 没有使用写时复制的文件系统层

生产级容器运行时（如 runc）会实现所有这些功能。
