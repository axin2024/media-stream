# 快速开始

在 MySQL 8.4 创建空库和项目专用账号。下例以本机临时账号为例，密码应替换为随机值。

```sql
CREATE DATABASE media_stream CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin;
CREATE USER 'media'@'127.0.0.1' IDENTIFIED BY 'replace-with-random-password';
GRANT ALL PRIVILEGES ON media_stream.* TO 'media'@'127.0.0.1';
```

以 PowerShell 为例，从项目根目录执行。在其他终端启动独立进程时，设置相同的 MySQL DSN，并使用同一个媒体路径。

```powershell
go build -trimpath -o .local/media-stream.exe ./cmd/media-stream
$env:MEDIA_STREAM_MYSQL_DSN='media:replace-with-random-password@tcp(127.0.0.1:3306)/media_stream'
$env:MEDIA_STREAM_SIGNING_KEY=(& ./.local/media-stream.exe keygen)
./.local/media-stream.exe migrate
./.local/media-stream.exe node -name encoder-1 -slots 2
$owner=(& ./.local/media-stream.exe grant -tenant studio -name host -role owner | ConvertFrom-Json).secret
$viewer=(& ./.local/media-stream.exe grant -tenant studio -name guest -role viewer | ConvertFrom-Json).secret
```

生产部署时将签名密钥存入密钥管理系统。所有接入进程共享同一密钥。

## 开节目

在独立终端启动转码节点和工作台。示例的两个终端从相同目录启动。

```powershell
./.local/media-stream.exe worker -node encoder-1 -concurrency 2 -media .local/broadcast
./.local/media-stream.exe serve -listen 127.0.0.1:8080 -media .local/broadcast
```

打开 `http://127.0.0.1:8080`，用频道主凭据连接，预约一个当前时间已落入开放期的频道。进入频道后打开会话，选择 `720p + 360p` 和 4 秒分片。保留会话 ID，以及工作台返回的两路短期凭据。

生成带时码的样片。为主路准备前 45 秒，在播放中截断主路将得到相同效果。

```powershell
./.local/media-stream.exe sample -out .local/source.mp4 -seconds 120
ffmpeg -i .local/source.mp4 -t 45 -c copy .local/primary.mp4
```

准备两个推流终端，各自设置凭据。备路使用完整样片，主路播放到结尾时发送中断事件。

```powershell
$env:MEDIA_STREAM_PUSH_TOKEN='replace-with-primary-grant'
./.local/media-stream.exe push -session '<session>' -source .local/primary.mp4 -segment-ms 4000 -notify-lost
```

```powershell
$env:MEDIA_STREAM_PUSH_TOKEN='replace-with-backup-grant'
./.local/media-stream.exe push -session '<session>' -source .local/source.mp4 -segment-ms 4000
```

也可以用 FFmpeg 支持的 SRT、RTMP 输入作为 `-source`。推流器在本地切片，再经 HTTPS 或本机 HTTP 上传。集中部署时，FFmpeg 节点与接入进程需能访问同一个媒体文件系统。

## 播放、封存与剪辑

1. 进入频道并点击播放，选择自动或指定画质。使用观众凭据访问时会隐藏节目控制。
2. 主路结束后，查看输入代际、切换事件、首个备路可播片和播放器卡顿。回放时间轴由可播片组成，源间隙与原始时间戳保存在索引中。
3. 备路结束后点击 **结束并归档**。刷新目录并从回放播放，切换前后都能定位。
4. 导入 [示例字幕](../examples/rehearsal-en.vtt)，选择 20 至 40 秒，提交剪辑。作业完成后下载 MP4 和重定位的 WebVTT。
5. 用观众身份对照回放访问和剪辑下载。下架回放后，之前交付的剪辑继续按自身授权和期限访问。

在不同浏览器配置文件中可同时保留频道主和观众会话。通过自动化或播放器调试时，可对媒体请求附加 `Authorization: Bearer …`，或先经 API 设置播放 Cookie。
