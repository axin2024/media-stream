# HTTP API 与播放协议

制作节目、源校时、串联单、导播授权、图层和归档的 HTTP 路径见 [制作导播接口](production.md#接口)。

API 使用 JSON，接入片段使用 MPEG-TS，字幕文件使用 WebVTT。所有 API 路径都接受 `Authorization: Bearer <token>`，媒体路径也接受同站 HttpOnly 播放 Cookie。普通 HTTP 仅用在回环地址。

## 频道与输入

| 方法与路径 | 主要参数及响应 |
| --- | --- |
| `GET /api/me` | 当前租户、角色与频道范围 |
| `GET /api/channels?after=` | 频道列表及稳定键游标 |
| `POST /api/channels` | `key`、`title`、RFC3339 格式的 `available_from` 和 `available_until` |
| `POST /api/channels/{channel}/sessions` | `Idempotency-Key`、`preset`、`segment_ms`、两路源时钟起点、`push_ttl_seconds`。返回会话及有效的原代凭据 |
| `GET /api/sessions/{id}` | 输入代际、水位、时长、版本和状态 |
| `PUT /api/sessions/{id}/input` | MPEG-TS 流片段，`X-Source-Time-Ms`、`X-Source-Duration-Ms`。成功含 `offset_ms` 和 `replay` |
| `POST /api/sessions/{id}/input-lost` | 推流器报告所属代际中断。未主动报告时也可由调度器按时限接管 |
| `POST /api/sessions/{id}/inputs/{source}/rotate` | `Idempotency-Key`、旧 `generation`、新 `source_zero_ms`、凭据有效期 |
| `POST /api/sessions/{id}/prefer-input` | `source`、会话 `revision`。目标收到可用片段后切换 |
| `POST /api/sessions/{id}/end` | 重复结束返回同一会话；排空后生成唯一回放 |

输入偏移等于源时间减去该代际起点，源片起始偏移按会话的分片长度对齐。可接受 100 毫秒至分片长度的有效时长，输入时长允许 250 毫秒封装舍入，发布时收敛到分片长度。序列前推最多八片。小段重复发送同样的时间和内容会返回原回执。旧代际或已提交水位之前的新片会被拒绝。

## 播放、回放和剪辑

| 方法与路径 | 响应 |
| --- | --- |
| `POST /api/playback-cookie` | 浏览器媒体请求用 Cookie，有效期一小时并受底层凭据有效期约束 |
| `DELETE /api/playback-cookie` | 清除浏览器播放会话 |
| `GET /media/live/{id}/index.m3u8` | 直播 HLS 主清单 |
| `GET /media/vod/{id}/index.m3u8` | 归档回放主清单及最新字幕 |
| `GET /api/sessions/{id}/segments?from=` | 最多 100 个媒体序号的各档索引、源偏移、媒体偏移和切换间隙 |
| `GET /api/recordings?before=` | 当前角色可见回放 |
| `GET /api/recordings/{id}` | 回放版本和可见性 |
| `POST /api/recordings/{id}/visibility` | `revision`、`hide`、可选 RFC3339 `visible_until` |
| `GET /api/visibility-batches?before=` | 当前操作凭据的批次索引，每页最多 25 条，返回 `next` 游标 |
| `PUT /api/visibility-batches/{key}` | 保存 1 至 50 项选集和操作参数，返回准备或已有回执 |
| `GET /api/visibility-batches/{key}` | 查回同一批次的进度、每项回放 ID、预期版本、完成版本或拒绝原因 |
| `POST /api/visibility-batches/{key}/execute` | 执行首个待处理项起的剩余项目，重复执行时保留已保存结果 |
| `POST /api/recordings/{id}/captions/{lang}` | `Idempotency-Key` 与 WebVTT，返回字幕轨版本 |
| `GET /api/recordings/{id}/captions` | 每个语言的最新字幕版本 |
| `POST /api/recordings/{id}/edits` | `Idempotency-Key`、`recording_revision`、`from_ms`、`to_ms`、可选 `caption_id` 和 `visible_until` |
| `GET /api/recordings/{id}/edits?after=` | 该回放的剪辑及稳定键游标 |
| `GET /api/edits/{id}` | 剪辑状态、来源回放及实际切点 |
| `GET /media/clip/{id}/video.mp4` | 独立授权的 MP4，可按范围读取 |
| `GET /media/clip/{id}/subtitles.vtt` | 可选独立授权的重定位字幕 |

播放列表中的链接均为同源相对路径。直播清单提供最近八个序号，归档清单封存全部可播序号。源代际或源时间出现断点时，媒体清单使用 `EXT-X-DISCONTINUITY`，并保留 UTC 节目时间。两档按同一切片边界编码。失去整条连续档位时主清单只挂出仍覆盖整个窗口的档位；全部固定档缺一部分时，用随片切换的源档保持有效序列。发现某个序号的所有文件均不可读时，该清单返回 503。

剪辑的起止使用实际媒体时间。视频以 25 帧每秒编码，边界向后对齐至最近输出帧。回执返回请求区间和实际区间。回放中有源间隙的片段仍保留原始来源与真实媒体时间映射。

频道主或编辑可提交批次。例如，下架选集中的一份已发布回放。

```json
{
  "hide": true,
  "items": [{"recording_id": "ce5a264a153cdac4673c25cc", "expected_revision": 1}]
}
```

设置期限时用 `hide: false` 并传入 RFC3339 格式的 `visible_until`。项目顺序、版本和批次操作随请求键一起锁定，改动后使用新键。准备完成后的 `prepared` 回执不会改变回放。执行时每项的写入、事件和回执在同一事务提交，状态包含 `not_started`、`applied` 和 `rejected`。`not_found` 同时用于不存在或未授权对象，`revision_or_state_changed` 表示选集版本或状态已变化，`deadline_elapsed` 表示待处理项的截止时间在执行前已过。连接中断先用原凭据 GET 批次，再继续剩余项。

## 质量、用量和错误

- `GET /api/sessions/{id}/events?after=` 返回运维时间线与对象版本相关事件。
- `POST /api/sessions/{id}/quality` 接受 `device`、`event_key`、`kind`、`duration_ms` 与 `media_ms`。同键同内容返回 `replay:true`，同键异内容返回 409。
- `GET /api/sessions/{id}/quality` 聚合输入切换、排队、转码失败、起播、卡顿和出口数据。
- `GET /api/ops/usage?since=` 汇总租户内每小时入口、出口、请求和出口错误。
- `GET /api/ops/nodes` 返回节点容量、启用状态与最近心跳，仅租户运维身份可用。
- `GET /healthz` 为存活，`GET /readyz` 验证 MySQL 与最终迁移表。

无权输入返回 403，越租户访问和已撤销媒体返回 404，状态或代际冲突返回 409，超限文件返回 413，源时钟超前返回 429，输入池已满返回 503。429 和输入池满的 503 带 `Retry-After`。遇到网络中断，请用原有幂等键、相同内容重试。幂等请求需要先校验身份。

协议依据见 [HLS RFC 8216](https://www.rfc-editor.org/rfc/rfc8216) 与 [FFmpeg 格式文档](https://ffmpeg.org/ffmpeg-formats.html)。
