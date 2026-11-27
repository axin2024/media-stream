# 制作导播与节目播出

制作节目关联一个等待输入的频道会话。导播台读取各路机位、节目时码、已确认串联单与实际主输出。回放仍按原频道和租户权限读取。

## 启动与试播

1. 按[快速开始](quickstart.md)配置 MySQL 和签名密钥，执行 `media-stream migrate`。准备 FFmpeg、FFprobe 和一份制作人员有权使用的 TTF 或 OTF 字体。
2. 用 `media-stream serve -ffmpeg ffmpeg -ffprobe ffprobe -overlay-font studio.ttf` 启动。已有分布式部署可在接入进程禁用 `-render-productions`，再用 `media-stream renderer -media ./media-data -ffmpeg ffmpeg -ffprobe ffprobe -overlay-font studio.ttf` 运行独立制作编码器。所有进程须共享 MySQL、媒体目录和同一份字体。
3. 制作人用 `owner` 凭据打开直播工作台，在可用时间内创建频道和等待输入的会话。切到「制作导播」，复制会话编号，填写时区和备用槽位 `CONT`，创建制作节目。
4. 技术员建立 `CONT` 垫片、`INTRO` 片段和 `CAM-A` 到 `CAM-D` 直播槽位。给机位提交三到九组同一瞬间采集的源时码和节目时码，并检查读回的校准值。节目时间 1000 ms 对应源时间 1200 ms 时，稳定零点差为 200 ms，工作台读回 −200 ms，首片可从源帧 5 对齐节目帧 0。
5. 在串联单添加片头与机位，填入节目播入、播出、源时码。固定广告保留预定播出窗口，可移动直播和片段通过拖动或上移下移重新录入。提交后读回版本，再用「延长直播」检查后续节目与固定广告的冲突。
6. 导播领取控制，确认预监后开播。选择机位的「设为预监」会更新预监标签。TAKE 入队后可读回编号，实际编码发布后显示节目帧和 PTS。主输出和四个预监独立播放，试听按钮切换监看声道。
7. 包装员在图文包装预览字形和边界，再安排上屏。字幕条用 125 帧表示 5 秒。清屏从下一段尚未认领的输出开始，播出日志保留原定和实际撤下时刻。
8. 封存节目后读取实际播出单，按播出切点填入核验范围。完整区间返回可播放的 HLS、实际边界和交付摘要。缺片结果列出时码、时长和原因。实际播出单和归档清单均可下载 JSON。

### 分片推送

用 `media-stream grant -tenant <租户> -channel <频道> -name input-tech -role operator` 生成按频道授权的短期技术员凭据，在推送进程中设置 `MEDIA_STREAM_PRODUCTION_TOKEN`。以下示例用同一份自制影音依次演示预载片段和文件模拟直播。文件推送默认按媒体进度发送，RTMP、SRT 及 RIST 输入由本地 FFmpeg 读取，支持协议以本机编译选项为准。

```text
media-stream sample -out opening.mp4 -seconds 30
media-stream push -origin http://127.0.0.1:8080 -production <制作编号> -slot INTRO -source opening.mp4 -segment-ms 5000 -clock-start-ms 0 -fast
media-stream push -origin http://127.0.0.1:8080 -production <制作编号> -slot CAM-A -source camera-a.mp4 -segment-ms 5000 -clock-start-ms 30200 -notify-lost
```

可提前启动各推送进程并用一致的节目钟安排机位，也可在源时间从节目 30 秒开始时接入。上例假定机位的源时钟比节目晚 200 ms。对于已完成分片的文件，始终用对应源首帧调用 HTTP API。现有 `push -session` 继续只用于常规主备会话。

输入为独立 H.264 MPEG-TS，每片从关键帧起、最多 250 视频帧。对非静音槽位附带 AAC；输出会重采样到 48 kHz。源视频帧间隔固定为 3600 个 90 kHz 刻度。目录的备用槽位可上传自制垫片。缺少对应文件时，使用程序内置的静音深色网格。

## 接口

管理请求使用 `Authorization: Bearer <token>`。写入 JSON 设 `Content-Type: application/json`。下表短路径以 `/api/productions/{id}` 为起点，列表和创建除外。

