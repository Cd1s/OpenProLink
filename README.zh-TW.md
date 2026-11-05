<p align="center"><img src="./docs/assets/hero-banner.svg" alt="OpenCDJ-3000" width="100%"></p>

<p align="center"><a href="./README.md">English</a> · <a href="./README.zh-CN.md">简体中文</a> · <b>繁體中文</b></p>

**OpenCDJ-3000** 是給 Pro DJ Link 測試台用的軟體播放機：它以虛擬 CDJ-3000 的身分加入網路，把一個 rekordbox 隨身碟提供給受測裝置；你在網頁上看它、控制它：播放機送了什麼、對方解析出了什麼、中間每一個封包。測試台上不需要實機。

> **狀態：早期。** 封包格式依據 Deep Symmetry 的協定分析（`dysentery`、`beat-link`、`crate-digger`），不是我們自己擷取的實機封包。測試通過只說明受測裝置與本模擬器彼此一致，相容性要以實機為準。

## 功能

| 面板 | 內容 |
| --- | --- |
| **虛擬 CDJ-3000** | 機號 1–4、播放 / 暫停 / CUE、調速 ±6 / ±10 / ±16 % / WIDE、拍號計數、捲動三段波形與總覽、隨身碟曲目表、瀏覽器內播放 |
| **受測裝置** | 程序內執行 [EngineX OS](https://github.com/Cd1s/EngineX-OS) 的 Pro DJ Link 接收端：它辨識出的播放機、BPM、pitch、拍號、播放狀態，以及它經 NFS 取得的波形 |
| **CDJ → Denon 匯入** | 經 NFSv2 複製虛擬隨身碟並執行 EngineX 的 rb-bridge，列出產生的 Engine 歌單 |
| **[LINK] Denon → CDJ** | 用 dbserver 用戶端（TCP 12523）瀏覽 EngineX 匯出、經 NFSv2 載入曲目、顯示波形並播放；示範機號衝突時匯出端自動讓位 |
| **封包面板** | UDP 50000 keepalive、50001 beat、50002 status、dbserver 請求與 NFS 讀取，附每個封包開頭的十六進位 |

預設隨身碟是產生的：32 小節 120 BPM 鼓組，附對應的 beatgrid、PWV5 彩色波形與 PWV7 三段波形。用 `-usb` 指定真實的 rekordbox 匯出。

## 快速開始

在 Linux 主機上編譯執行（使用迴路位址 127.0.0.1、127.0.0.2 與 UDP 50000–50002）：

```sh
go build -trimpath -o bin/cdj3000-sim ./cmd/cdj3000-sim
./bin/cdj3000-sim -http 127.0.0.1:18300                # 產生的隨身碟
./bin/cdj3000-sim -usb /path/to/USB -http 127.0.0.1:18300
```

網頁只監聽迴路位址。在你的電腦上：

```sh
ssh -L 18300:127.0.0.1:18300 <編譯機>
open http://127.0.0.1:18300
```

| 參數 | 預設值 | 含義 |
| --- | --- | --- |
| `-http` | `127.0.0.1:18300` | 網頁位址，保持在迴路 |
| `-usb` | 產生 | 含 `PIONEER/rekordbox/export.pdb` 的 rekordbox 隨身碟根目錄 |
| `-sim-ip` | `127.0.0.2` | 虛擬 CDJ 的送包位址與 NFS 位址 |
| `-denon-ip` | `127.0.0.1` | 受測接收端的監聽位址 |
| `-denon-empty` | 關 | Denon 碟上沒有自己的 rekordbox 匯出 |

## 儲存庫結構

| 路徑 | 內容 |
| --- | --- |
| `cmd/cdj3000-sim/` | 模擬器：送包、dbserver 用戶端、產生的隨身碟、網頁 |
| `third_party/enginex/` | 受測的 EngineX OS 程式碼唯讀副本，用 `scripts/sync-enginex.sh` 更新，不要直接改 |
| `docs/ARCHITECTURE.md` | 元件、資料流、測試策略與路線圖 |
| `docs/PROLINK-WIRE-SPEC.md` | 模擬器收送的每個封包，逐位元組說明 |

## 商標

Pioneer DJ、CDJ、rekordbox、Pro DJ Link 是 AlphaTheta Corporation 的商標；Denon DJ、Engine DJ 是 inMusic Brands 的商標。本專案與上述公司無關，也未獲其認可。型號名稱僅用於說明所模擬的硬體。

## 授權

MIT。`third_party/enginex` 是 EngineX OS 的程式碼，沿用其 MIT 授權。
