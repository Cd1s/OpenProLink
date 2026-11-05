<p align="center"><img src="./docs/assets/hero-banner.svg" alt="OpenCDJ-3000" width="100%"></p>

<p align="center"><a href="./README.md">English</a> · <b>简体中文</b> · <a href="./README.zh-TW.md">繁體中文</a></p>

**OpenCDJ-3000** 是给 Pro DJ Link 测试台用的软件碟机：它以虚拟 CDJ-3000 的身份加入网络，把一个 rekordbox U 盘提供给被测设备；你在网页上看它、控制它：碟机发了什么、对方解析出了什么、中间每一个包。测试台上不需要真机。

> **状态：早期。** 包格式依据 Deep Symmetry 的协议分析（`dysentery`、`beat-link`、`crate-digger`），不是我们自己抓的真机包。测试通过只说明被测设备和本模拟器彼此一致，兼容性要以真机为准。

## 功能

| 面板 | 内容 |
| --- | --- |
| **虚拟 CDJ-3000** | 机号 1–4、播放 / 暂停 / CUE、调速 ±6 / ±10 / ±16 % / WIDE、拍号计数、滚动三段波形与总览、U 盘曲目表、浏览器内播放 |
| **被测设备** | 进程内运行 [EngineX OS](https://github.com/Cd1s/EngineX-OS) 的 Pro DJ Link 接收端：它识别出的碟机、BPM、pitch、拍号、播放状态，以及它经 NFS 取到的波形 |
| **CDJ → Denon 导入** | 经 NFSv2 拷贝虚拟 U 盘并运行 EngineX 的 rb-bridge，列出生成的 Engine 歌单 |
| **[LINK] Denon → CDJ** | 用 dbserver 客户端（TCP 12523）浏览 EngineX 导出、经 NFSv2 加载曲目、显示波形并播放；演示机号冲突时导出端自动让位 |
| **抓包面板** | UDP 50000 keepalive、50001 beat、50002 status、dbserver 请求与 NFS 读取，附每个包开头的十六进制 |

默认 U 盘是生成的：32 小节 120 BPM 鼓组，带对应的 beatgrid、PWV5 彩色波形和 PWV7 三段波形。用 `-usb` 指定真实的 rekordbox 导出。

## 快速开始

在 Linux 主机上编译运行（使用回环地址 127.0.0.1、127.0.0.2 和 UDP 50000–50002）：

```sh
go build -trimpath -o bin/cdj3000-sim ./cmd/cdj3000-sim
./bin/cdj3000-sim -http 127.0.0.1:18300                # 生成的 U 盘
./bin/cdj3000-sim -usb /path/to/USB -http 127.0.0.1:18300
```

网页只监听回环地址。在你的电脑上：

```sh
ssh -L 18300:127.0.0.1:18300 <编译机>
open http://127.0.0.1:18300
```

| 参数 | 默认值 | 含义 |
| --- | --- | --- |
| `-http` | `127.0.0.1:18300` | 网页地址，保持在回环 |
| `-usb` | 生成 | 含 `PIONEER/rekordbox/export.pdb` 的 rekordbox U 盘根目录 |
| `-sim-ip` | `127.0.0.2` | 虚拟 CDJ 的发包地址和 NFS 地址 |
| `-denon-ip` | `127.0.0.1` | 被测接收端的监听地址 |
| `-denon-empty` | 关 | Denon 盘上没有自己的 rekordbox 导出 |

## 仓库结构

| 路径 | 内容 |
| --- | --- |
| `cmd/cdj3000-sim/` | 模拟器：发包、dbserver 客户端、生成的 U 盘、网页 |
| `third_party/enginex/` | 被测的 EngineX OS 代码只读副本，用 `scripts/sync-enginex.sh` 更新，不要直接改 |
| `docs/ARCHITECTURE.md` | 组件、数据流、测试策略与路线图 |
| `docs/PROLINK-WIRE-SPEC.md` | 模拟器收发的每个包，逐字节说明 |

## 商标

Pioneer DJ、CDJ、rekordbox、Pro DJ Link 是 AlphaTheta Corporation 的商标；Denon DJ、Engine DJ 是 inMusic Brands 的商标。本项目与上述公司无关，也未获其认可。型号名称仅用于说明所模拟的硬件。

## 许可证

MIT。`third_party/enginex` 是 EngineX OS 的代码，沿用其 MIT 许可证。
