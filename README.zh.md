# Simorgh — 游戏加速隧道

[English](README.md) · [فارسی](README.fa.md) · **中文** · [Русский](README.ru.md)

Simorgh 是一个自托管的低延迟隧道，专为在伊朗服务器与境外服务器之间转发游戏
（以及一般 VPN）流量而设计——降低延迟并在丢包时恢复，同时不引入额外的往返开销。

隧道核心运行在**你自己构建**的镜像中。不会从任何镜像仓库拉取隧道镜像：`core/`
是完整的 Go 源码，`install.sh` 对它执行 `docker build`。

## 面板

`panel/` 是一个完整的管理面板——18 种协议（Xray/VMess/VLESS/Trojan、WireGuard、
OpenVPN、L2TP、PPTP、IPsec/IKEv2、SSTP、OpenConnect、MTProto、AmneziaWG、GRE、
SSH、RADIUS 等）、订阅系统，以及带流量额度账本的经销商系统。界面支持 13 种语言。

在继承自上游的功能之外，本分支新增：

| | |
|---|---|
| **多节点** | 一个面板通过 mTLS 管理其他国家的服务器。添加节点只需 IP 和 SSH 密码；依赖项自动安装，包括内核模块。见 [docs/NODES.md](docs/NODES.md)。 |
| **多地区订阅** | 一个入站部署在三个节点上，客户就得到三份配置可供选择，全部共用同一份流量额度。 |
| **每客户一份额度** | 20 GB 覆盖他持有的所有协议，而不是每个协议各 20 GB。 |
| **设备数限制** | 限制多少台设备可以获取订阅。在以此对外销售前，请先阅读 [docs/RESELLERS-AND-DEVICES.md](docs/RESELLERS-AND-DEVICES.md) 中的适用范围说明。 |
| **独立的经销商面板** | 经销商在自己的 URL 登录，管理面板的位置不再是每个销售人员都知道的信息。 |
| **面板内置隧道** | `core/` 作为一个受管核心与 Xray 并列运行，具有相同的启动/停止/日志操作。无需 Docker。 |

**要搭建伊朗 → 境外？** [docs/SETUP-IRAN-RELAY.md](docs/SETUP-IRAN-RELAY.md)
提供了分步说明，包括最容易出错的两项设置。

## 快速安装

```bash
curl -fsSL https://raw.githubusercontent.com/devprogrmer/simorgh/main/install.sh -o /tmp/simorgh-install.sh && sudo bash /tmp/simorgh-install.sh
```

先下载再执行，比直接管道给 bash 更安全。请在**两台**服务器上都运行。脚本会先询问
这台服务器的用途，然后据此完成其余配置。

## 主要特性

- **传输方式**：ICMP（默认——开销最低，最不容易被过滤）、UDP，或 `auto`
  （仅客户端——先试 ICMP 再试 UDP，锁定能连通的那一个）。均支持 IPv4/IPv6 双栈。
- **可承载任意协议**：`MODE=forward` 转发基于 UDP（WireGuard、OpenVPN-UDP、
  L2TP/IPSec）或 TCP（OpenVPN-TCP、Cisco、Xray/VLESS-TCP、Trojan）的流量。
- **单服务器多客户**：`CUSTOMERS_FILE` 使一个进程服务多个客户，各自完全隔离，
  拥有独立的加密、FEC 与可选带宽上限。
- **安全性**：X25519 握手提供前向保密，AES-256-GCM 加密会话。密码用于认证握手，
  从不直接作为加密密钥；每个会话都协商新密钥。
- **自适应 FEC**、**多服务器自动故障转移**（基于 RTT 与丢包评分，带迟滞）、
  **实时链路质量监控**、**MTU 优化器**、**DSCP 标记**、**带宽整形**。

## 环境要求

- 一台位于伊朗的 Linux VPS 和一台境外 Linux VPS，均由你自己控制，两者之间开放
  ICMP 或某个 UDP 端口。
- Docker（缺失时 `install.sh` 会自动安装）。
- 两台机器上的 root 权限。

## 测试状态——盲目信任之前请先阅读

这一节刻意写得直白，因为一份不说明哪些未经测试的文档比没有文档更糟。

- **隧道核心**（两种传输、两种模式、故障转移、TCP 与 UDP 中继、多客户隔离）：
  已在基于网络命名空间的真实测试中端到端验证。IPv6 经过代码审查但**未做运行时
  测试**——请在你自己的服务器上验证。
- **面板**：可以编译，测试套件全绿。但**多节点未在真实服务器上运行过**——SSH
  引导、远程依赖安装以及隧道的真实负载均仅有单元测试覆盖。
- **共享额度**：同一个 email 出现在两个 **Xray 原生**入站上的情况，未在真实
  Xray 上验证。其他协议不受影响。

## 文档

| | |
|---|---|
| [SETUP-IRAN-RELAY.md](docs/SETUP-IRAN-RELAY.md) | 伊朗 → 境外，分步搭建 |
| [NODES.md](docs/NODES.md) | 添加与管理节点 |
| [RESELLERS-AND-DEVICES.md](docs/RESELLERS-AND-DEVICES.md) | 经销商、共享额度、设备限制 |
| [PROTOCOL.md](docs/PROTOCOL.md) | 线路格式、握手、加密、FEC |
| [CONFIGURATION.md](docs/CONFIGURATION.md) | 核心读取的所有环境变量 |
| [DEPLOYMENT.md](docs/DEPLOYMENT.md) | 服务器优化与故障排查表 |

## 许可证

`core/`、`nodepanel/`、`protocols/` 和 `install.sh` 采用 **MIT** 许可——见
`LICENSE`。`panel/` 是一个独立分支，采用 **GPLv3**——见 `panel/LICENSE`。
不要假设 MIT 条款适用于该目录；并不适用。
