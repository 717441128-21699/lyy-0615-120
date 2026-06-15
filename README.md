# TinyContainer - 简化容器运行时技术说明

## 项目结构

```
.
├── main.go                    # 程序入口，命令行参数解析
├── go.mod                     # Go 模块定义
└── container/
    ├── types.go              # 类型定义
    ├── run.go                # 容器启动主逻辑 (父进程)
    ├── init.go               # 容器内 init 进程逻辑 (子进程)
    └── cgroup.go             # cgroup 资源限制
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
```

---

## 一、各隔离步骤的系统调用详解

### 1. Namespace 隔离 (clone 系统调用)

**代码位置**: [run.go](file:///d:/trae-bz/TraeProjects/120/container/run.go#L50-L55)

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

**代码位置**: [init.go](file:///d:/trae-bz/TraeProjects/120/container/init.go#L98-L100)

**系统调用**: `sethostname()`

```go
syscall.Sethostname([]byte(hostname))
```

由于进程在新的 UTS namespace 中，修改 hostname 不会影响宿主机。

---

### 3. Mount Namespace - 挂载点隔离

**代码位置**: [init.go](file:///d:/trae-bz/TraeProjects/120/container/init.go#L102-L112)

**关键系统调用**:
- `mount("", "/", "", MS_REC|MS_PRIVATE, "")` - 将根目录挂载改为 private
- `mount(rootfs, rootfs, "", MS_BIND|MS_REC, "")` - bind 挂载 rootfs

**为什么要先 remount root 为 private？**
- 默认情况下，mount namespace 的挂载事件可能会传播到父 namespace
- 设置 `MS_PRIVATE` 确保挂载事件不会传播
- 这是 pivot_root 正常工作的前提

---

### 4. 根文件系统切换 (pivot_root)

**代码位置**: [init.go](file:///d:/trae-bz/TraeProjects/120/container/init.go#L114-L134)

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

**代码位置**: [init.go](file:///d:/trae-bz/TraeProjects/120/container/init.go#L136-L143)

**系统调用**: `mount()`

```go
syscall.Mount("proc", "/proc", "proc", 0, "")
```

在新的 PID namespace 中，必须重新挂载 proc 文件系统才能看到正确的进程列表。否则 `/proc` 仍然显示宿主机的进程。

---

### 6. Cgroup 资源限制

**代码位置**: [cgroup.go](file:///d:/trae-bz/TraeProjects/120/container/cgroup.go)

**原理**: cgroup v1 通过虚拟文件系统 (`/sys/fs/cgroup/`) 进行管理

**CPU 限制**:
- `cpu.cfs_period_us` - CFS 调度周期（默认 100ms）
- `cpu.cfs_quota_us` - 周期内可用 CPU 时间（微秒）
  - `quota = 100000` 且 `period = 100000` 表示限制为 1 个 CPU 核心
  - `quota = 50000` 且 `period = 100000` 表示限制为 0.5 个 CPU 核心

**内存限制**:
- `memory.limit_in_bytes` - 内存使用上限（字节）

**将进程加入 cgroup**:
- 写入 `cgroup.procs` 文件将进程 PID 加入 cgroup

---

## 二、为什么 chroot 不足以构成安全隔离

### 1. chroot 只是路径前缀替换

`chroot()` 系统调用仅修改进程的根目录 `/` 的指向，它不提供真正的隔离：

- **可以逃出 chroot**: 通过 `chroot("..")` 等手段，如果进程有合适的权限，可以逃出 chroot 环境
- **进程可见**: 进程仍然可以看到宿主机上的所有其他进程（没有 PID namespace）
- **网络可见**: 可以访问宿主机的所有网络接口和套接字
- **挂载可见**: 可以看到宿主机的所有挂载点

### 2. 缺少其他 namespace 隔离

chroot 只涉及文件系统路径，不隔离：
- PID namespace - 进程 ID 隔离
- Network namespace - 网络栈隔离
- UTS namespace - 主机名隔离
- IPC namespace - 进程间通信隔离
- User namespace - 用户/组 ID 隔离

### 3. 缺少资源限制

chroot 不提供任何资源限制机制，容器内进程可以无限制地使用 CPU、内存等资源。

### 4. pivot_root + mount namespace 才是正确方式

`pivot_root` 系统调用配合 mount namespace：
- 完全替换根文件系统
- 旧的根文件系统可以被彻底卸载
- 配合其他 namespace 才能构成完整的隔离环境

---

## 三、PID Namespace 中 1 号进程的特殊职责

### 1. 孤儿进程回收

在 Linux 中，当一个父进程先于子进程退出时，子进程会变成**孤儿进程**。内核会将孤儿进程的父进程设置为 PID 1（init 进程）。

**PID 1 的特殊职责**:
- 必须调用 `wait()` / `waitpid()` 来回收所有孤儿进程
- 如果 PID 1 不回收，孤儿进程会变成**僵尸进程**，占用系统资源

### 2. 信号处理特殊

- PID 1 不会被默认信号处理杀死（如 SIGKILL 也不能直接杀死 PID 1）
- PID 1 必须显式注册信号处理函数才能响应信号

### 3. 本实现的处理方式

**代码位置**: [init.go](file:///d:/trae-bz/TraeProjects/120/container/init.go#L38-L85)

本实现中，容器内的 PID 1 进程承担 init 职责：

1. **启动用户命令**: 使用 `os.StartProcess()` fork 出用户指定的命令作为子进程
2. **循环等待子进程**: 使用 `syscall.Wait4(-1, &status, 0, nil)` 循环等待所有子进程
   - `pid = -1` 表示等待任意子进程
3. **回收孤儿进程**: 当用户命令创建的子进程成为孤儿时，PID 1 负责回收它们
4. **转发信号**: 接收到的信号转发给用户命令进程
5. **正确退出**: 当主进程退出且所有子进程都回收完毕后，以主进程的退出码退出

```go
for {
    var status syscall.WaitStatus
    pid, err := syscall.Wait4(-1, &status, 0, nil)
    if err != nil {
        if err == syscall.ECHILD {
            // 没有更多子进程，退出
            os.Exit(firstExitStatus)
        }
        continue
    }
    // 记录主进程的退出状态
    if pid == proc.Pid {
        firstExited = true
        // ... 设置退出码
    }
}
```

---

## 四、容器退出后资源清理机制

### 1. Namespace 资源 - 内核自动清理

Namespace 是内核级对象，当 namespace 中的最后一个进程退出后，内核会自动释放该 namespace 的资源。

**自动清理的 namespace**:
- PID namespace
- Mount namespace
- Network namespace
- UTS namespace
- IPC namespace
- User namespace

**无需手动清理**，进程退出即释放。

### 2. Cgroup 资源 - 需手动清理

Cgroup 是持久化的，不会因为进程退出而自动删除。必须显式删除 cgroup 目录。

**本实现的清理机制**:

#### (1) defer 清理
**代码位置**: [run.go](file:///d:/trae-bz/TraeProjects/120/container/run.go#L31-L43)

```go
cleanupDone := make(chan struct{})
cleanup := func() {
    select {
    case <-cleanupDone:
        return
    default:
    }
    close(cleanupDone)
    if err := cgroup.Destroy(); err != nil {
        fmt.Fprintf(os.Stderr, "Warning: failed to destroy cgroup: %v\n", err)
    }
}
defer cleanup()
```

函数返回时自动清理 cgroup。

#### (2) 信号处理
**代码位置**: [run.go](file:///d:/trae-bz/TraeProjects/120/container/run.go#L69-L84)

捕获 SIGINT、SIGTERM、SIGQUIT、SIGHUP 等信号，转发给容器进程后，通过 defer 确保清理。

#### (3) 防止重复清理
使用 `cleanupDone` channel 保证清理函数只执行一次。

### 3. 挂载点清理

- 容器内的挂载点在 mount namespace 中
- 进程退出后，mount namespace 销毁，挂载自动消失
- 无需手动清理

### 4. 极端情况：父进程被 SIGKILL

如果父进程被 `SIGKILL`（信号 9）强制杀死，defer 和信号处理都不会执行，此时 cgroup 会残留。

**解决思路**:
- 使用 `PID file` + 启动时检测清理
- 使用 cgroup v2 的 "release_agent" 机制
- 使用更高层级的管理进程（如 containerd、systemd）来监控和清理

生产环境中通常由容器运行时（runc）配合容器管理组件（containerd）来确保资源不泄漏。

---

## 五、整体执行流程图

```
父进程 (宿主机)
  │
  ├─ 1. 创建 cgroup 目录结构
  ├─ 2. 设置 CPU 和内存限制
  │
  ├─ 3. clone() 系统调用 ──── 同时创建 4 个新 namespace
  │     (CLONE_NEWPID |
  │      CLONE_NEWNS |
  │      CLONE_NEWNET |
  │      CLONE_NEWUTS)
  │
  │                    子进程 (新 namespace 中，PID 1)
  │                      │
  │                      ├─ 4. sethostname() - 设置主机名
  │                      ├─ 5. mount() - 重新挂载 root 为 private
  │                      ├─ 6. mount() - bind 挂载 rootfs
  │                      ├─ 7. pivot_root() - 切换根文件系统
  │                      ├─ 8. mount() - 挂载 proc 文件系统
  │                      │
  │                      ├─ 9. fork() 启动用户命令
  │                      │
  │                      ├─ 10. 循环 Wait4() 回收所有子进程
  │                      │     (充当 init 进程)
  │                      │
  │                      └─ 11. 用户命令退出，所有子进程回收完
  │                           PID 1 退出
  │
  ├─ 4. 将子进程 PID 写入 cgroup.procs
  ├─ 5. wait() 等待子进程退出
  └─ 6. 删除 cgroup 目录 (defer 清理)
```

---

## 六、局限性与改进方向

1. **网络 namespace 为空**: 新网络 namespace 中只有 loopback，没有配置网络设备
2. **缺少 user namespace**: 容器内 root 即宿主机 root，有安全风险
3. **缺少 IPC namespace**: System V IPC 和 POSIX 消息队列未隔离
4. **cgroup v1**: 现代系统已转向 cgroup v2
5. **缺少 overlayfs**: 没有使用写时复制的文件系统层
6. **没有 seccomp**: 缺少系统调用过滤
7. **没有 capability 限制**: 进程拥有所有 root 权限

生产级容器运行时（如 runc）会实现所有这些功能。
