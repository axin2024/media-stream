# 第三方软件与示例素材

| 组件 | 用途 | 许可证 |
| --- | --- | --- |
| Go 1.26 | 实现、编译和运行时 | [BSD 3-Clause](licenses/go.txt) |
| go-sql-driver/mysql 1.10.2 | MySQL 连接 | [MPL-2.0](licenses/go-sql-driver-mysql.txt) |
| golang-migrate 4.20.1 | 版本化 SQL 迁移 | [MIT](licenses/golang-migrate.txt) |
| edwards25519 1.2.0 | MySQL 驱动依赖 | [BSD 3-Clause](licenses/edwards25519.txt) |
| golang.org/x/sys 0.49.0 | 迁移库依赖 | [BSD 3-Clause](licenses/golang-sys.txt) |
| HLS.js 1.7.4 | 浏览器播放器，内置上游 npm 发布文件并移除源码映射引用 | [Apache-2.0](licenses/hls.js.txt) |

内置播放器来自 [HLS.js 上游](https://github.com/video-dev/hls.js)。内置 `hls.min.js` 的 SHA-256 为 `a56a4f0328b16adf3a0a04fcec0968051bf95b327d0fd46b3f5d4f67637e5254`。

FFmpeg 由部署者独立安装，程序以子进程调用，发行包不含其二进制。FFmpeg 编译选项影响最终许可证，以部署者使用的实际版本为准。 [FFmpeg 许可说明](https://ffmpeg.org/legal.html)

演练的色块、时码、正弦音和示例字幕来自本项目的合成流程。源码仓库内仅含生成方式与原创字幕文本。