| 方法和路径 | 请求与实际回执 |
| --- | --- |
| `GET /api/productions?after=…`、`GET /` | 按租户和频道过滤列表，详情给出控制代际、当前/预监/备用槽位、时码和归档版本 |
| `POST /api/productions` | `session_id, title, timezone, fallback_slot`，需幂等键 |
| `GET /slots`、`PUT /slots/{slot}` | 列出时钟、声道、最后片段；待播时设置 `title, kind, audio_channel` |
| `PUT /slots/{slot}/clock` | `samples` 中提供 `source_time_ms` 与 `program_time_ms`，读回 `alignment_ms` |
| `PUT /slots/{slot}/input/{source_frame}` | 请求体为 MPEG-TS，`X-Frame-Count` 为实际视频帧数；回执给出对齐帧和 `replay` |
| `POST /slots/{slot}/lost`、`GET /slots/{slot}/monitor.mp4` | 写入失联记录；读取独立三秒预监 |
| `GET/PUT /rundown` | 读回版本；`expected_revision` 加 `items`，或加 `extend_item, extension_frames`，冲突给出 `conflict_items` |
| `POST /control` | `expected_control_epoch, lease_seconds`，3–120 秒，返回当前代际、控制者和到期时间 |
| `POST /preview`、`POST /start`、`POST /end` | 都带当前代际，预监还带 `slot_id` |
| `POST /takes`、`GET /takes/{take}` | `slot_id, expected_control_epoch, transition, command_id` 和幂等键，溶解加 `transition_frames`；实际发布后返回 `actual_frame, actual_pts` |
| `POST /overlays/preview.png`、`POST /overlays` | `template, text, font_size, duration_frames`，可选 `on_frame`；预览 PNG 或图层回执 |
| `GET /overlays?after=…`、`POST /overlays/clear` | 读回已批准的字体摘要及撤下时刻；清屏给出下一可修改的帧 |
| `GET /as-run?from_frame=…` | 翻页 `next_frame`，逐段记录节目单修订、切换编号、输出序号、音画时间戳和媒体 SHA-256 |
| `GET /archive?from_frame=…&to_frame=…&track=720p` | 最多选择 10 分钟，含输入帧范围、实际分片边界、摘要与缺片；可加 `format=playlist` |
| `GET /media/archive/{id}/index.m3u8` | 同样提供范围和画质，可用 HttpOnly 播放 Cookie |

授权角色按现有频道权限控制。`owner`、`operator` 配置输入，`owner`、`director` 确认串联单；`owner`、`director`、`operator` 可领取租约，切换写入同时校验凭据与代际；`owner`、`director`、`editor` 安排和撤销图层。`viewer` 可以读取获授权节目和回看，浏览器的媒体 Cookie 默认仅用于 `/media/`。

重复提交相同 TAKE 返回同一编号。代际已过期或控制者变更返回 409，并附当前代际；无权限读取的节目返回 404；业务输入错误返回 422 和字段路径。处理中的 TAKE 以 202 与 `Location` 返回，终态为 `published`、`fenced` 或 `failed`。`actual_pts` 使用从节目时间 0 起的 90 kHz 刻度，MPEG-TS 封装额外保留 90000 刻度起始余量。

## 按节目时钟恢复

节目每段在 MySQL 持久化源选择、已批准图层和当前发布认领。渲染成功后，同一事务写入两档媒体索引、实际播出单与 TAKE 回执。不同编码进程共享这套认领，正在使用的凭据到期后由当班导播重新领取代际。遇到源丢失时，垫片占用缺失区间；恢复输入应从当前节目时钟对应的源帧继续，落在已发布区间的片段只供定位和监看。

制作输出按分片提交，预监为最近可读分片生成的独立短片。观众通过 HLS 缓冲观看。节目 TAKE 的确认是实际编码结果，预监试听存在独立处理延迟。当前文字台标使用已授权字体叠加。归档精度为节目分片边界，选择落在片内的区间时返回范围覆盖所需的完整分片并明确实际边界。对逐帧截取可从已有录制入口建立剪辑。

## 复现验收

Windows 需要 PowerShell 7、MySQL 8.4 和 FFmpeg/FFprobe。测试脚本为每次执行启动独立回环数据库，运行后清除对应的临时实例。

```powershell
$env:MEDIA_STREAM_TEST_PRODUCTION_REPORT = (Join-Path $PWD '.local/production.json')
.\scripts\test-mysql.ps1 -MySQLBin C:\mysql\bin -FFmpeg C:\ffmpeg\bin\ffmpeg.exe -Scenario Production

$env:MEDIA_STREAM_TEST_PRODUCTION_LOAD_REPORT = (Join-Path $PWD '.local/production-load.json')
.\scripts\test-mysql.ps1 -MySQLBin C:\mysql\bin -FFmpeg C:\ffmpeg\bin\ffmpeg.exe -Scenario ProductionLoad
```

可为第一条命令同时设置 `MEDIA_STREAM_TEST_PRODUCTION_BROWSER`（`scripts/browser-production.cjs` 的绝对路径）、`MEDIA_STREAM_TEST_CHROME` 和 `MEDIA_STREAM_TEST_PLAYWRIGHT`，从浏览器核对四路预监、主输出和归档。Linux CI 使用 MySQL 测试库及系统 FFmpeg 执行同一套 Go 集成测试。完整容量测试使用真实墙上时间，短时排查可通过 `MEDIA_STREAM_TEST_PRODUCTION_LOAD_SECONDS` 显式缩短。
